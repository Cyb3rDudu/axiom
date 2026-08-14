// Package sourceurl builds and verifies the HMAC-signed source-download URLs
// the dispatcher hands to a processor (contract §3 remote source transport).
//
// Signature covers jobID|exp (unix seconds). The secret is shared between
// dispatcher (signer) and server endpoint (verifier) via
// AXIOMNG_PROCESSOR_SOURCE_SECRET; an empty secret disables the feature
// everywhere (endpoint 404s, dispatcher sends no source_url).
package sourceurl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Sign returns the hex HMAC-SHA256 over "jobID|exp".
func Sign(secret, jobID string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s|%d", jobID, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether sig matches the expected HMAC for jobID|exp.
// Constant-time via hmac.Equal.
func Verify(secret, jobID string, exp int64, sig string) bool {
	want := Sign(secret, jobID, exp)
	return hmac.Equal([]byte(want), []byte(sig))
}

// BuildURL assembles <base>/api/processor/source/<jobID>?exp=<unix>&sig=<hmac>.
func BuildURL(base, jobID string, exp int64, sig string) string {
	q := url.Values{}
	q.Set("exp", strconv.FormatInt(exp, 10))
	q.Set("sig", sig)
	base = strings.TrimSuffix(base, "/")
	return base + "/api/processor/source/" + url.PathEscape(jobID) + "?" + q.Encode()
}
