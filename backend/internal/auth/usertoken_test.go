package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const testSecret = "test-secret-not-used-anywhere-real"

var testNow = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

func TestUserToken_RoundTrips(t *testing.T) {
	tok, err := MintUserToken(testSecret, "user-123", testNow)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	claims, err := VerifyUserToken(testSecret, tok, testNow)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "user-123" {
		t.Errorf("sub = %q, want user-123", claims.Subject)
	}
	if claims.ExpiresAt <= claims.IssuedAt {
		t.Error("exp is not after iat")
	}
}

// The classic JWT attacks. Each of these is a documented way real
// implementations have been broken; each must fail here.

func TestUserToken_AlgNoneIsRejected(t *testing.T) {
	// The canonical JWT vulnerability: forge a token declaring no algorithm
	// and an empty signature, hoping the verifier honours the header.
	header := b64([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := b64(mustJSON(t, Claims{
		Subject: "attacker", IssuedAt: testNow.Unix(), ExpiresAt: testNow.Add(time.Hour).Unix(),
	}))

	for _, forged := range []string{
		header + "." + payload + ".",
		header + "." + payload + "." + b64([]byte("")),
	} {
		if _, err := VerifyUserToken(testSecret, forged, testNow); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("alg=none token accepted (err = %v)", err)
		}
	}
}

func TestUserToken_TamperedPayloadIsRejected(t *testing.T) {
	tok, err := MintUserToken(testSecret, "user-123", testNow)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	parts := strings.Split(tok, ".")

	// Swap the subject while keeping the original signature — privilege
	// escalation by editing the payload.
	parts[1] = b64(mustJSON(t, Claims{
		Subject: "someone-else", IssuedAt: testNow.Unix(), ExpiresAt: testNow.Add(time.Hour).Unix(),
	}))
	if _, err := VerifyUserToken(testSecret, strings.Join(parts, "."), testNow); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("tampered payload accepted (err = %v)", err)
	}
}

func TestUserToken_WrongSecretIsRejected(t *testing.T) {
	tok, err := MintUserToken(testSecret, "user-123", testNow)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := VerifyUserToken("a-different-secret", tok, testNow); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("token verified under the wrong secret (err = %v)", err)
	}
}

func TestUserToken_ExpiryIsEnforced(t *testing.T) {
	tok, err := MintUserToken(testSecret, "user-123", testNow)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Valid just before, invalid at and after the boundary.
	if _, err := VerifyUserToken(testSecret, tok, testNow.Add(tokenTTL-time.Second)); err != nil {
		t.Errorf("token rejected while still valid: %v", err)
	}
	if _, err := VerifyUserToken(testSecret, tok, testNow.Add(tokenTTL)); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("token accepted exactly at expiry (err = %v)", err)
	}
	if _, err := VerifyUserToken(testSecret, tok, testNow.Add(30*24*time.Hour)); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("long-expired token accepted (err = %v)", err)
	}
}

func TestUserToken_MissingExpiryIsRejected(t *testing.T) {
	// A correctly-signed token with no exp claim. Treating an absent expiry
	// as "never expires" is the failure mode; it is also what a future bug in
	// MintUserToken would produce, so this guards both.
	header := b64([]byte(jwtHeaderHS256))
	payload := b64([]byte(`{"sub":"user-123","iat":1}`))
	signing := header + "." + payload
	forged := signing + "." + b64(sign(testSecret, signing))

	if _, err := VerifyUserToken(testSecret, forged, testNow); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("token without exp accepted (err = %v)", err)
	}
}

func TestUserToken_EmptySubjectIsRejected(t *testing.T) {
	header := b64([]byte(jwtHeaderHS256))
	payload := b64(mustJSON(t, Claims{
		Subject: "", IssuedAt: testNow.Unix(), ExpiresAt: testNow.Add(time.Hour).Unix(),
	}))
	signing := header + "." + payload
	forged := signing + "." + b64(sign(testSecret, signing))

	// A signed token naming nobody would otherwise resolve to a zero-value
	// user id and hit the database as a lookup for "".
	if _, err := VerifyUserToken(testSecret, forged, testNow); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("token with empty subject accepted (err = %v)", err)
	}
}

func TestUserToken_MalformedShapes(t *testing.T) {
	for name, tok := range map[string]string{
		"empty":          "",
		"one part":       "abc",
		"two parts":      "abc.def",
		"four parts":     "a.b.c.d",
		"bad base64 sig": "a.b.!!!",
		"app token":      "oedb_keyid.secret",
	} {
		if _, err := VerifyUserToken(testSecret, tok, testNow); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}
}

func TestUserToken_NoSecretIsAConfigError(t *testing.T) {
	// Distinct from ErrInvalidToken: an operator must be able to tell "the
	// service is misconfigured" from "this user sent a bad token".
	if _, err := MintUserToken("", "user-123", testNow); !errors.Is(err, ErrNoSecret) {
		t.Errorf("mint without secret: err = %v, want ErrNoSecret", err)
	}
	if _, err := VerifyUserToken("", "a.b.c", testNow); !errors.Is(err, ErrNoSecret) {
		t.Errorf("verify without secret: err = %v, want ErrNoSecret", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// Guard against the signature being decodable-but-wrong in a way that panics
// rather than erroring.
func TestUserToken_SignatureLengthVariations(t *testing.T) {
	tok, err := MintUserToken(testSecret, "user-123", testNow)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	parts := strings.Split(tok, ".")
	for name, sig := range map[string]string{
		"empty":     "",
		"truncated": parts[2][:len(parts[2])-4],
		"extended":  parts[2] + base64.RawURLEncoding.EncodeToString([]byte("xx")),
	} {
		if _, err := VerifyUserToken(testSecret, parts[0]+"."+parts[1]+"."+sig, testNow); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s signature accepted (err = %v)", name, err)
		}
	}
}
