package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Tokens is the credential-generation boundary used by repositories. ID returns
// 32 lowercase hex characters; Token returns 32 bytes encoded as base64url.
type Tokens interface {
	ID() (string, error)
	Token() (string, error)
}

type RandomTokens struct{ entropy io.Reader }

func NewTokens(entropy io.Reader) (*RandomTokens, error) {
	if entropy == nil {
		return nil, errors.New("auth: token entropy source is required")
	}
	return &RandomTokens{entropy: entropy}, nil
}

// MustNewTokens is the fail-fast startup variant. Call it only from main.
func MustNewTokens(entropy io.Reader) *RandomTokens {
	tokens, err := NewTokens(entropy)
	if err != nil {
		panic(err)
	}
	return tokens
}

func (r *RandomTokens) Token() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(r.entropy, b); err != nil {
		return "", fmt.Errorf("auth: token entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (r *RandomTokens) ID() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(r.entropy, b); err != nil {
		return "", fmt.Errorf("auth: ID entropy: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func Digest(token string) string { h := sha256.Sum256([]byte(token)); return hex.EncodeToString(h[:]) }
