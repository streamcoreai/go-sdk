package streamcoreai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ResourceIDHeader carries the caller identity when there is no token endpoint
// to sign one into a claim. For server-side clients, not browsers.
const ResourceIDHeader = "X-StreamCore-Resource-Id"

// WHIPResult holds the response from a WHIP signaling exchange.
type WHIPResult struct {
	AnswerSDP  string
	SessionURL string
	// ETag identifies the ICE session (RFC 9725 §4.3.1) and is required to
	// PATCH it for an ICE restart.
	ETag string
	// ResumeToken is a single-use credential for reattaching a later redial to
	// this conversation. Empty when the server cannot resume — a realtime
	// (speech-to-speech) session, whose history lives inside the provider.
	ResumeToken string
	// ResumeStatus is "new", "resumed", or "expired". Anything but "resumed"
	// on a redial means the agent has no memory of the earlier conversation.
	ResumeStatus string
}

// WHIPRestartResult holds the server's reply to an ICE restart.
type WHIPRestartResult struct {
	// Fragment is the server's sdpfrag, to fold into the stored remote
	// description.
	Fragment string
	// ETag is the rotated tag identifying the new ICE session.
	ETag string
}

// WHIPRestartError is returned when a PATCH is rejected. Status lets the
// caller tell a retryable failure from a terminal one.
type WHIPRestartError struct {
	Status int
	Body   string
	// CurrentETag is the tag the server reported as current, present on a 412.
	CurrentETag string
}

func (e *WHIPRestartError) Error() string {
	return fmt.Sprintf("whip: ICE restart failed (%d): %s", e.Status, e.Body)
}

// Retryable reports whether another attempt against the same session could
// still succeed. A 404 means the session was reaped, 409 that it has no peer
// to restart, and 405 that the server declines restarts entirely — only a
// redial recovers from those.
func (e *WHIPRestartError) Retryable() bool {
	return e.Status != http.StatusNotFound &&
		e.Status != http.StatusConflict &&
		e.Status != http.StatusMethodNotAllowed
}

// whipOffer performs a WHIP signaling exchange per RFC 9725 §4.2:
// POST an SDP offer, receive a 201 Created with SDP answer and Location header.
//
// resumeToken is a StreamCore extension: it asks the server to reattach this
// new transport to the conversation a previous connection was having, rather
// than starting a fresh one. Check ResumeStatus on the result — a token the
// server no longer recognises still yields a working call, but one whose agent
// remembers nothing.
//
// resourceID goes in a header, not a query parameter, and the server ignores it
// whenever the token already carries a claim.
func whipOffer(endpoint, offerSDP string, metadata map[string]string, token, resumeToken, resourceID string) (*WHIPResult, error) {
	// Append metadata and the resume token as query parameters.
	if len(metadata) > 0 || resumeToken != "" {
		parsed, err := url.Parse(endpoint)
		if err == nil {
			q := parsed.Query()
			for k, v := range metadata {
				q.Set(k, v)
			}
			if resumeToken != "" {
				q.Set("resume", resumeToken)
			}
			parsed.RawQuery = q.Encode()
			endpoint = parsed.String()
		}
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(offerSDP))
	if err != nil {
		return nil, fmt.Errorf("whip: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/sdp")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if resourceID != "" {
		req.Header.Set(ResourceIDHeader, resourceID)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("whip: POST request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("whip: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	answerBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("whip: read answer: %w", err)
	}

	// RFC 9725 §4.2: Location header points to the WHIP session URL.
	location := resp.Header.Get("Location")
	sessionURL := location
	if location != "" && !strings.HasPrefix(location, "http") {
		parsed, err := url.Parse(endpoint)
		if err == nil {
			sessionURL = parsed.Scheme + "://" + parsed.Host + location
		}
	}

	return &WHIPResult{
		AnswerSDP:    string(answerBytes),
		SessionURL:   sessionURL,
		ETag:         resp.Header.Get("ETag"),
		ResumeToken:  resp.Header.Get("X-Resume-Token"),
		ResumeStatus: resp.Header.Get("X-Resume-Status"),
	}, nil
}

// whipRestartICE sends an ICE restart to the session URL per RFC 9725 §4.4.2.
//
// etag is sent as If-Match so a restart racing another one is rejected rather
// than applied to a generation that no longer exists.
func whipRestartICE(sessionURL, fragment, etag, token string) (*WHIPRestartResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, sessionURL, strings.NewReader(fragment))
	if err != nil {
		return nil, fmt.Errorf("whip: create restart request: %w", err)
	}
	req.Header.Set("Content-Type", ICEFragmentContentType)
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("whip: PATCH request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, &WHIPRestartError{
			Status:      resp.StatusCode,
			Body:        string(body),
			CurrentETag: resp.Header.Get("ETag"),
		}
	}

	newETag := resp.Header.Get("ETag")
	if newETag == "" {
		newETag = etag
	}
	return &WHIPRestartResult{Fragment: string(body), ETag: newETag}, nil
}

// fetchToken POSTs to the given token endpoint and returns the JWT string.
// If apiKey is non-empty it is sent as a Bearer Authorization header.
//
// A non-empty resourceID goes in the body for the server to sign into the
// token. This call carries the API key, which is why it is trusted to say so.
func fetchToken(tokenURL, apiKey, resourceID string) (string, error) {
	var body io.Reader
	if resourceID != "" {
		payload, err := json.Marshal(map[string]string{"resource_id": resourceID})
		if err != nil {
			return "", fmt.Errorf("token request: %w", err)
		}
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequest(http.MethodPost, tokenURL, body)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token request: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("token response: %w", err)
	}
	return result.Token, nil
}

// whipDelete terminates a WHIP session per RFC 9725 §4.2:
// Send HTTP DELETE to the WHIP session URL.
func whipDelete(sessionURL, token string) {
	if sessionURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, sessionURL, nil)
	if err != nil {
		return
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// Best-effort teardown; connection may already be closed.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
