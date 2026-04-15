package streamcoreai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WHIPResult holds the response from a WHIP signaling exchange.
type WHIPResult struct {
	AnswerSDP  string
	SessionURL string
}

// whipOffer performs a WHIP signaling exchange per RFC 9725 §4.2:
// POST an SDP offer, receive a 201 Created with SDP answer and Location header.
func whipOffer(endpoint, offerSDP string, metadata map[string]string, token string) (*WHIPResult, error) {
	// Append metadata as query parameters.
	if len(metadata) > 0 {
		parsed, err := url.Parse(endpoint)
		if err == nil {
			q := parsed.Query()
			for k, v := range metadata {
				q.Set(k, v)
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
		AnswerSDP:  string(answerBytes),
		SessionURL: sessionURL,
	}, nil
}

// fetchToken POSTs to the given token endpoint and returns the JWT string.
// If apiKey is non-empty it is sent as a Bearer Authorization header.
func fetchToken(tokenURL, apiKey string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, tokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
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
