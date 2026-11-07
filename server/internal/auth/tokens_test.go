package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

type brokenEntropy struct{ err error }

func (r brokenEntropy) Read([]byte) (int, error) { return 0, r.err }

func TestEntropyFailuresReturnErrors(t *testing.T) {
	failure := errors.New("entropy unavailable")
	tokens, err := NewTokens(brokenEntropy{failure})
	if err != nil {
		t.Fatal(err)
	}
	for _, generate := range []func() (string, error){tokens.ID, tokens.Token} {
		if _, err := generate(); !errors.Is(err, failure) {
			t.Fatalf("expected entropy error, got %v", err)
		}
	}
	passwords, err := NewPasswords(brokenEntropy{failure})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = passwords.Hash("long-enough-password"); !errors.Is(err, failure) || errors.Is(err, ErrPasswordInvalid) {
		t.Fatalf("wrong hash failure: %v", err)
	}
}

func TestTokensUseFullEntropyAndRetainWireFormat(t *testing.T) {
	tokens, err := NewTokens(bytes.NewReader(bytes.Repeat([]byte{0xab}, 48)))
	if err != nil {
		t.Fatal(err)
	}
	id, err := tokens.ID()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 16 {
		t.Fatalf("invalid ID: %q", id)
	}
	token, err := tokens.Token()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("invalid token: %q", token)
	}
	if _, err = tokens.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("short entropy did not fail: %v", err)
	}
}
