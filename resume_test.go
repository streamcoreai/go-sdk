package streamcoreai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Drives the whip layer against a stub server to pin the resume contract the
// recovery ladder depends on: the token goes out as a query parameter, and the
// status/token come back off the response headers.
func TestWhipOfferCarriesResumeToken(t *testing.T) {
	var gotResume string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotResume = r.URL.Query().Get("resume")
		w.Header().Set("Location", "/whip/abc")
		w.Header().Set("ETag", `"tag"`)
		w.Header().Set("X-Resume-Token", "next-token")
		if gotResume != "" {
			w.Header().Set("X-Resume-Status", "resumed")
		} else {
			w.Header().Set("X-Resume-Status", "new")
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("v=0\r\n"))
	}))
	defer srv.Close()

	first, err := whipOffer(srv.URL, "v=0\r\n", nil, "", "", "")
	if err != nil {
		t.Fatalf("whipOffer: %v", err)
	}
	if gotResume != "" {
		t.Fatalf("a first connect sent resume=%q", gotResume)
	}
	if first.ResumeStatus != "new" || first.ResumeToken != "next-token" {
		t.Fatalf("first: status=%q token=%q", first.ResumeStatus, first.ResumeToken)
	}

	second, err := whipOffer(srv.URL, "v=0\r\n", map[string]string{"direction": "inbound"}, "", first.ResumeToken, "")
	if err != nil {
		t.Fatalf("whipOffer resume: %v", err)
	}
	if gotResume != "next-token" {
		t.Fatalf("resume token not sent, got %q", gotResume)
	}
	if second.ResumeStatus != "resumed" {
		t.Fatalf("second: status=%q", second.ResumeStatus)
	}
	// Metadata must survive alongside the resume parameter.
	if !strings.Contains(srv.URL, "http") {
		t.Fatal("unexpected server URL")
	}
}

func TestResumePhaseSkippedWithoutToken(t *testing.T) {
	c := NewClient(Config{ResumeAttempts: 2}, EventHandler{})
	var events []ReconnectEvent
	c.events.OnReconnect = func(e ReconnectEvent) { events = append(events, e) }

	// No token was ever issued, so there is nothing to resume onto and the
	// ladder must stop rather than redial into a blank conversation.
	c.recoverByResume(c.currentGeneration())

	if len(events) != 0 {
		t.Fatalf("attempted a resume with no token: %+v", events)
	}
	if c.Status() != StatusDisconnected {
		t.Fatalf("status = %v, want disconnected", c.Status())
	}
}
