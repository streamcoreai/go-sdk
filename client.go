package streamcoreai

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// Client manages a WebRTC connection to a Voice Agent server via WHIP signaling.
// It handles peer connection setup, data channel event handling, and provides
// access to the local/remote audio tracks for custom audio I/O.
type Client struct {
	config Config
	events EventHandler
	ctx    context.Context
	cancel context.CancelFunc

	pc         *webrtc.PeerConnection
	sessionURL string

	// LocalTrack is the outbound audio track you write RTP packets to.
	// It is created during Connect() and available afterwards.
	LocalTrack *webrtc.TrackLocalStaticRTP

	// RemoteTrack receives inbound audio from the agent.
	// It is delivered via the RemoteTrackCh channel after the connection is established.
	RemoteTrackCh chan *webrtc.TrackRemote

	mu         sync.Mutex
	status     ConnectionStatus
	transcript []TranscriptEntry
	assistBuf  string

	audio audioState
}

// NewClient creates a new voice agent client with the given configuration and event handlers.
func NewClient(cfg Config, events EventHandler) *Client {
	resolved := cfg.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())

	return &Client{
		config:        resolved,
		events:        events,
		ctx:           ctx,
		cancel:        cancel,
		status:        StatusIdle,
		RemoteTrackCh: make(chan *webrtc.TrackRemote, 1),
	}
}

// Status returns the current connection status.
func (c *Client) Status() ConnectionStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Transcript returns the current conversation transcript.
func (c *Client) Transcript() []TranscriptEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]TranscriptEntry, len(c.transcript))
	copy(cp, c.transcript)
	return cp
}

// Connect establishes a WebRTC connection to the voice agent server using WHIP.
// It creates a local audio track (Opus), performs WHIP signaling, and sets up
// the data channel for receiving transcript/response events.
//
// After Connect returns, write audio to LocalTrack and read agent audio from RemoteTrackCh.
func (c *Client) Connect(ctx context.Context) error {
	c.setStatus(StatusConnecting)

	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    1,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		c.setStatus(StatusError)
		return fmt.Errorf("register codec: %w", err)
	}

	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		c.setStatus(StatusError)
		return fmt.Errorf("register interceptors: %w", err)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))

	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: c.config.ICEServers,
	})
	if err != nil {
		c.setStatus(StatusError)
		return fmt.Errorf("create peer connection: %w", err)
	}
	c.pc = pc

	// Create local audio track for sending audio to the server.
	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  1,
		},
		"audio",
		"streamcoreai-client",
	)
	if err != nil {
		c.setStatus(StatusError)
		pc.Close()
		return fmt.Errorf("create local track: %w", err)
	}
	c.LocalTrack = localTrack

	if err := c.initAudioSend(); err != nil {
		c.setStatus(StatusError)
		pc.Close()
		return fmt.Errorf("init audio: %w", err)
	}

	if _, err := pc.AddTrack(localTrack); err != nil {
		c.setStatus(StatusError)
		pc.Close()
		return fmt.Errorf("add track: %w", err)
	}

	// Create data channel for receiving events from the server.
	dc, err := pc.CreateDataChannel("events", nil)
	if err != nil {
		c.setStatus(StatusError)
		pc.Close()
		return fmt.Errorf("create data channel: %w", err)
	}

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var dcMsg DataChannelMessage
		if err := json.Unmarshal(msg.Data, &dcMsg); err != nil {
			log.Printf("[streamcoreai-sdk] failed to parse DC message: %v", err)
			return
		}
		if c.events.OnDataChannelMessage != nil {
			c.events.OnDataChannelMessage(dcMsg)
		}
		c.handleDataChannelMessage(dcMsg)
	})

	// Deliver remote audio track via channel.
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		select {
		case c.RemoteTrackCh <- track:
		default:
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			c.setStatus(StatusConnected)
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			c.setStatus(StatusDisconnected)
		case webrtc.PeerConnectionStateDisconnected:
			c.setStatus(StatusDisconnected)
		}
	})

	// Create offer.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		c.setStatus(StatusError)
		pc.Close()
		return fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		c.setStatus(StatusError)
		pc.Close()
		return fmt.Errorf("set local description: %w", err)
	}

	// Wait for ICE gathering to complete.
	gatherDone := webrtc.GatheringCompletePromise(pc)
	select {
	case <-gatherDone:
	case <-ctx.Done():
		c.setStatus(StatusError)
		pc.Close()
		return ctx.Err()
	}

	// Fetch a fresh token from the token endpoint if configured.
	token := c.config.Token
	if c.config.TokenURL != "" {
		t, err := fetchToken(c.config.TokenURL, c.config.APIKey)
		if err != nil {
			c.setStatus(StatusError)
			pc.Close()
			return fmt.Errorf("fetch token: %w", err)
		}
		token = t
	}

	// WHIP exchange.
	result, err := whipOffer(c.config.WHIPEndpoint, pc.LocalDescription().SDP, c.config.Metadata, token)
	if err != nil {
		c.setStatus(StatusError)
		pc.Close()
		return fmt.Errorf("whip offer: %w", err)
	}
	c.sessionURL = result.SessionURL

	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  result.AnswerSDP,
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		c.setStatus(StatusError)
		pc.Close()
		return fmt.Errorf("set remote description: %w", err)
	}

	return nil
}

// Disconnect tears down the WebRTC connection and frees resources.
func (c *Client) Disconnect() {
	c.cancel()
	whipDelete(c.sessionURL, c.config.Token)
	c.sessionURL = ""
	if c.pc != nil {
		done := make(chan struct{})
		go func() {
			c.pc.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			log.Println("pc.Close() timed out")
		}
		c.pc = nil
	}
	c.setStatus(StatusIdle)
}

func (c *Client) setStatus(s ConnectionStatus) {
	c.mu.Lock()
	c.status = s
	c.mu.Unlock()
	if c.events.OnStatusChange != nil {
		c.events.OnStatusChange(s)
	}
}

func (c *Client) handleDataChannelMessage(msg DataChannelMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch msg.Type {
	case "transcript":
		if msg.Final {
			pendingAssistant := c.assistBuf
			c.assistBuf = ""

			// Remove partial entries.
			var updated []TranscriptEntry
			for _, e := range c.transcript {
				if e.Role == "user" && e.Partial {
					continue
				}
				if e.Role == "assistant" && e.Partial {
					continue
				}
				updated = append(updated, e)
			}
			if pendingAssistant != "" {
				updated = append(updated, TranscriptEntry{Role: "assistant", Text: pendingAssistant})
			}
			updated = append(updated, TranscriptEntry{Role: "user", Text: msg.Text})
			c.transcript = updated
		} else {
			var updated []TranscriptEntry
			for _, e := range c.transcript {
				if e.Role == "user" && e.Partial {
					continue
				}
				updated = append(updated, e)
			}
			updated = append(updated, TranscriptEntry{Role: "user", Text: msg.Text, Partial: true})
			c.transcript = updated
		}

		if c.events.OnTranscript != nil {
			all := make([]TranscriptEntry, len(c.transcript))
			copy(all, c.transcript)
			c.events.OnTranscript(c.transcript[len(c.transcript)-1], all)
		}

	case "response":
		c.assistBuf += msg.Text
		currentText := c.assistBuf

		var updated []TranscriptEntry
		for _, e := range c.transcript {
			if e.Role == "assistant" && e.Partial {
				continue
			}
			updated = append(updated, e)
		}
		updated = append(updated, TranscriptEntry{Role: "assistant", Text: currentText, Partial: true})
		c.transcript = updated

		if c.events.OnTranscript != nil {
			all := make([]TranscriptEntry, len(c.transcript))
			copy(all, c.transcript)
			c.events.OnTranscript(c.transcript[len(c.transcript)-1], all)
		}

	case "error":
		if c.events.OnError != nil {
			c.events.OnError(fmt.Errorf("server: %s", msg.Message))
		}

	case "timing":
		if c.events.OnTiming != nil {
			c.events.OnTiming(TimingEvent{Stage: msg.Stage, Ms: msg.Ms})
		}

	case "state":
		if c.events.OnAgentStateChange != nil && msg.State != "" {
			c.events.OnAgentStateChange(AgentState(msg.State))
		}
	}
}
