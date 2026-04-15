# @streamcoreai/go-sdk

Go SDK for connecting to a [StreamCoreAI](https://github.com/streamcoreai/streamcore-server) server via WebRTC + WHIP.

When this module lives in a split repository, tag releases (for example `v0.1.0`) so downstream modules can `require` a version without a `replace` directive. See [Repository layout](../docs/repository-structure.md).

## Installation

```bash
go get github.com/streamcoreai/go-sdk
```

## Quick Start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	streamcoreai "github.com/streamcoreai/go-sdk"
)

func main() {
	client := streamcoreai.NewClient(
		streamcoreai.Config{
			WHIPEndpoint: "http://localhost:8080/whip",
		},
		streamcoreai.EventHandler{
			OnStatusChange: func(status streamcoreai.ConnectionStatus) {
				fmt.Println("Status:", status)
			},
			OnTranscript: func(entry streamcoreai.TranscriptEntry, all []streamcoreai.TranscriptEntry) {
				fmt.Printf("[%s] %s\n", entry.Role, entry.Text)
			},
			OnError: func(err error) {
				log.Println("Error:", err)
			},
		},
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		log.Fatal(err)
	}
	defer client.Disconnect()

	// Send microphone audio to the agent.
	// (Replace with your actual audio capture — e.g. PortAudio)
	go func() {
		pcm := make([]int16, streamcoreai.FrameSize) // 20ms mono 48kHz
		for {
			// Fill pcm from your audio source...
			if err := client.SendPCM(pcm); err != nil {
				return
			}
		}
	}()

	// Receive agent audio and play it.
	go func() {
		pcm := make([]int16, streamcoreai.FrameSize)
		for {
			nSamples, err := client.RecvPCM(pcm)
			if err != nil {
				return
			}
			// Play pcm[:nSamples] through your audio output...
			_ = nSamples
		}
	}()

	<-ctx.Done()
}
```

## API

### `streamcoreai.NewClient(cfg, events)`

Creates a new client instance.

#### `Config`

| Field          | Type               | Default                              | Description                     |
| -------------- | ------------------ | ------------------------------------ | ------------------------------- |
| `WHIPEndpoint` | `string`           | `"http://localhost:8080/whip"`       | WHIP signaling endpoint URL     |
| `ICEServers`   | `[]webrtc.ICEServer` | Google STUN server                 | ICE server configuration        |

#### `EventHandler`

| Callback               | Signature                                                     | Description                             |
| ---------------------- | ------------------------------------------------------------- | --------------------------------------- |
| `OnStatusChange`       | `func(status ConnectionStatus)`                               | Fired when connection status changes    |
| `OnTranscript`         | `func(entry TranscriptEntry, all []TranscriptEntry)`          | Fired on new or updated transcript      |
| `OnError`              | `func(err error)`                                             | Fired on connection or server errors    |
| `OnDataChannelMessage` | `func(msg DataChannelMessage)`                                | Fired for every raw data channel message |

### Client Methods

| Method                          | Returns              | Description                                          |
| ------------------------------- | -------------------- | ---------------------------------------------------- |
| `Connect(ctx context.Context)`  | `error`              | Establish WebRTC + WHIP session                      |
| `Disconnect()`                  | —                    | Tear down connection, free resources                 |
| `Status()`                      | `ConnectionStatus`   | Current connection status                            |
| `Transcript()`                  | `[]TranscriptEntry`  | Full conversation history (copy)                     |
| `SendPCM(pcm []int16)`         | `error`              | Encode and send a 20ms PCM frame (960 samples, mono, 48kHz) |
| `RecvPCM(pcm []int16)`         | `(int, error)`       | Receive and decode agent audio into PCM (blocks until available) |

### Audio Constants

| Constant     | Value  | Description                             |
| ------------ | ------ | --------------------------------------- |
| `SampleRate` | 48000  | Audio sample rate in Hz (Opus)          |
| `Channels`   | 1      | Number of audio channels (mono)         |
| `FrameSize`  | 960    | Samples per 20ms frame at 48kHz         |

### Client Fields (advanced — for custom audio pipelines)

| Field           | Type                          | Description                                    |
| --------------- | ----------------------------- | ---------------------------------------------- |
| `LocalTrack`    | `*webrtc.TrackLocalStaticRTP` | Raw RTP track for sending audio to server      |
| `RemoteTrackCh` | `chan *webrtc.TrackRemote`    | Receives the agent's raw audio track           |

### Types

```go
type ConnectionStatus string // "idle", "connecting", "connected", "error", "disconnected"

type TranscriptEntry struct {
    Role    string // "user" or "assistant"
    Text    string
    Partial bool
}

type DataChannelMessage struct {
    Type    string // "transcript", "response", or "error"
    Text    string
    Final   bool
    Message string // for error type
}
```

## Audio I/O

The SDK handles **Opus encoding/decoding and RTP packetization** internally. You only need to supply and consume raw PCM audio:

- **Sending audio**: Capture PCM int16 samples (mono, 48kHz, 960 samples per frame) and call `client.SendPCM(pcm)`
- **Receiving audio**: Call `client.RecvPCM(pcm)` to get decoded PCM samples from the agent

For PortAudio setup, use the exported constants: `streamcoreai.SampleRate`, `streamcoreai.Channels`, `streamcoreai.FrameSize`.

For advanced use cases that need direct access to the WebRTC tracks (custom codecs, raw RTP), the `LocalTrack` and `RemoteTrackCh` fields are still available.

For reference implementations, see the [Go CLI example](../examples/golang/) and [Go TUI example](../examples/golang-tui/).

## License

MIT
