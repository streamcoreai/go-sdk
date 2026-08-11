package streamcoreai

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godeps/opus"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	// SampleRate is the audio sample rate in Hz required by Opus.
	SampleRate = 48000
	// Channels is the number of audio channels (mono).
	Channels = 1
	// FrameSize is the number of samples in a 20ms frame at 48kHz.
	FrameSize = 960

	maxRTPPayload  = 1500
	rtpPayloadType = 111
	rtpSSRC        = 0xDEADBEEF
)

// audioState holds internal Opus codec and RTP sequencing state.
type audioState struct {
	encoder *opus.Encoder
	sendSeq uint32
	sendTS  uint32
	sendBuf []byte // pre-allocated Opus output buffer

	decoder     *opus.Decoder
	decoderOnce sync.Once
	decoderErr  error
	rtpBuf      []byte

	// remoteTrack is the inbound track currently being read. A reconnect
	// delivers a replacement on RemoteTrackCh, so this is re-read rather than
	// captured once — otherwise RecvPCM would keep reading the dead track from
	// before the drop and never recover.
	remoteTrackMu sync.Mutex
	remoteTrack   *webrtc.TrackRemote
}

// initAudioSend creates the Opus encoder for outbound audio.
func (c *Client) initAudioSend() error {
	enc, err := opus.NewEncoder(SampleRate, Channels, opus.AppVoIP)
	if err != nil {
		return fmt.Errorf("opus encoder: %w", err)
	}
	_ = enc.SetInBandFEC(true)
	c.audio.encoder = enc
	c.audio.sendBuf = make([]byte, maxRTPPayload)
	return nil
}

// SendPCM encodes a 20ms frame of PCM int16 audio (mono, 48kHz, 960 samples)
// and sends it as an RTP/Opus packet to the voice agent server.
func (c *Client) SendPCM(pcm []int16) error {
	if c.audio.encoder == nil {
		return fmt.Errorf("audio not initialized (call Connect first)")
	}
	if c.LocalTrack == nil {
		return fmt.Errorf("local track not available")
	}

	n, err := c.audio.encoder.Encode(pcm, c.audio.sendBuf)
	if err != nil {
		return fmt.Errorf("opus encode: %w", err)
	}

	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    rtpPayloadType,
			SequenceNumber: uint16(atomic.AddUint32(&c.audio.sendSeq, 1)),
			Timestamp:      atomic.AddUint32(&c.audio.sendTS, FrameSize),
			SSRC:           rtpSSRC,
		},
		Payload: c.audio.sendBuf[:n],
	}

	return c.LocalTrack.WriteRTP(pkt)
}

// RecvPCM blocks until a frame of audio is received from the agent, decodes
// the Opus payload, and writes PCM int16 samples into pcm.
// The pcm slice should have capacity for at least FrameSize (960) samples.
// Returns the number of decoded samples.
func (c *Client) RecvPCM(pcm []int16) (int, error) {
	c.audio.decoderOnce.Do(func() {
		dec, err := opus.NewDecoder(SampleRate, Channels)
		if err != nil {
			c.audio.decoderErr = fmt.Errorf("opus decoder: %w", err)
			return
		}
		c.audio.decoder = dec
		c.audio.rtpBuf = make([]byte, maxRTPPayload)
	})

	if c.audio.decoderErr != nil {
		return 0, c.audio.decoderErr
	}

	for {
		track, err := c.waitForRemoteTrack()
		if err != nil {
			return 0, err
		}

		n, _, err := track.Read(c.audio.rtpBuf)
		if err != nil {
			// The track died with the connection that carried it. If recovery
			// hands us a replacement, keep going: to the caller a reconnect
			// should be a gap in the audio, not the end of the stream.
			if c.discardRemoteTrack(track) {
				continue
			}
			return 0, err
		}

		var pkt rtp.Packet
		if err := pkt.Unmarshal(c.audio.rtpBuf[:n]); err != nil {
			continue
		}
		if len(pkt.Payload) == 0 {
			continue
		}

		nSamples, err := c.audio.decoder.Decode(pkt.Payload, pcm)
		if err != nil {
			continue
		}

		return nSamples, nil
	}
}

// waitForRemoteTrack returns the inbound track, blocking until one arrives.
//
// The track is re-read on every frame rather than captured once, because a
// reconnect replaces it: the transport that carried the old one is gone.
func (c *Client) waitForRemoteTrack() (*webrtc.TrackRemote, error) {
	c.audio.remoteTrackMu.Lock()
	track := c.audio.remoteTrack
	c.audio.remoteTrackMu.Unlock()
	if track != nil {
		return track, nil
	}

	select {
	case track := <-c.RemoteTrackCh:
		c.audio.remoteTrackMu.Lock()
		c.audio.remoteTrack = track
		c.audio.remoteTrackMu.Unlock()
		return track, nil
	case <-c.ctx.Done():
		return nil, c.ctx.Err()
	}
}

// discardRemoteTrack drops a track that failed to read and reports whether a
// replacement is already available — which is how a reconnect looks from here.
// Waits briefly rather than returning immediately, because the read fails the
// moment the transport dies and the new track only arrives once the redial has
// completed.
func (c *Client) discardRemoteTrack(dead *webrtc.TrackRemote) bool {
	c.audio.remoteTrackMu.Lock()
	if c.audio.remoteTrack == dead {
		c.audio.remoteTrack = nil
	}
	c.audio.remoteTrackMu.Unlock()

	deadline := c.config.ResumeDelay*time.Duration(c.config.ResumeAttempts+1)*2 + 5*time.Second
	select {
	case track := <-c.RemoteTrackCh:
		c.audio.remoteTrackMu.Lock()
		c.audio.remoteTrack = track
		c.audio.remoteTrackMu.Unlock()
		return true
	case <-time.After(deadline):
		return false
	case <-c.ctx.Done():
		return false
	}
}
