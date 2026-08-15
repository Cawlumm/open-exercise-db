// Package auth holds the credential primitives the write API authenticates
// with, independent of HTTP.
//
// Separate from internal/api because there are two consumers, not one: the
// middleware that verifies a presented credential, and whatever registers an
// app in the first place (a maintainer CLI today, a self-service flow later).
// Minting and verifying have to agree exactly, so they live together.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Token layout. An app presents one opaque string; this package is the only
// thing that knows it has structure.
//
//	oedb_<key_id>.<secret>
//
// The prefix makes a leaked credential greppable — in a log aggregator, a
// commit, a paste — and makes it obvious what a stray string is. The dot
// separates the two halves, and is safe as a separator precisely because both
// halves are base64url, whose alphabet excludes it.
const (
	tokenPrefix = "oedb_"
	tokenSep    = "."

	// 16 bytes of key id: enough that ids never collide, short enough to read
	// in a log line. It is not a secret — it travels in every request and is
	// stored in the clear — it only has to be unique.
	keyIDBytes = 16

	// 32 bytes of secret: 256 bits of CSPRNG output. See HashSecret for why
	// that size is what makes a plain hash the right choice here.
	secretBytes = 32
)

// ErrMalformedToken is returned by ParseToken for anything that is not a
// well-formed credential. Deliberately one error for every malformation:
// telling a caller *how* their token is wrong tells an attacker which half
// they got right.
var ErrMalformedToken = errors.New("malformed app token")

// GenerateAppCredential mints a new app credential.
//
// Returns the key id (stored in the clear), the full token (handed to the app
// once and never recoverable afterward), and the hash to store. The secret
// itself is deliberately not returned separately — the caller has no use for
// it beyond building the token, and every extra copy is another place it can
// be logged.
func GenerateAppCredential() (keyID, token, hash string, err error) {
	idRaw := make([]byte, keyIDBytes)
	if _, err := rand.Read(idRaw); err != nil {
		return "", "", "", fmt.Errorf("generate key id: %w", err)
	}
	secretRaw := make([]byte, secretBytes)
	if _, err := rand.Read(secretRaw); err != nil {
		return "", "", "", fmt.Errorf("generate secret: %w", err)
	}

	keyID = base64.RawURLEncoding.EncodeToString(idRaw)
	secret := base64.RawURLEncoding.EncodeToString(secretRaw)

	return keyID, FormatToken(keyID, secret), HashSecret(secret), nil
}

// FormatToken assembles the string an app presents.
func FormatToken(keyID, secret string) string {
	return tokenPrefix + keyID + tokenSep + secret
}

// ParseToken splits a presented token into the half that identifies which app
// row to load and the half that proves the app is entitled to it.
func ParseToken(token string) (keyID, secret string, err error) {
	rest, ok := strings.CutPrefix(token, tokenPrefix)
	if !ok {
		return "", "", ErrMalformedToken
	}
	keyID, secret, ok = strings.Cut(rest, tokenSep)
	if !ok || keyID == "" || secret == "" {
		return "", "", ErrMalformedToken
	}
	return keyID, secret, nil
}

// HashSecret returns the value stored in apps.key_hash.
//
// A plain SHA-256, deliberately, where a human password would demand bcrypt
// or argon2. Key stretching exists to make brute-forcing *low-entropy* inputs
// expensive — people pick guessable passwords, so each guess must be made to
// cost. This secret is 256 bits straight from crypto/rand and is not
// guessable at any cost, so stretching buys nothing. It is not free either:
// bcrypt would add tens of milliseconds to every authenticated write, on the
// one path a busy app hits hardest, in exchange for defending against an
// attack that cannot happen.
//
// This is the same reasoning behind Cloudflare R2's own S3 credentials, whose
// secret access key is a SHA-256 of the API token value.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// VerifySecret reports whether secret matches a stored hash.
//
// Constant-time, so the comparison cannot be turned into an oracle that leaks
// the stored hash a byte at a time. The hash is not itself secret in the way
// the token is, but a `==` here would be a free vulnerability to avoid.
func VerifySecret(secret, storedHash string) bool {
	computed := HashSecret(secret)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(storedHash)) == 1
}
