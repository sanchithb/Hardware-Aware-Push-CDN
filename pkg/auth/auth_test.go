package auth

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

const secret = "hps_test-secret"

func TestSignAndValidate(t *testing.T) {
	q := SignPath(secret, "/play/stream1/index.m3u8", "", time.Hour)
	vals, err := url.ParseQuery(q)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if err := ValidateQuery(secret, "/play/stream1/index.m3u8", vals, time.Now()); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	q := SignPath(secret, "/play/a.ts", "", time.Minute)
	vals, _ := url.ParseQuery(q)
	if err := ValidateQuery(secret, "/play/a.ts", vals, time.Now().Add(2*time.Minute)); err != ErrExpired {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestScopeCoversChildren(t *testing.T) {
	// One signature scoped to the stream dir must admit every segment.
	q := SignPath(secret, "/play/stream1/index.m3u8", "/play/stream1/", time.Hour)
	vals, _ := url.ParseQuery(q)
	for _, p := range []string{"/play/stream1/index.m3u8", "/play/stream1/seg42.ts", "/play/stream1/sub/init.mp4"} {
		if err := ValidateQuery(secret, p, vals, time.Now()); err != nil {
			t.Errorf("path %s should be in scope: %v", p, err)
		}
	}
	if err := ValidateQuery(secret, "/play/other/seg.ts", vals, time.Now()); err != ErrOutOfScope {
		t.Errorf("out-of-scope path admitted: %v", err)
	}
}

func TestTamperedSignature(t *testing.T) {
	q := SignPath(secret, "/play/a.ts", "", time.Hour)
	vals, _ := url.ParseQuery(q)

	if err := ValidateQuery(secret, "/play/b.ts", vals, time.Now()); err != ErrBadSignature {
		t.Errorf("different path must fail: %v", err)
	}
	if err := ValidateQuery("hps_other", "/play/a.ts", vals, time.Now()); err != ErrBadSignature {
		t.Errorf("different key must fail: %v", err)
	}
	bad := url.Values{}
	for k, v := range vals {
		bad[k] = v
	}
	bad.Set(ParamSig, vals.Get(ParamSig)+"x")
	if err := ValidateQuery(secret, "/play/a.ts", bad, time.Now()); err != ErrBadSignature {
		t.Errorf("mutated signature must fail: %v", err)
	}
	// Extending expiry without re-signing must fail.
	bad2 := url.Values{}
	for k, v := range vals {
		bad2[k] = v
	}
	bad2.Set(ParamExpires, "9999999999")
	if err := ValidateQuery(secret, "/play/a.ts", bad2, time.Now()); err != ErrBadSignature {
		t.Errorf("expiry extension must fail: %v", err)
	}
}

func TestMissingAndMalformed(t *testing.T) {
	if err := ValidateQuery(secret, "/p", url.Values{}, time.Now()); err != ErrMissingSignature {
		t.Errorf("missing sig: %v", err)
	}
	v := url.Values{ParamSig: {"abc"}, ParamExpires: {"not-a-number"}}
	if err := ValidateQuery(secret, "/p", v, time.Now()); err != ErrMalformed {
		t.Errorf("malformed exp: %v", err)
	}
}

func TestTokenPrefixesAndUniqueness(t *testing.T) {
	a, b := NewToken(PrefixAdminKey), NewToken(PrefixAdminKey)
	if a == b {
		t.Fatal("tokens must be unique")
	}
	if !strings.HasPrefix(a, "hpa_") {
		t.Fatalf("prefix missing: %s", a)
	}
}

func TestSignURLAppendsToExistingQuery(t *testing.T) {
	u, err := SignURL(secret, "https://cdn.example.com/play/s/x.ts?foo=1", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	if parsed.Query().Get("foo") != "1" {
		t.Error("existing query dropped")
	}
	if err := ValidateQuery(secret, "/play/s/x.ts", parsed.Query(), time.Now()); err != nil {
		t.Errorf("signed URL invalid: %v", err)
	}
}
