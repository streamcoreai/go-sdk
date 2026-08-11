package streamcoreai

import (
	"strconv"
	"strings"
)

// ICEFragmentContentType is the media type of an ICE restart body
// (RFC 8840, RFC 9725 §4.4).
const ICEFragmentContentType = "application/trickle-ice-sdpfrag"

// ICE restart support.
//
// When the local address changes — a machine moving networks, a VPN toggle, a
// NAT rebind that does not recover — the gathered candidates are dead and the
// connection cannot heal on its own. Re-POSTing an offer would allocate a new
// session on the server, losing the conversation history and replaying the
// greeting. An ICE restart instead swaps only the ICE generation on the
// existing session, so nothing above the transport notices.
//
// The wire format is an SDP fragment rather than a full description, so these
// helpers translate both ways: the new local offer becomes a fragment to
// PATCH, and the server's reply is folded back into the answer already held.

// iceFragmentFromSDP renders a local offer as the sdpfrag to PATCH: the
// credentials, then the bundle-master m-line with its mid and candidates.
// Shaped after the request example in RFC 9725 §4.4.2.
func iceFragmentFromSDP(localSDP string) string {
	var (
		ufrag, pwd string
		mLine, mid string
		candidates []string
		mediaIndex = -1
	)

	for _, line := range splitSDPLines(localSDP) {
		if strings.HasPrefix(line, "m=") {
			mediaIndex++
			if mediaIndex == 0 {
				mLine = line
			}
			continue
		}
		// Session-level attributes and the first media section's are both
		// usable; later sections are bundled onto the first.
		if mediaIndex > 0 {
			continue
		}
		switch {
		case strings.HasPrefix(line, "a=ice-ufrag:"):
			if ufrag == "" {
				ufrag = strings.TrimPrefix(line, "a=ice-ufrag:")
			}
		case strings.HasPrefix(line, "a=ice-pwd:"):
			if pwd == "" {
				pwd = strings.TrimPrefix(line, "a=ice-pwd:")
			}
		case strings.HasPrefix(line, "a=mid:"):
			if mid == "" {
				mid = strings.TrimPrefix(line, "a=mid:")
			}
		case strings.HasPrefix(line, "a=candidate:"):
			candidates = append(candidates, strings.TrimPrefix(line, "a=candidate:"))
		}
	}

	out := make([]string, 0, len(candidates)+5)
	if ufrag != "" {
		out = append(out, "a=ice-ufrag:"+ufrag)
	}
	if pwd != "" {
		out = append(out, "a=ice-pwd:"+pwd)
	}
	if mLine != "" {
		out = append(out, mLine)
	}
	if mid != "" {
		out = append(out, "a=mid:"+mid)
	}
	for _, c := range candidates {
		out = append(out, "a=candidate:"+c)
	}
	out = append(out, "a=end-of-candidates")

	return strings.Join(out, "\r\n") + "\r\n"
}

// applyICEFragment folds the server's reply fragment into the answer already
// held, producing a full SDP that SetRemoteDescription accepts.
//
// Only the ICE generation changes: credentials are replaced wherever they
// appear, stale candidates are dropped, and the new ones are inserted into the
// first media section. Everything else — m-lines, payload types, the DTLS
// fingerprint, SSRCs — is carried over verbatim, because a restart is not
// meant to renegotiate any of it.
//
// Returns ok=false if the fragment carries no credentials, which would mean it
// is not a restart reply at all.
func applyICEFragment(remoteSDP, fragment string) (string, bool) {
	ufrag, pwd, candidates := parseICEDetails(fragment)
	if ufrag == "" || pwd == "" {
		return "", false
	}

	lines := splitSDPLines(remoteSDP)
	out := make([]string, 0, len(lines)+len(candidates)+1)

	mediaIndex := -1
	inserted := false
	insertCandidates := func() {
		if inserted {
			return
		}
		inserted = true
		for _, c := range candidates {
			out = append(out, "a=candidate:"+c)
		}
		if len(candidates) > 0 {
			out = append(out, "a=end-of-candidates")
		}
	}

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "m="):
			// Leaving the first media section — the new candidates belong at
			// its end, after the attributes it already carries.
			if mediaIndex == 0 {
				insertCandidates()
			}
			mediaIndex++
			out = append(out, line)
		case strings.HasPrefix(line, "o="):
			out = append(out, bumpSDPOrigin(line))
		case strings.HasPrefix(line, "a=ice-ufrag:"):
			out = append(out, "a=ice-ufrag:"+ufrag)
		case strings.HasPrefix(line, "a=ice-pwd:"):
			out = append(out, "a=ice-pwd:"+pwd)
		case strings.HasPrefix(line, "a=candidate:"), line == "a=end-of-candidates":
			// Previous ICE generation — dropped.
		default:
			out = append(out, line)
		}
	}
	insertCandidates()

	return strings.Join(out, "\r\n") + "\r\n", true
}

// parseICEDetails reads the ICE credentials and candidates out of an SDP or
// fragment. Credentials may sit at session or media level; the first of each
// wins, which is what a bundled description means anyway.
func parseICEDetails(sdp string) (ufrag, pwd string, candidates []string) {
	for _, line := range splitSDPLines(sdp) {
		switch {
		case strings.HasPrefix(line, "a=ice-ufrag:"):
			if ufrag == "" {
				ufrag = strings.TrimPrefix(line, "a=ice-ufrag:")
			}
		case strings.HasPrefix(line, "a=ice-pwd:"):
			if pwd == "" {
				pwd = strings.TrimPrefix(line, "a=ice-pwd:")
			}
		case strings.HasPrefix(line, "a=candidate:"):
			candidates = append(candidates, strings.TrimPrefix(line, "a=candidate:"))
		}
	}
	return ufrag, pwd, candidates
}

// bumpSDPOrigin increments the session version in an `o=` line, which is how
// JSEP marks a description as a new revision of the same session.
func bumpSDPOrigin(line string) string {
	fields := strings.Fields(strings.TrimPrefix(line, "o="))
	if len(fields) < 6 {
		return line
	}
	version, err := strconv.ParseUint(fields[2], 10, 64)
	if err != nil {
		return line
	}
	fields[2] = strconv.FormatUint(version+1, 10)
	return "o=" + strings.Join(fields, " ")
}

// splitSDPLines splits an SDP or fragment into lines, tolerating LF-only
// bodies and dropping the blank line a trailing CRLF leaves behind.
func splitSDPLines(sdp string) []string {
	raw := strings.Split(sdp, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}
