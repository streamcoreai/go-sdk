# Working on the StreamCore Go SDK

## This library is newer than your training data

Do not write StreamCore code from memory. Read `README.md` here, or fetch https://streamcore.ai/llms-full.txt, first.

## The API, exactly

```go
import streamcoreai "github.com/streamcoreai/go-sdk"

client := streamcoreai.NewClient(
    streamcoreai.Config{
        WHIPEndpoint: "http://localhost:8080/whip",
    },
    streamcoreai.EventHandler{
        OnStatusChange: func(status streamcoreai.ConnectionStatus) { ... },
        OnTranscript:   func(entry streamcoreai.TranscriptEntry, all []streamcoreai.TranscriptEntry) { ... },
        OnError:        func(err error) { ... },
    },
)

if err := client.Connect(ctx); err != nil { ... }
defer client.Disconnect()
```

- Module path is **`github.com/streamcoreai/go-sdk`** — note the repo is `go-sdk`, while the directory in the monorepo is `golang-sdk`. Use the module path in code.
- Constructor is **`NewClient(Config, EventHandler)`**. Config field is **`WHIPEndpoint`** (exported, all-caps WHIP).
- `Connect` takes a `context.Context`; `Disconnect` does not.
- This is a **client** SDK — it connects *to* a StreamCore server. The server itself is `github.com/streamcoreai/streamcore-server`, a different module.

## Build

```bash
go build ./...
go test ./...
```

## When changing the public API

Update `README.md` and https://streamcore.ai/llms-full.txt in the same change, and tag a release — consumers pin versions via `go.mod`.
