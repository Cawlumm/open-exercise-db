package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// User session tokens: HS256 JWTs, minted and verified only by this service.
//
// Hand-rolled against crypto/hmac rather than taking a JWT dependency,
// because the requirement here is narrow enough that the library's value —
// handling algorithms, key formats, and third-party issuers we will never
// see — does not apply. This token is effectively a signed cookie: we are
// both the issuer and the only audience, there is one algorithm, and there is
// one symmetric key.
//
// The classic JWT vulnerabilities are closed by construction rather than by
// configuration, which is the reason this is safe to hand-roll at all:
//
//   - "alg": "none" — the header is never consulted when verifying. This code
//     computes the HS256 signature it expects and compares; a token claiming
//     any other algorithm simply fails that comparison.
//   - RS256/HS256 confusion — there is no asymmetric path to confuse it with.
//   - Non-constant-time comparison — hmac.Equal.
//   - Missing expiry enforcement — checked explicitly below, and a token
//     without an exp claim is rejected rather than treated as eternal.
//
// Each of those has a test asserting the attack fails.
const (
	// tokenTTL is how long a sign-in lasts. Long enough not to interrupt a
	// moderation session, short enough that a leaked token is not permanent.
	// Roles are not embedded in the token (see Claims), so a demotion takes
	// effect immediately regardless of this value.
	tokenTTL = 12 * time.Hour

	jwtHeaderHS256 = `{"alg":"HS256","typ":"JWT"}`
)

var (
	// ErrInvalidToken covers every way a user token fails to verify. One
	// error, for the same reason app credentials collapse to one 401:
	// distinguishing "expired" from "forged" from "malformed" tells an
	// attacker which part of a guess landed.
	ErrInvalidToken = errors.New("invalid user token")

	// ErrNoSecret is a configuration failure, not a credential failure, and
	// is deliberately distinct: it means the service cannot mint or verify
	// anything, which an operator must be able to tell apart from a user
	// presenting a bad token.
	ErrNoSecret = errors.New("JWT_SECRET is not set")
)

// Claims is the payload this service puts in a session token.
//
// Deliberately minimal: a subject and the timing claims. No role, no username,
// no email. A role in the token would be a snapshot — a moderator demoted at
// noon would keep moderating until their token expired, which is precisely the
// window that matters. The middleware loads the user by id on each request
// instead, trading one indexed primary-key lookup for the guarantee that
// authorization reflects the database rather than a claim made hours ago.
type Claims struct {
	Subject   string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// MintUserToken issues a session token for a user id.
func MintUserToken(secret, userID string, now time.Time) (string, error) {
	if secret == "" {
		return "", ErrNoSecret
	}
	if userID == "" {
		return "", errors.New("mint user token: empty user id")
	}

	claims := Claims{
		Subject:   userID,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(tokenTTL).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("mint user token: %w", err)
	}

	signing := b64(([]byte)(jwtHeaderHS256)) + "." + b64(payload)
	return signing + "." + b64(sign(secret, signing)), nil
}

// VerifyUserToken checks a token's signature and expiry, returning its claims.
//
// The header is parsed for nothing. Whatever a token claims about its own
// algorithm is irrelevant: the expected HS256 signature over the presented
// header and payload is computed and compared, so a token asserting
// "alg":"none" fails exactly like one signed with the wrong key.
func VerifyUserToken(secret, token string, now time.Time) (Claims, error) {
	if secret == "" {
		return Claims{}, ErrNoSecret
	}

	header, rest, ok := strings.Cut(token, ".")
	if !ok {
		return Claims{}, ErrInvalidToken
	}
	payload, signature, ok := strings.Cut(rest, ".")
	if !ok {
		return Claims{}, ErrInvalidToken
	}

	want := sign(secret, header+"."+payload)
	got, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(want, got) {
		return Claims{}, ErrInvalidToken
	}

	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var claims Claims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return Claims{}, ErrInvalidToken
	}

	// A token with no exp is rejected rather than treated as never expiring —
	// the absent-claim case is the one an attacker would construct, and it is
	// also what a future bug in MintUserToken would produce.
	if claims.ExpiresAt == 0 || claims.Subject == "" {
		return Claims{}, ErrInvalidToken
	}
	if now.Unix() >= claims.ExpiresAt {
		return Claims{}, ErrInvalidToken
	}
	return claims, nil
}

func sign(secret, signing string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signing))
	return mac.Sum(nil)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
