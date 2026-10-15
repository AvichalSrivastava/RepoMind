package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	apigithub "github.com/repomind/repomind/internal/application/github"
)

// Compile-time guarantee that *HMACVerifier satisfies the port.
var _ apigithub.WebhookVerifier = (*HMACVerifier)(nil)

func sign(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestNewHMACVerifier_RejectsEmpty(t *testing.T) {
	if _, err := NewHMACVerifier(""); err == nil {
		t.Error("expected error for empty secret")
	}
	if _, err := NewHMACVerifier("   "); err == nil {
		t.Error("expected error for whitespace secret")
	}
}

func TestHMACVerifier_HappyPath(t *testing.T) {
	v, err := NewHMACVerifier("topsecret")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"action":"opened"}`)
	if err := v.Verify(sign(t, "topsecret", body), body); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestHMACVerifier_TamperedBody(t *testing.T) {
	v, _ := NewHMACVerifier("topsecret")
	body := []byte(`{"action":"opened"}`)
	sig := sign(t, "topsecret", body)
	tampered := []byte(`{"action":"closed"}`)
	if err := v.Verify(sig, tampered); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature, got %v", err)
	}
}

func TestHMACVerifier_WrongSecret(t *testing.T) {
	v, _ := NewHMACVerifier("topsecret")
	body := []byte(`{"a":1}`)
	if err := v.Verify(sign(t, "other-secret", body), body); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature, got %v", err)
	}
}

func TestHMACVerifier_RejectsSha1(t *testing.T) {
	v, _ := NewHMACVerifier("topsecret")
	body := []byte(`{}`)
	// X-Hub-Signature (v1) prefix — must be refused.
	if err := v.Verify("sha1=deadbeef", body); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature for sha1 header, got %v", err)
	}
}

func TestHMACVerifier_MissingPrefix(t *testing.T) {
	v, _ := NewHMACVerifier("topsecret")
	if err := v.Verify("", []byte("x")); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature for empty header, got %v", err)
	}
	if err := v.Verify("deadbeef", []byte("x")); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature for unprefixed sig, got %v", err)
	}
}

func TestHMACVerifier_NonHexSignature(t *testing.T) {
	v, _ := NewHMACVerifier("topsecret")
	if err := v.Verify("sha256=zzz-not-hex", []byte("x")); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature, got %v", err)
	}
}

func TestHMACVerifier_TrimsHeaderWhitespace(t *testing.T) {
	v, _ := NewHMACVerifier("topsecret")
	body := []byte(`{}`)
	sig := strings.TrimSpace(sign(t, "topsecret", body))
	if err := v.Verify("  "+sig+"  ", body); err != nil {
		t.Fatalf("verify trimmed: %v", err)
	}
}
