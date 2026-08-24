# github.com/streamcoreai/go-sdk

[English](./README.md) | **简体中文**

Go SDK，通过 WebRTC + WHIP 连接 [StreamCoreAI](https://github.com/streamcoreai/streamcore-server) 服务端。

当本模块位于拆分出去的独立仓库时，请为发布打 tag（例如 `v0.1.0`），这样下游模块无需 `replace` 指令即可 `require` 某个版本。见 [仓库结构](../docs/repository-structure.md)。

## 安装

```bash
go get github.com/streamcoreai/go-sdk
```

## 快速开始

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

创建一个新的客户端实例。

#### `Config`

| 字段          | 类型               | 默认值                              | 说明                     |
| -------------- | ------------------ | ------------------------------------ | ------------------------------- |
| `WHIPEndpoint` | `string`           | `"http://localhost:8080/whip"`       | WHIP 信令端点 URL     |
| `Token`        | `string`           | —                                    | 在 WHIP 请求中以 `Authorization: Bearer` 发送的 JWT |
| `TokenURL`     | `string`           | —                                    | token 端点；设置后每次连接前都会取一次 JWT（优先于 `Token`） |
| `APIKey`       | `string`           | —                                    | 从 `TokenURL` 取 token 时以 `Authorization: Bearer` 发送 |
| `ResourceID`   | `string`           | —                                    | 通话里的是谁，会转发给外部 agent，使其把记忆划到人而非单次通话上。设置了 `TokenURL` 时随取 token 的请求体发送（由服务端签进 token），否则作为 `X-StreamCore-Resource-Id` 请求头发送 |
| `ICEServers`   | `[]webrtc.ICEServer` | Google STUN 服务器                 | ICE 服务器配置        |

#### `EventHandler`

所有回调都是可选的；不用的字段留空即可。

| 回调               | 签名                                                     | 说明                             |
| ---------------------- | ------------------------------------------------------------- | --------------------------------------- |
| `OnStatusChange`       | `func(status ConnectionStatus)`                               | 连接状态变化时触发    |
| `OnTranscript`         | `func(entry TranscriptEntry, all []TranscriptEntry)`          | 有新的或更新的转写时触发      |
| `OnAgentStateChange`   | `func(state AgentState)`                                      | 智能体开始聆听、思考或说话时触发 |
| `OnTiming`             | `func(event TimingEvent)`                                     | 携带服务端流水线耗时信息 |
| `OnError`              | `func(err error)`                                             | 连接或服务端错误时触发    |
| `OnDataChannelMessage` | `func(msg DataChannelMessage)`                                | 每条原始 DataChannel 消息都会触发 |
| `OnData`               | `func(topic string, payload []byte)`                          | 服务端下发的单向数据包，payload 已完成 base64 解码（`movement.command` 承载移动指令） |

### 客户端方法

| 方法                          | 返回值              | 说明                                          |
| ------------------------------- | -------------------- | ---------------------------------------------------- |
| `Connect(ctx context.Context)`  | `error`              | 建立 WebRTC + WHIP 会话                      |
| `Disconnect()`                  | —                    | 拆除连接、释放资源                 |
| `Status()`                      | `ConnectionStatus`   | 当前连接状态                            |
| `Transcript()`                  | `[]TranscriptEntry`  | 完整对话历史（副本）                     |
| `SendPCM(pcm []int16)`         | `error`              | 编码并发送一个 20ms PCM 帧（960 采样点、单声道、48kHz） |
| `RecvPCM(pcm []int16)`         | `(int, error)`       | 接收智能体音频并解码为 PCM（阻塞直到有数据） |

### 音频常量

| 常量     | 值  | 说明                             |
| ------------ | ------ | --------------------------------------- |
| `SampleRate` | 48000  | 音频采样率（Hz，Opus）          |
| `Channels`   | 1      | 音频声道数（单声道）         |
| `FrameSize`  | 960    | 48kHz 下每 20ms 帧的采样点数         |

### 客户端字段（进阶 —— 用于自定义音频流水线）

| 字段           | 类型                          | 说明                                    |
| --------------- | ----------------------------- | ---------------------------------------------- |
| `LocalTrack`    | `*webrtc.TrackLocalStaticRTP` | 向服务端发送音频的原始 RTP track      |
| `RemoteTrackCh` | `chan *webrtc.TrackRemote`    | 接收智能体的原始音频 track           |

### 类型

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

## 音频 I/O

SDK 内部处理 **Opus 编解码与 RTP 打包**。你只需要提供和消费原始 PCM 音频：

- **发送音频**：采集 PCM int16 采样（单声道、48kHz、每帧 960 采样点）并调用 `client.SendPCM(pcm)`
- **接收音频**：调用 `client.RecvPCM(pcm)` 获取来自智能体的解码 PCM 采样

配置 PortAudio 时，请使用导出的常量：`streamcoreai.SampleRate`、`streamcoreai.Channels`、`streamcoreai.FrameSize`。

对于需要直接访问 WebRTC track 的进阶场景（自定义编解码、原始 RTP），`LocalTrack` 和 `RemoteTrackCh` 字段依然可用。

参考实现见 [Go CLI 示例](../examples/golang/) 和 [Go TUI 示例](../examples/golang-tui/)。

## 许可证

MIT
