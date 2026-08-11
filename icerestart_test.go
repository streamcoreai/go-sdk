package streamcoreai

import (
	"strings"
	"testing"
)

const testRemoteAnswer = "v=0\r\n" +
	"o=- 4611731400430051336 2 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"a=group:BUNDLE 0 1\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=mid:0\r\n" +
	"a=ice-ufrag:oldU\r\n" +
	"a=ice-pwd:oldPassword0000000000\r\n" +
	"a=fingerprint:sha-256 AA:BB:CC\r\n" +
	"a=candidate:1 1 udp 2130706431 192.0.2.10 41000 typ host\r\n" +
	"a=end-of-candidates\r\n" +
	"a=rtpmap:111 opus/48000/2\r\n" +
	"a=ssrc:12345 cname:stream\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
	"a=mid:1\r\n" +
	"a=ice-ufrag:oldU\r\n" +
	"a=ice-pwd:oldPassword0000000000\r\n"

const testServerFragment = "a=ice-ufrag:newU\r\n" +
	"a=ice-pwd:newPassword111111111\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
	"a=mid:0\r\n" +
	"a=candidate:1 1 udp 2130706431 198.51.100.1 39132 typ host\r\n"

func TestParseICEDetails(t *testing.T) {
	ufrag, pwd, candidates := parseICEDetails(testServerFragment)
	if ufrag != "newU" || pwd != "newPassword111111111" {
		t.Fatalf("credentials = %q / %q", ufrag, pwd)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	if !strings.HasPrefix(candidates[0], "1 1 udp") {
		t.Fatalf("candidate kept its a=candidate: prefix: %q", candidates[0])
	}
}

func TestParseICEDetailsLFOnly(t *testing.T) {
	ufrag, pwd, _ := parseICEDetails("a=ice-ufrag:newU\na=ice-pwd:pw\n")
	if ufrag != "newU" || pwd != "pw" {
		t.Fatalf("credentials = %q / %q", ufrag, pwd)
	}
}

func TestICEFragmentFromSDPUsesBundleMasterOnly(t *testing.T) {
	local := "v=0\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
		"a=mid:0\r\n" +
		"a=ice-ufrag:localU\r\n" +
		"a=ice-pwd:localPassword222222\r\n" +
		"a=candidate:1 1 udp 2130706431 198.51.100.7 51000 typ host\r\n" +
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
		"a=mid:1\r\n" +
		"a=candidate:9 1 udp 1 10.0.0.1 1 typ host\r\n"

	frag := iceFragmentFromSDP(local)

	for _, must := range []string{
		"a=ice-ufrag:localU",
		"a=ice-pwd:localPassword222222",
		"m=audio 9 UDP/TLS/RTP/SAVPF 111",
		"a=mid:0",
		"198.51.100.7",
		"a=end-of-candidates",
	} {
		if !strings.Contains(frag, must) {
			t.Fatalf("fragment missing %q:\n%s", must, frag)
		}
	}
	// Bundled sections are not described.
	if strings.Contains(frag, "m=application") || strings.Contains(frag, "10.0.0.1") {
		t.Fatalf("fragment leaked a bundled section:\n%s", frag)
	}
}

func TestApplyICEFragment(t *testing.T) {
	applied, ok := applyICEFragment(testRemoteAnswer, testServerFragment)
	if !ok {
		t.Fatal("applyICEFragment rejected a valid restart reply")
	}

	if strings.Contains(applied, "oldU") || strings.Contains(applied, "oldPassword0000000000") {
		t.Fatalf("old credentials survived:\n%s", applied)
	}
	// Both bundled sections must agree on the new credentials.
	if n := strings.Count(applied, "a=ice-ufrag:newU"); n != 2 {
		t.Fatalf("a=ice-ufrag:newU appears %d times, want 2:\n%s", n, applied)
	}
	if n := strings.Count(applied, "a=ice-pwd:newPassword111111111"); n != 2 {
		t.Fatalf("a=ice-pwd appears %d times, want 2:\n%s", n, applied)
	}

	if strings.Contains(applied, "192.0.2.10") {
		t.Fatalf("stale candidate survived:\n%s", applied)
	}
	if !strings.Contains(applied, "a=candidate:1 1 udp 2130706431 198.51.100.1 39132 typ host") {
		t.Fatalf("new candidate missing:\n%s", applied)
	}

	// Everything the transport is not responsible for survives verbatim.
	for _, must := range []string{
		"a=mid:0", "a=mid:1", "a=ssrc:12345 cname:stream",
		"a=fingerprint:sha-256 AA:BB:CC", "a=rtpmap:111 opus/48000/2",
		"a=group:BUNDLE 0 1", "m=application 9 UDP/DTLS/SCTP webrtc-datachannel",
	} {
		if !strings.Contains(applied, must) {
			t.Fatalf("rewrite dropped %q:\n%s", must, applied)
		}
	}

	// A new revision of the same session.
	if !strings.Contains(applied, "o=- 4611731400430051336 3 IN IP4 127.0.0.1") {
		t.Fatalf("origin version not bumped:\n%s", applied)
	}

	// Candidates belong to the first (bundle master) media section.
	audio := strings.Index(applied, "m=audio")
	app := strings.Index(applied, "m=application")
	candidate := strings.Index(applied, "a=candidate:")
	if candidate < audio || candidate > app {
		t.Fatalf("candidate landed outside the first media section:\n%s", applied)
	}
}

func TestApplyICEFragmentRejectsTrickleOnly(t *testing.T) {
	trickle := "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
		"a=candidate:1 1 udp 1 1.2.3.4 1 typ host\r\n"
	if _, ok := applyICEFragment(testRemoteAnswer, trickle); ok {
		t.Fatal("a credential-less fragment was accepted as a restart reply")
	}
}

func TestBumpSDPOrigin(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"increments the session version", "o=- 1 2 IN IP4 127.0.0.1", "o=- 1 3 IN IP4 127.0.0.1"},
		{"leaves a malformed origin alone", "o=- 123", "o=- 123"},
		{"leaves a non-numeric version alone", "o=- 1 abc IN IP4 127.0.0.1", "o=- 1 abc IN IP4 127.0.0.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bumpSDPOrigin(tc.in); got != tc.want {
				t.Fatalf("bumpSDPOrigin = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWHIPRestartErrorRetryable(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{404, false}, // session reaped
		{409, false}, // no peer to restart
		{405, false}, // server declines restarts
		{412, true},  // stale ETag — retry against the current one
		{500, true},  // transient server fault
		{429, true},  // rate limited
	}
	for _, tc := range tests {
		err := &WHIPRestartError{Status: tc.status}
		if got := err.Retryable(); got != tc.want {
			t.Fatalf("status %d: Retryable = %v, want %v", tc.status, got, tc.want)
		}
	}
}
