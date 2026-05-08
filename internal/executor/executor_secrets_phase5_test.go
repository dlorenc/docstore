package executor

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// TestBuildSecretMapOIDCOnly: an OIDC token with no user secrets yields the
// two built-in keys and nothing else.
func TestBuildSecretMapOIDCOnly(t *testing.T) {
	got := buildSecretMap("tok", "https://oidc.example", nil)
	want := map[string][]byte{
		"docstore_oidc_request_token": []byte("tok"),
		"docstore_oidc_request_url":   []byte("https://oidc.example"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestBuildSecretMapUserOnly: with no OIDC token but user secrets present,
// the map contains exactly those LocalName keys.
func TestBuildSecretMapUserOnly(t *testing.T) {
	user := map[string][]byte{
		"DOCKERHUB_TOKEN": []byte("dh-secret"),
		"SLACK_WEBHOOK":   []byte("https://hooks.slack/x"),
	}
	got := buildSecretMap("", "", user)
	if !reflect.DeepEqual(got, user) {
		t.Errorf("got %v, want %v", got, user)
	}
	// Defensive: built-in OIDC keys must not appear when oidcToken is empty.
	for _, k := range []string{"docstore_oidc_request_token", "docstore_oidc_request_url"} {
		if _, ok := got[k]; ok {
			t.Errorf("unexpected built-in key %q present when OIDC unset", k)
		}
	}
}

// TestBuildSecretMapBoth: both sets coexist with no collisions.
func TestBuildSecretMapBoth(t *testing.T) {
	user := map[string][]byte{
		"DOCKERHUB_TOKEN": []byte("dh-secret"),
	}
	got := buildSecretMap("tok", "https://oidc.example", user)
	want := map[string][]byte{
		"docstore_oidc_request_token": []byte("tok"),
		"docstore_oidc_request_url":   []byte("https://oidc.example"),
		"DOCKERHUB_TOKEN":             []byte("dh-secret"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestBuildSecretMapEmptyUserSecretsEqualsOIDCOnly: an empty (but non-nil)
// userSecrets behaves identically to nil.
func TestBuildSecretMapEmptyUserSecretsEqualsOIDCOnly(t *testing.T) {
	gotNil := buildSecretMap("tok", "u", nil)
	gotEmpty := buildSecretMap("tok", "u", map[string][]byte{})
	if !reflect.DeepEqual(gotNil, gotEmpty) {
		t.Errorf("nil vs empty differ: nil=%v empty=%v", gotNil, gotEmpty)
	}
}

// TestBuildSecretMapNothingReturnsNil: with neither OIDC nor user secrets,
// the function returns nil so callers can skip attaching a secrets provider.
func TestBuildSecretMapNothingReturnsNil(t *testing.T) {
	if got := buildSecretMap("", "", nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	if got := buildSecretMap("", "", map[string][]byte{}); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

// TestScrubLogsEmptySecrets: nil/empty secret map returns input unchanged.
func TestScrubLogsEmptySecrets(t *testing.T) {
	in := []byte("hello world")
	if got := scrubLogs(in, nil); !bytes.Equal(got, in) {
		t.Errorf("nil secrets: got %q, want %q", got, in)
	}
	if got := scrubLogs(in, map[string][]byte{}); !bytes.Equal(got, in) {
		t.Errorf("empty secrets: got %q, want %q", got, in)
	}
}

// TestScrubLogsSingleOccurrence: a value appearing once is replaced.
func TestScrubLogsSingleOccurrence(t *testing.T) {
	in := []byte("token=hunter2 done")
	got := scrubLogs(in, map[string][]byte{"FOO": []byte("hunter2")})
	want := []byte("token=*** done")
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestScrubLogsMultipleOccurrences: every occurrence of one value is replaced.
func TestScrubLogsMultipleOccurrences(t *testing.T) {
	in := []byte("a hunter2 b hunter2 c hunter2")
	got := scrubLogs(in, map[string][]byte{"FOO": []byte("hunter2")})
	want := []byte("a *** b *** c ***")
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestScrubLogsTwoSecrets: distinct secrets are both scrubbed.
func TestScrubLogsTwoSecrets(t *testing.T) {
	in := []byte("user=hunter2 webhook=https://hooks.example/abc")
	got := scrubLogs(in, map[string][]byte{
		"PW":      []byte("hunter2"),
		"WEBHOOK": []byte("https://hooks.example/abc"),
	})
	if bytes.Contains(got, []byte("hunter2")) {
		t.Errorf("hunter2 still present in %q", got)
	}
	if bytes.Contains(got, []byte("hooks.example")) {
		t.Errorf("webhook still present in %q", got)
	}
	if !bytes.Contains(got, []byte("***")) {
		t.Errorf("no *** marker in %q", got)
	}
}

// TestScrubLogsEmptyValueDoesNotCorrupt: an empty []byte in the map must be
// skipped — replacing "" would inject *** at every byte boundary.
func TestScrubLogsEmptyValueDoesNotCorrupt(t *testing.T) {
	in := []byte("plain text")
	got := scrubLogs(in, map[string][]byte{
		"EMPTY":  nil,
		"EMPTY2": []byte(""),
		"REAL":   []byte("text"),
	})
	want := []byte("plain ***")
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	// Stronger guard: assert *** count is exactly 1 (not 11).
	if n := strings.Count(string(got), "***"); n != 1 {
		t.Errorf("expected exactly 1 ***, got %d in %q", n, got)
	}
}

// TestScrubLogsBinarySecret: secrets containing NUL bytes are handled by
// bytes.ReplaceAll like any other content.
func TestScrubLogsBinarySecret(t *testing.T) {
	secret := []byte{0x00, 0x01, 0x02, 'A', 'B'}
	in := append([]byte("prefix "), secret...)
	in = append(in, []byte(" suffix")...)
	got := scrubLogs(in, map[string][]byte{"BIN": secret})
	want := []byte("prefix *** suffix")
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestScrubLogsAbsentSecret: a value not present in the buffer is a no-op
// and the buffer comes back byte-equal to the input.
func TestScrubLogsAbsentSecret(t *testing.T) {
	in := []byte("nothing to scrub here")
	got := scrubLogs(in, map[string][]byte{"FOO": []byte("hunter2")})
	if !bytes.Equal(got, in) {
		t.Errorf("got %q, want %q", got, in)
	}
}

// TestScrubLogsShortValue: even a single-character secret gets replaced.
// This is intentional — the user wrote it as a secret and we honor that.
func TestScrubLogsShortValue(t *testing.T) {
	in := []byte("xxxxx")
	got := scrubLogs(in, map[string][]byte{"X": []byte("x")})
	want := []byte("***************") // 5 * len("***")
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestScrubLogsRedundantValues: when two LocalNames map to the same plaintext
// the bytes are scrubbed once (and the second pass is a no-op). Outcome
// matches the single-occurrence case.
func TestScrubLogsRedundantValues(t *testing.T) {
	in := []byte("token=hunter2 done")
	got := scrubLogs(in, map[string][]byte{
		"A": []byte("hunter2"),
		"B": []byte("hunter2"),
	})
	want := []byte("token=*** done")
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
