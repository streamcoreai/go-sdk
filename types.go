package streamcoreai

import "github.com/pion/webrtc/v4"

// ConnectionStatus represents the current state of the voice agent connection.
type ConnectionStatus string

const (
	StatusIdle         ConnectionStatus = "idle"
	StatusConnecting   ConnectionStatus = "connecting"
	StatusConnected    ConnectionStatus = "connected"
	StatusError        ConnectionStatus = "error"
	StatusDisconnected ConnectionStatus = "disconnected"
)

// TranscriptEntry represents a single transcript message in the conversation.
type TranscriptEntry struct {
	Role    string `json:"role"`    // "user" or "assistant"
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
	Type    string `json:"type"`              // "transcript", "response", "error", "timing", or "state"
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
	return out
}
