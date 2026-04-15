package streamcoreai

import (
	"fmt"
	"sync"
	"sync/atomic"

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
	remoteTrack *webrtc.TrackRemote
	rtpBuf      []byte
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
		// Wait for the remote track to arrive.
		select {
		case track := <-c.RemoteTrackCh:
			c.audio.remoteTrack = track
		case <-c.ctx.Done():
			c.audio.decoderErr = c.ctx.Err()
			return
		}

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
		n, _, err := c.audio.remoteTrack.Read(c.audio.rtpBuf)
		if err != nil {
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
