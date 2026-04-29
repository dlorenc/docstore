package archivesign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
)

// Sign returns a hex-encoded HMAC-SHA256 signature over the canonical message
// "repo\nbranch\nsequence\nexpires" (newline-separated to prevent ambiguity).
func Sign(secret []byte, repo, branch string, sequence, expiresUnix int64) string {
	msg := canonicalMessage(repo, branch, sequence, expiresUnix)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether sig is a valid HMAC for the given parameters.
// Uses hmac.Equal for constant-time comparison.
func Verify(secret []byte, repo, branch string, sequence, expiresUnix int64, sig string) bool {
	expected := Sign(secret, repo, branch, sequence, expiresUnix)
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	exp, _ := hex.DecodeString(expected)
	return hmac.Equal(exp, got)
}

func canonicalMessage(repo, branch string, sequence, expiresUnix int64) string {
	return fmt.Sprintf("%s\n%s\n%s\n%s",
		repo, branch,
		strconv.FormatInt(sequence, 10),
		strconv.FormatInt(expiresUnix, 10),
	)
}
