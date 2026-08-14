# github.com/streamcoreai/go-sdk

**English** | [简体中文](./README.zh-CN.md)

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
| `Token`        | `string`           | —                                    | JWT sent as `Authorization: Bearer` on the WHIP request |
| `TokenURL`     | `string`           | —                                    | Token endpoint; when set, a JWT is fetched before each connection (overrides `Token`) |
| `APIKey`       | `string`           | —                                    | Sent as `Authorization: Bearer` when fetching from `TokenURL` |
| `ResourceID`   | `string`           | —                                    | Who is on the call, forwarded to an external agent so it can scope memory to the person rather than the call. Sent in the token request body when `TokenURL` is set (the server signs it into the token), otherwise as an `X-StreamCore-Resource-Id` header |
| `ICEServers`   | `[]webrtc.ICEServer` | Google STUN server                 | ICE server configuration        |
| `ReconnectAttempts` | `int`         | `3`                                  | ICE restarts while `Disconnected`; negative disables the phase |
| `ReconnectDelay` | `time.Duration`  | `2s`                                 | Wait before the first ICE restart, doubling each retry |
| `ResumeAttempts` | `int`            | `2`                                  | Resume redials once the connection has `Failed`; negative disables the phase |
| `ResumeDelay`  | `time.Duration`    | `1s`                                 | Wait before the first redial, doubling each retry |

#### `EventHandler`

All callbacks are optional; leave unused fields nil.

| Callback               | Signature                                                     | Description                             |
| ---------------------- | ------------------------------------------------------------- | --------------------------------------- |
| `OnStatusChange`       | `func(status ConnectionStatus)`                               | Fired when connection status changes    |
| `OnTranscript`         | `func(entry TranscriptEntry, all []TranscriptEntry)`          | Fired on new or updated transcript      |
| `OnAgentStateChange`   | `func(state AgentState)`                                      | Fired when the agent starts listening, thinking, or speaking |
| `OnTiming`             | `func(event TimingEvent)`                                     | Fired with server-side pipeline timing info |
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

## Reconnection

A network change mid-call — a machine moving networks, a VPN toggle, a process
suspended and resumed — kills the transport without ending the call. The client
recovers it automatically and the conversation survives: the agent still knows
who it is talking to and does not replay its greeting.

Recovery runs as a **ladder of two phases**:

| Phase | When | Cost |
|-------|------|------|
| **ICE restart** | While the connection is `Disconnected` | Invisible. Same `PeerConnection`, same DTLS, same tracks — just new candidates. |
| **Resume redial** | Once the connection has `Failed` | A full renegotiation and a moment of silence, but the server reattaches you to the same conversation. |

ICE restart is tried first because it costs nothing. It stops being possible
the moment the connection reaches `Failed` — the server has closed its peer by
then — which is where a process that was paused, or offline for more than about
25 seconds, always lands. The resume phase recovers those.

Status goes `StatusConnected` → `StatusReconnecting` → `StatusConnected`:

```go
client := streamcoreai.NewClient(streamcoreai.Config{
    WHIPEndpoint:      "http://localhost:8080/whip",
    ReconnectAttempts: 3,                // ICE restarts, 2s -> 4s -> 8s
    ReconnectDelay:    2 * time.Second,
    ResumeAttempts:    2,                // then redials,  1s -> 2s
    ResumeDelay:       time.Second,
}, streamcoreai.EventHandler{
    OnReconnect: func(e streamcoreai.ReconnectEvent) {
        log.Printf("%s %d/%d: %s", e.Phase, e.Attempt, e.MaxAttempts, e.Outcome)
        if e.Outcome == streamcoreai.ReconnectRecoveredWithoutHistory {
            log.Println("reconnected, but the agent has lost the conversation")
        }
    },
})
```

**Handle `ReconnectRecoveredWithoutHistory`.** It means the call is working but
the server could not resume the session — usually because the client was away
longer than `session_grace_ms` — so the agent has no memory of anything said
before. Everything still functions, which is exactly why it goes unnoticed
until the agent asks something it was already told.

Two details worth knowing:

- **The first ICE restart is deliberately delayed** (`ReconnectDelay`, default
  2s). Most drops are brief packet loss that ICE repairs unaided, and patching
  immediately would spend an attempt on a connection that was about to recover
  by itself.
- **Both phases share one deadline.** `Disconnected` becomes `Failed` after
  roughly 25 seconds, and the server then holds the conversation for
  `session_grace_ms` (30s by default). Raising `ReconnectAttempts` spends
  budget the resume phase would otherwise have.

Set `ReconnectAttempts` or `ResumeAttempts` negative to disable either phase.

**What this means for your audio loops.** `LocalTrack` is deliberately *not*
replaced across a redial — the same track is rebound to the new
`PeerConnection`, so a goroutine writing RTP to it keeps working. Inbound audio
is different: the server sends a new track, delivered on `RemoteTrackCh`.
`RecvPCM` handles that for you (it re-acquires and keeps decoding, so a
reconnect is a gap in the audio rather than an error). If you read
`RemoteTrackCh` yourself, expect a second track after a reconnect and switch to
it — the old one only returns read errors.

## Audio I/O
## Audio I/O

The SDK handles **Opus encoding/decoding and RTP packetization** internally. You only need to supply and consume raw PCM audio:

- **Sending audio**: Capture PCM int16 samples (mono, 48kHz, 960 samples per frame) and call `client.SendPCM(pcm)`
- **Receiving audio**: Call `client.RecvPCM(pcm)` to get decoded PCM samples from the agent

For PortAudio setup, use the exported constants: `streamcoreai.SampleRate`, `streamcoreai.Channels`, `streamcoreai.FrameSize`.

For advanced use cases that need direct access to the WebRTC tracks (custom codecs, raw RTP), the `LocalTrack` and `RemoteTrackCh` fields are still available.

For reference implementations, see the [Go CLI example](../examples/golang/) and [Go TUI example](../examples/golang-tui/).

## License

MIT
