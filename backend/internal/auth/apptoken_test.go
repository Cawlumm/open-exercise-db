package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestGenerateAppCredential_RoundTrips(t *testing.T) {
	keyID, token, hash, err := GenerateAppCredential()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	parsedID, secret, err := ParseToken(token)
	if err != nil {
		t.Fatalf("parse own token: %v", err)
	}
	if parsedID != keyID {
		t.Errorf("parsed key id = %q, want %q", parsedID, keyID)
	}
	if !VerifySecret(secret, hash) {
		t.Error("freshly generated secret does not verify against its own hash")
	}
}

func TestGenerateAppCredential_IsUnique(t *testing.T) {
	// A collision here would mean two apps sharing a credential, so this is
	// worth asserting rather than assuming crypto/rand behaves.
	seen := make(map[string]bool, 50)
	for range 50 {
		keyID, _, _, err := GenerateAppCredential()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if seen[keyID] {
			t.Fatalf("duplicate key id generated: %q", keyID)
		}
		seen[keyID] = true
	}
}

func TestVerifySecret_RejectsWrongSecret(t *testing.T) {
	_, token, hash, err := GenerateAppCredential()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	_, secret, _ := ParseToken(token)

	// The real one verifies...
	if !VerifySecret(secret, hash) {
		t.Fatal("correct secret rejected")
	}
	// ...and near-misses do not. A truncated or extended secret is what a
	// copy-paste error produces, so both are worth covering explicitly.
	for name, wrong := range map[string]string{
		"empty":       "",
		"truncated":   secret[:len(secret)-1],
		"extra char":  secret + "x",
		"other's key": mustSecret(t),
	} {
		if VerifySecret(wrong, hash) {
			t.Errorf("%s secret verified against the wrong hash", name)
		}
	}
}

// mustSecret returns the secret half of an unrelated fresh credential.
func mustSecret(t *testing.T) string {
	t.Helper()
	_, token, _, err := GenerateAppCredential()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	_, secret, err := ParseToken(token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return secret
}

func TestParseToken_Malformed(t *testing.T) {
	// Every malformation must produce the same error: telling a caller which
	// half of their token is wrong tells an attacker which half is right.
	for name, tok := range map[string]string{
		"empty":            "",
		"no prefix":        "abc.def",
		"wrong prefix":     "oedb-abc.def",
		"prefix only":      "oedb_",
		"no separator":     "oedb_abcdef",
		"empty key id":     "oedb_.def",
		"empty secret":     "oedb_abc.",
		"separator only":   "oedb_.",
		"bearer not token": "Bearer oedb_abc.def",
	} {
		if _, _, err := ParseToken(tok); !errors.Is(err, ErrMalformedToken) {
			t.Errorf("%s: err = %v, want ErrMalformedToken", name, err)
		}
	}
}

func TestParseToken_SecretMayContainNoSeparator(t *testing.T) {
	// The dot is safe as a separator only because both halves are base64url,
	// whose alphabet excludes it. If either encoding ever changes, this is
	// the test that should fail.
	keyID, token, _, err := GenerateAppCredential()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if strings.Count(token, tokenSep) != 1 {
		t.Fatalf("token contains %d separators, want exactly 1: %q",
			strings.Count(token, tokenSep), token)
	}
	if strings.ContainsAny(keyID, ".+/=") {
		t.Errorf("key id contains a non-base64url character: %q", keyID)
	}
}

func TestHashSecret_IsStableAndNotTheSecret(t *testing.T) {
	const secret = "a-known-secret"
	first := HashSecret(secret)

	if first != HashSecret(secret) {
		t.Error("HashSecret is not deterministic")
	}
	if strings.Contains(first, secret) {
		t.Error("hash contains the secret")
	}
	if len(first) != 64 { // sha256 hex
		t.Errorf("hash length = %d, want 64", len(first))
	}
}
