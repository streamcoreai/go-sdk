package streamcoreai

import (
	"time"

	"github.com/pion/webrtc/v4"
)

// ConnectionStatus represents the current state of the voice agent connection.
type ConnectionStatus string

const (
	StatusIdle       ConnectionStatus = "idle"
	StatusConnecting ConnectionStatus = "connecting"
	StatusConnected  ConnectionStatus = "connected"
	// StatusReconnecting means the transport dropped and an ICE restart is in
	// flight. The session, and with it the conversation, is still alive on the
	// server — this is not a terminal state and usually resolves back to
	// StatusConnected.
	StatusReconnecting ConnectionStatus = "reconnecting"
	StatusError        ConnectionStatus = "error"
	StatusDisconnected ConnectionStatus = "disconnected"
)

// ReconnectPhase identifies which recovery mechanism an attempt used.
type ReconnectPhase string

const (
	// PhaseICERestart keeps the existing transport alive. Only possible while
	// the connection is Disconnected; nothing above the transport notices.
	PhaseICERestart ReconnectPhase = "ice-restart"
	// PhaseResume rebuilds the transport and reattaches it to the same
	// server-side conversation. The only option once the connection has Failed.
	PhaseResume ReconnectPhase = "resume"
)

// ReconnectOutcome describes where a recovery attempt got to.
type ReconnectOutcome string

const (
	ReconnectAttempting ReconnectOutcome = "attempting"
	ReconnectRecovered  ReconnectOutcome = "recovered"
	// ReconnectRecoveredWithoutHistory means the call works but the server
	// could not resume the session, so the agent has forgotten the
	// conversation and will not know what was already said. Worth surfacing
	// to the user rather than only logging.
	ReconnectRecoveredWithoutHistory ReconnectOutcome = "recovered-without-history"
	ReconnectFailed                  ReconnectOutcome = "failed"
)

// ReconnectEvent reports progress of the recovery sequence.
type ReconnectEvent struct {
	Attempt     int              // 1-based attempt number, counted within the phase
	MaxAttempts int              // attempts this phase makes before moving on
	Phase       ReconnectPhase   // which mechanism this attempt used
	Outcome     ReconnectOutcome // "attempting" while in flight, then the result
	Err         error            // why the attempt failed, when Outcome is ReconnectFailed
}

// TranscriptEntry represents a single transcript message in the conversation.
type TranscriptEntry struct {
	Role    string `json:"role"` // "user" or "assistant"
	Text    string `json:"text"`
	Partial bool   `json:"partial,omitempty"`
}

// AgentState represents the server-reported state of the voice agent pipeline.
type AgentState string

const (
	AgentListening AgentState = "listening"
	AgentThinking  AgentState = "thinking"
	AgentSpeaking  AgentState = "speaking"
)

// TimingEvent represents a single latency measurement from the server pipeline.
type TimingEvent struct {
	Stage string `json:"stage"`
	Ms    int    `json:"ms"`
}

// DataChannelMessage represents a message received on the data channel.
type DataChannelMessage struct {
	Type    string `json:"type"` // "transcript", "response", "error", "timing", or "state"
	Text    string `json:"text,omitempty"`
	Final   bool   `json:"final,omitempty"`
	Message string `json:"message,omitempty"` // for error type
	Stage   string `json:"stage,omitempty"`   // for timing type
	Ms      int    `json:"ms,omitempty"`      // for timing type
	State   string `json:"state,omitempty"`   // for state type
}

// Config holds the configuration for a StreamCoreAIClient.
type Config struct {
	// WHIPEndpoint is the URL of the WHIP signaling endpoint.
	// Defaults to "http://localhost:8080/whip".
	WHIPEndpoint string

	// Token is an optional JWT token for authenticating with the WHIP endpoint.
	Token string

	// TokenURL is the URL of a token endpoint. If set, the client will POST
	// to this URL to fetch a JWT before each WHIP connection. Overrides Token.
	TokenURL string

	// APIKey is sent as a Bearer header when fetching a token from TokenURL.
	APIKey string

	// ICEServers configures the ICE servers for the WebRTC connection.
	// Defaults to Google's public STUN server.
	ICEServers []webrtc.ICEServer

	// Metadata is an optional set of key-value pairs appended to the WHIP
	// endpoint as query parameters (e.g. direction=outbound).
	Metadata map[string]string

	// ReconnectAttempts is how many ICE restarts to attempt before giving up
	// on a dropped connection. Attempts are spaced by ReconnectDelay doubling
	// each time, and the whole sequence must finish inside the ~25 seconds it
	// takes the connection to go from disconnected to failed — past that the
	// server has closed the peer and only a fresh Connect recovers.
	// Defaults to 3. Negative disables automatic reconnection.
	ReconnectAttempts int

	// ReconnectDelay is the wait before the first ICE restart attempt,
	// doubling for each retry. The initial wait matters: most disconnected
	// transitions are brief packet loss that ICE repairs on its own, and
	// patching immediately would spend a restart on a connection that was
	// about to recover by itself. Defaults to 2s.
	ReconnectDelay time.Duration

	// ResumeAttempts is how many times to redial with the session's resume
	// token after ICE restart is no longer possible — which is the case as
	// soon as the connection reaches Failed, and the only case for a process
	// that was suspended or offline longer than ICE tolerates.
	//
	// A redial builds a new transport but reattaches to the same conversation,
	// so history and the agent's memory survive. The deadline is the server's
	// session_grace_ms (30s by default) measured from the drop, and the ICE
	// restart phase spends from the same budget. Defaults to 2. Negative
	// disables the resume phase.
	ResumeAttempts int

	// ResumeDelay is the wait before the first resume redial, doubling for
	// each retry. Shorter than ReconnectDelay because by this point the
	// connection is known dead — there is nothing left to wait out.
	// Defaults to 1s.
	ResumeDelay time.Duration
}

// EventHandler defines callbacks for voice agent events.
type EventHandler struct {
	// OnStatusChange is called when the connection status changes.
	OnStatusChange func(status ConnectionStatus)

	// OnTranscript is called when a new or updated transcript entry is received.
	OnTranscript func(entry TranscriptEntry, all []TranscriptEntry)

	// OnError is called when an error occurs.
	OnError func(err error)

	// OnTiming is called when a timing/latency event is received from the server.
	OnTiming func(event TimingEvent)

	// OnAgentStateChange is called when the server reports an agent state transition.
	OnAgentStateChange func(state AgentState)

	// OnDataChannelMessage is called for every raw data channel message.
	// This is optional and useful for custom message handling.
	OnDataChannelMessage func(msg DataChannelMessage)

	// OnReconnect is called for each recovery attempt and once when the
	// outcome is known, so a UI can distinguish a recoverable drop from a
	// lost call — and, via ReconnectRecoveredWithoutHistory, a reconnection
	// that kept the conversation from one that lost it.
	OnReconnect func(event ReconnectEvent)
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.WHIPEndpoint == "" {
		out.WHIPEndpoint = "http://localhost:8080/whip"
	}
	if len(out.ICEServers) == 0 {
		out.ICEServers = []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		}
	}
	if out.ReconnectAttempts == 0 {
		out.ReconnectAttempts = 3
	}
	if out.ReconnectDelay <= 0 {
		out.ReconnectDelay = 2 * time.Second
	}
	if out.ResumeAttempts == 0 {
		out.ResumeAttempts = 2
	}
	if out.ResumeDelay <= 0 {
		out.ResumeDelay = time.Second
	}
	return out
}
