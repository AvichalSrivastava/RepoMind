package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// HMACVerifier verifies the X-Hub-Signature-256 header on incoming
// GitHub webhook deliveries. It is the only sha256-based verifier we
// support; sha1 has been deprecated by GitHub since 2019.
//
// Construction: NewHMACVerifier(secret). secret must not be empty.
//
// Verify is safe for concurrent use; it allocates only for the
// hex decode of the signature header.
type HMACVerifier struct {
	secret []byte
}

// NewHMACVerifier returns a verifier bound to the given shared secret.
func NewHMACVerifier(secret string) (*HMACVerifier, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("github: webhook secret is empty")
	}
	// Defensive copy so callers can scrub the original.
	b := make([]byte, len(secret))
	copy(b, secret)
	return &HMACVerifier{secret: b}, nil
}

// Verify implements apigithub.WebhookVerifier.
//
// signatureHeader MUST be the value of the X-Hub-Signature-256 header
// (e.g. "sha256=ab12..."). body MUST be the raw, untouched request
// body — any normalisation (e.g. JSON re-encoding) will invalidate the
// signature.
func (v *HMACVerifier) Verify(signatureHeader string, body []byte) error {
	const prefix = "sha256="
	sig := strings.TrimSpace(signatureHeader)
	if !strings.HasPrefix(sig, prefix) {
		return ErrInvalidSignature
	}
	got, err := hex.DecodeString(sig[len(prefix):])
	if err != nil {
		return ErrInvalidSignature
	}

	mac := hmac.New(sha256.New, v.secret)
	_, _ = mac.Write(body)
	expected := mac.Sum(nil)

	if !hmac.Equal(got, expected) {
		return ErrInvalidSignature
	}
	return nil
}
