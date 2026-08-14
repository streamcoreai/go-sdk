package streamcoreai

import (
	"context"
	"encoding/json"
	"errors"
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
	// etag identifies the current ICE session; sent as If-Match on a restart.
	etag string
	// resumeToken is a single-use credential that reattaches a redial to this
	// conversation. Re-issued by the server on every response.
	resumeToken string
	// resumeStatus is the server's answer on the last POST: "new", "resumed",
	// or "expired".
	resumeStatus string
	// recovering guards the recovery sequence so it is never doubled up.
	recovering bool
	// generation is bumped by Connect and Disconnect so an in-flight recovery
	// abandons itself rather than fighting the connection replacing it.
	generation int

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
	// lastToken holds the most recently used JWT (either the static
	// config.Token or one fetched from TokenURL during Connect) so that
	// Disconnect can reuse it on the WHIP DELETE. Servers that enforce
	// Bearer auth on /whip will otherwise reject the teardown and skip
	// server-side finalization (billing, transcript persistence, etc.).
	lastToken string

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
	// A fresh Connect abandons whatever came before, including any resume
	// token: this is a new conversation, not a continuation.
	c.mu.Lock()
	c.generation++
	c.resumeToken = ""
	c.mu.Unlock()
	return c.establish(ctx, "")
}

// establish builds a peer connection and completes WHIP signaling.
//
// With a resume token the server reattaches this new transport to the
// conversation the previous one was having, so the history, the rolling
// summary and the agent's memory of the call all survive. Without one it is an
// ordinary first connect.
func (c *Client) establish(ctx context.Context, resumeToken string) error {
	if resumeToken != "" {
		c.setStatus(StatusReconnecting)
	} else {
		c.setStatus(StatusConnecting)
	}

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

	// Reuse the outbound track across a redial. It is a public field callers
	// write RTP to, so replacing it would leave their writer aimed at a track
	// bound to nothing. TrackLocalStaticRTP supports several bindings, so the
	// same track simply joins the new PeerConnection.
	localTrack := c.LocalTrack
	if localTrack == nil {
		localTrack, err = webrtc.NewTrackLocalStaticRTP(
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
		// A PeerConnection this client has already replaced must not report
		// its own teardown as the client going offline.
		if c.currentPC() != pc {
			return
		}
		switch state {
		case webrtc.PeerConnectionStateConnected:
			c.setStatus(StatusConnected)
		case webrtc.PeerConnectionStateDisconnected:
			// Transient. ICE often repairs this unaided, so the restart
			// sequence waits before spending an attempt — but if the local
			// address changed it never will, and this is the only window in
			// which an ICE restart can work: at ~25s the connection fails and
			// the server closes its peer.
			go c.recover()
		case webrtc.PeerConnectionStateFailed:
			// Straight to failed, or the restart phase ran out of time. ICE
			// restart is off the table; only a resume redial can save the
			// conversation now.
			go c.recover()
		case webrtc.PeerConnectionStateClosed:
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
		t, err := fetchToken(c.config.TokenURL, c.config.APIKey, c.config.ResourceID)
		if err != nil {
			c.setStatus(StatusError)
			pc.Close()
			return fmt.Errorf("fetch token: %w", err)
		}
		token = t
	}

	// Cache the token so Disconnect() can authenticate the WHIP DELETE.
	c.mu.Lock()
	c.lastToken = token
	c.mu.Unlock()

	// WHIP exchange.
	result, err := whipOffer(c.config.WHIPEndpoint, pc.LocalDescription().SDP, c.config.Metadata, token, resumeToken, c.config.ResourceID)
	if err != nil {
		c.setStatus(StatusError)
		pc.Close()
		return fmt.Errorf("whip offer: %w", err)
	}
	c.sessionURL = result.SessionURL
	c.mu.Lock()
	c.etag = result.ETag
	c.resumeToken = result.ResumeToken
	c.resumeStatus = result.ResumeStatus
	c.mu.Unlock()

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
	// Abandons any in-flight recovery: it would otherwise resume a session
	// this call is about to DELETE.
	c.mu.Lock()
	c.generation++
	c.resumeToken = ""
	c.resumeStatus = ""
	c.mu.Unlock()
	c.cancel()

	// Resolve the token used for the WHIP DELETE. Prefer the cached
	// token captured during Connect (which may have come from TokenURL),
	// fall back to the static config.Token, and as a last resort
	// re-fetch from TokenURL so teardown still authenticates.
	c.mu.Lock()
	token := c.lastToken
	c.mu.Unlock()
	if token == "" {
		token = c.config.Token
	}
	if token == "" && c.config.TokenURL != "" {
		if t, err := fetchToken(c.config.TokenURL, c.config.APIKey, c.config.ResourceID); err == nil {
			token = t
		}
	}
	whipDelete(c.sessionURL, token)
	c.sessionURL = ""
	c.mu.Lock()
	c.lastToken = ""
	c.etag = ""
	c.mu.Unlock()
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

// recover recovers a dropped connection, in two phases.
//
// ICE restart first, while the PeerConnection is merely Disconnected. It keeps
// the transport itself alive — same PeerConnection, same DTLS, same tracks —
// so nothing above it notices.
//
// Resume redial when that is no longer possible: once the connection has
// Failed the server has closed its peer and there is nothing left to restart.
// A fresh PeerConnection then POSTs with the session's resume token, and the
// server reattaches it to the same conversation. The transport is new; the
// history, the rolling summary and the agent's memory of the call are not.
//
// The phases are one sequence rather than two independent ones because they
// share a deadline: the server holds an abandoned conversation for
// session_grace_ms (30s by default), and time spent on restarts is time not
// available for redials.
func (c *Client) recover() {
	c.mu.Lock()
	if c.recovering {
		c.mu.Unlock()
		return
	}
	c.recovering = true
	generation := c.generation
	sessionURL := c.sessionURL
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.recovering = false
		c.mu.Unlock()
	}()

	if sessionURL == "" {
		return
	}

	c.setStatus(StatusReconnecting)
	if c.recoverByICERestart(generation, sessionURL) {
		return
	}
	if generation != c.currentGeneration() {
		return
	}
	c.recoverByResume(generation)
}

// recoverByICERestart is phase one: keep the transport alive with an ICE
// restart. Returns true when the drop is dealt with — recovered, or abandoned
// because the client itself went away. False means phase two should take over.
func (c *Client) recoverByICERestart(generation int, sessionURL string) bool {
	maxAttempts := c.config.ReconnectAttempts
	if maxAttempts <= 0 {
		return false
	}
	delay := c.config.ReconnectDelay

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		select {
		case <-time.After(delay):
		case <-c.ctx.Done():
			return true // Disconnect was called
		}
		delay *= 2

		if generation != c.currentGeneration() {
			return true
		}
		pc := c.currentPC()
		if pc == nil {
			return true
		}
		switch pc.ConnectionState() {
		case webrtc.PeerConnectionStateClosed:
			return true
		case webrtc.PeerConnectionStateConnected:
			// Healed on its own while we waited.
			c.setStatus(StatusConnected)
			c.emitReconnect(ReconnectEvent{attempt, maxAttempts, PhaseICERestart, ReconnectRecovered, nil})
			return true
		case webrtc.PeerConnectionStateFailed:
			// The server has already closed its peer; every further restart
			// would 404.
			return false
		}

		c.emitReconnect(ReconnectEvent{attempt, maxAttempts, PhaseICERestart, ReconnectAttempting, nil})

		err := c.restartICE(pc, sessionURL)
		if err == nil {
			// The restart is applied; ICE still has to complete its checks, so
			// the move back to StatusConnected comes from the state handler.
			c.emitReconnect(ReconnectEvent{attempt, maxAttempts, PhaseICERestart, ReconnectRecovered, nil})
			return true
		}

		var restartErr *WHIPRestartError
		if errors.As(err, &restartErr) {
			// 404/409/405 mean the session cannot be restarted at all — stop
			// burning the shared deadline and let the resume phase try.
			if !restartErr.Retryable() {
				return false
			}
			// A 412 means another restart landed first; adopt the ETag the
			// server reported as current and try again against that generation.
			if restartErr.CurrentETag != "" {
				c.mu.Lock()
				c.etag = restartErr.CurrentETag
				c.mu.Unlock()
			}
		}
		log.Printf("[streamcoreai-sdk] ICE restart attempt %d failed: %v", attempt, err)
	}
	return false
}

// recoverByResume is phase two: rebuild the transport and reattach it to the
// conversation. This is the only recovery available once the connection has
// failed, and the only one at all for a process that was suspended or offline
// for longer than ICE tolerates.
func (c *Client) recoverByResume(generation int) {
	maxAttempts := c.config.ResumeAttempts
	c.mu.Lock()
	token := c.resumeToken
	c.mu.Unlock()

	if maxAttempts <= 0 || token == "" {
		c.setStatus(StatusDisconnected)
		return
	}

	delay := c.config.ResumeDelay
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// The dead PeerConnection still holds a binding on the outbound track;
		// drop it before building its replacement.
		c.closePeerConnection()

		select {
		case <-time.After(delay):
		case <-c.ctx.Done():
			return
		}
		delay *= 2

		if generation != c.currentGeneration() {
			return
		}

		c.emitReconnect(ReconnectEvent{attempt, maxAttempts, PhaseResume, ReconnectAttempting, nil})

		if err := c.establish(c.ctx, token); err != nil {
			if attempt == maxAttempts {
				c.setStatus(StatusDisconnected)
				c.emitReconnect(ReconnectEvent{attempt, maxAttempts, PhaseResume, ReconnectFailed, err})
				return
			}
			log.Printf("[streamcoreai-sdk] resume attempt %d failed: %v", attempt, err)
			continue
		}

		c.mu.Lock()
		status := c.resumeStatus
		c.mu.Unlock()

		if status == "resumed" {
			c.emitReconnect(ReconnectEvent{attempt, maxAttempts, PhaseResume, ReconnectRecovered, nil})
		} else {
			log.Printf("[streamcoreai-sdk] reconnected, but the server could not resume "+
				"the session (status=%q) — the agent has no memory of the earlier conversation", status)
			c.emitReconnect(ReconnectEvent{attempt, maxAttempts, PhaseResume, ReconnectRecoveredWithoutHistory, nil})
		}
		return
	}
}

// closePeerConnection closes the current PeerConnection without touching
// session state, so a redial can still use the session URL and resume token.
func (c *Client) closePeerConnection() {
	c.mu.Lock()
	pc := c.pc
	c.pc = nil
	c.mu.Unlock()
	if pc == nil {
		return
	}
	// The state handler checks currentPC() and so ignores this teardown.
	done := make(chan struct{})
	go func() {
		pc.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		log.Println("[streamcoreai-sdk] pc.Close() timed out during recovery")
	}
}

func (c *Client) currentPC() *webrtc.PeerConnection {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pc
}

func (c *Client) currentGeneration() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

// restartICE performs one ICE restart round-trip against the existing peer
// connection.
func (c *Client) restartICE(pc *webrtc.PeerConnection, sessionURL string) error {
	remote := pc.RemoteDescription()
	if remote == nil {
		return fmt.Errorf("no remote description to restart against")
	}

	offer, err := pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		return fmt.Errorf("create restart offer: %w", err)
	}
	// The promise must be created after CreateOffer, which is what reopens
	// gathering — one made earlier would resolve against the old generation.
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set restart offer: %w", err)
	}
	select {
	case <-gatherDone:
	case <-time.After(5 * time.Second):
		log.Println("[streamcoreai-sdk] ICE re-gathering timed out, patching partial candidates")
	case <-c.ctx.Done():
		return c.ctx.Err()
	}

	c.mu.Lock()
	etag, token := c.etag, c.lastToken
	c.mu.Unlock()
	if token == "" {
		token = c.config.Token
	}

	result, err := whipRestartICE(
		sessionURL,
		iceFragmentFromSDP(pc.LocalDescription().SDP),
		etag,
		token,
	)
	if err != nil {
		return err
	}

	answerSDP, ok := applyICEFragment(remote.SDP, result.Fragment)
	if !ok {
		return fmt.Errorf("ICE restart reply has no ICE credentials")
	}

	// The offer left us in have-local-offer; the connection only returns to
	// stable once this answer is applied.
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		return fmt.Errorf("set restart answer: %w", err)
	}

	c.mu.Lock()
	c.etag = result.ETag
	c.mu.Unlock()
	return nil
}

func (c *Client) emitReconnect(event ReconnectEvent) {
	if c.events.OnReconnect != nil {
		c.events.OnReconnect(event)
	}
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
