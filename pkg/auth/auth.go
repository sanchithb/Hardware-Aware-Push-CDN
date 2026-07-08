// Package auth implements the security primitives shared across hpcdn:
//
//   - HMAC-SHA256 signed playback URLs with expiry and optional path scoping
//     (the scheme CloudFront custom policies, Cloudflare token auth and
//     Fastly VCL token auth all converge on), validated with a
//     constant-time compare.
//   - Prefixed random API credentials (admin keys, join tokens, node
//     secrets) in the style of modern SaaS tokens, so a leaked string is
//     immediately identifiable.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Token prefixes make credentials self-describing and grep-able in logs.
const (
	PrefixAdminKey   = "hpa_" // administrator API key
	PrefixJoinToken  = "hpj_" // node join token (enrollment)
	PrefixNodeSecret = "hpn_" // per-node secret issued at registration
	PrefixSigningKey = "hps_" // cluster URL-signing key
	PrefixIngestKey  = "hpi_" // cluster ingest/replication key
)

// Signed URL query parameters.
const (
	ParamExpires = "hpe" // unix seconds, UTC
	ParamScope   = "hps" // optional path prefix the signature covers
	ParamSig     = "hpx" // base64url(HMAC-SHA256)
)

// Common validation errors. Callers should not expose which one failed to
// end users; they exist for logging and metrics.
var (
	ErrMissingSignature = errors.New("auth: missing signature")
	ErrExpired          = errors.New("auth: url expired")
	ErrBadSignature     = errors.New("auth: signature mismatch")
	ErrOutOfScope       = errors.New("auth: path outside signed scope")
	ErrMalformed        = errors.New("auth: malformed signature params")
)

// NewToken returns a new random credential with the given prefix.
// 32 bytes of entropy, base64url without padding.
func NewToken(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure means the platform RNG is broken; nothing
		// sensible can continue.
		panic(fmt.Sprintf("auth: crypto/rand unavailable: %v", err))
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func mac(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// signPayload builds the canonical string covered by the signature.
// scope is the path prefix the signature grants access to.
func signPayload(scope string, expires int64) string {
	return scope + "\n" + strconv.FormatInt(expires, 10)
}

// SignPath signs a URL path with the cluster signing key.
//
// If scope is empty the signature covers exactly path. If scope is a
// non-empty prefix (e.g. "/play/stream1/"), one signed query string is
// valid for every path under it — this is what lets a player fetch every
// segment of a stream with the token minted for its playlist.
//
// The returned value is a query string fragment: "hpe=...&hps=...&hpx=...".
func SignPath(secret, path, scope string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	effective := scope
	if effective == "" {
		effective = path
	}
	v := url.Values{}
	v.Set(ParamExpires, strconv.FormatInt(exp, 10))
	if scope != "" {
		v.Set(ParamScope, scope)
	}
	v.Set(ParamSig, mac(secret, signPayload(effective, exp)))
	return v.Encode()
}

// SignURL is a convenience that appends a signature to a full or relative URL.
func SignURL(secret, rawURL, scope string, ttl time.Duration) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := SignPath(secret, u.Path, scope, ttl)
	if u.RawQuery == "" {
		u.RawQuery = q
	} else {
		u.RawQuery += "&" + q
	}
	return u.String(), nil
}

// ValidateQuery checks the signature parameters in query against path.
// now allows deterministic tests; pass time.Now() in production code.
func ValidateQuery(secret, path string, query url.Values, now time.Time) error {
	sig := query.Get(ParamSig)
	if sig == "" {
		return ErrMissingSignature
	}
	expStr := query.Get(ParamExpires)
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return ErrMalformed
	}
	if now.Unix() > exp {
		return ErrExpired
	}
	scope := query.Get(ParamScope)
	effective := path
	if scope != "" {
		if !strings.HasPrefix(path, scope) {
			return ErrOutOfScope
		}
		effective = scope
	}
	want := mac(secret, signPayload(effective, exp))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return ErrBadSignature
	}
	return nil
}

// SignatureParams extracts just the signature parameters from a query so
// they can be re-appended when rewriting playlist URIs.
func SignatureParams(query url.Values) url.Values {
	out := url.Values{}
	for _, k := range []string{ParamExpires, ParamScope, ParamSig} {
		if v := query.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	return out
}
