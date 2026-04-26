package streamcoreai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pion/webrtc/v4"
)

// newTestClient constructs a Client without going through Connect(), so the
// token-selection logic in Disconnect() can be exercised directly.
func newTestClient(cfg Config) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		config:        cfg.withDefaults(),
		ctx:           ctx,
		cancel:        cancel,
		status:        StatusConnected,
		RemoteTrackCh: make(chan *webrtc.TrackRemote, 1),
	}
}

func TestDisconnectTokenSelection(t *testing.T) {
	const staticToken = "static-jwt"
	const fetchedToken = "fetched-jwt"

	tests := []struct {
		name         string
		cfgToken     string
		useTokenURL  bool
		preCacheLast string // value to seed c.lastToken with (simulates post-Connect state)
		wantAuth     string // expected Authorization header on the DELETE
		// wantFetchHits is the number of times the token endpoint should be
		// hit during Disconnect (re-fetch fallback).
		wantFetchHits int32
	}{
		{
			name:          "static token only",
			cfgToken:      staticToken,
			preCacheLast:  staticToken, // Connect would have cached it
			wantAuth:      "Bearer " + staticToken,
			wantFetchHits: 0,
		},
		{
			name:          "token URL only — Connect captured, Disconnect reuses",
			useTokenURL:   true,
			preCacheLast:  fetchedToken, // captured during Connect
			wantAuth:      "Bearer " + fetchedToken,
			wantFetchHits: 0,
		},
		{
			name:          "both set — TokenURL value wins",
			cfgToken:      staticToken,
			useTokenURL:   true,
			preCacheLast:  fetchedToken, // Connect overwrote with fetched
			wantAuth:      "Bearer " + fetchedToken,
			wantFetchHits: 0,
		},
		{
			name:          "neither set — no Authorization header",
			preCacheLast:  "",
			wantAuth:      "",
			wantFetchHits: 0,
		},
		{
			name:          "cached token blank + TokenURL set — Disconnect re-fetches",
			useTokenURL:   true,
			preCacheLast:  "",
			wantAuth:      "Bearer " + fetchedToken,
			wantFetchHits: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var fetchHits int32
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&fetchHits, 1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"token": fetchedToken})
			}))
			defer tokenSrv.Close()

			gotAuthCh := make(chan string, 1)
			deleteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					t.Errorf("expected DELETE, got %s", r.Method)
				}
				select {
				case gotAuthCh <- r.Header.Get("Authorization"):
				default:
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer deleteSrv.Close()

			cfg := Config{
				WHIPEndpoint: "http://example.invalid/whip",
				Token:        tc.cfgToken,
			}
			if tc.useTokenURL {
				cfg.TokenURL = tokenSrv.URL
			}

			c := newTestClient(cfg)
			c.sessionURL = deleteSrv.URL
			c.lastToken = tc.preCacheLast

			c.Disconnect()

			var gotAuth string
			select {
			case gotAuth = <-gotAuthCh:
			default:
				t.Fatalf("DELETE was not called")
			}
			if gotAuth != tc.wantAuth {
				t.Errorf("Authorization header: got %q, want %q", gotAuth, tc.wantAuth)
			}
			if got := atomic.LoadInt32(&fetchHits); got != tc.wantFetchHits {
				t.Errorf("token endpoint hits: got %d, want %d", got, tc.wantFetchHits)
			}
			if c.lastToken != "" {
				t.Errorf("lastToken should be cleared after Disconnect, got %q", c.lastToken)
			}
			if c.sessionURL != "" {
				t.Errorf("sessionURL should be cleared after Disconnect, got %q", c.sessionURL)
			}
		})
	}
}

// Sanity check that fetchToken still parses {"token": "..."} responses.
func TestFetchToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer key-123" {
			t.Errorf("Authorization: got %q, want %q", got, "Bearer key-123")
		}
		_, _ = fmt.Fprint(w, `{"token":"abc"}`)
	}))
	defer srv.Close()

	tok, err := fetchToken(srv.URL, "key-123")
	if err != nil {
		t.Fatalf("fetchToken: %v", err)
	}
	if tok != "abc" {
		t.Errorf("token: got %q, want %q", tok, "abc")
	}
}
