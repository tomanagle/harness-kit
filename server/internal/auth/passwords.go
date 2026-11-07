// Package auth implements password hashing and credential generation using
// caller-supplied entropy. Failures are returned to the requesting operation.
package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

var ErrPasswordInvalid = errors.New("password must contain 12 to 1024 bytes")

type Passwords interface {
	Hash(string) (string, error)
	Verify(string, string) bool
}

type Argon2 struct{ entropy io.Reader }

func NewPasswords(entropy io.Reader) (*Argon2, error) {
	if entropy == nil {
		return nil, errors.New("auth: password entropy source is required")
	}
	return &Argon2{entropy: entropy}, nil
}

// MustNewPasswords is the fail-fast startup variant. Call it only from main.
func MustNewPasswords(entropy io.Reader) *Argon2 {
	passwords, err := NewPasswords(entropy)
	if err != nil {
		panic(err)
	}
	return passwords
}

func (a *Argon2) Hash(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 {
		return "", ErrPasswordInvalid
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(a.entropy, salt); err != nil {
		return "", fmt.Errorf("auth: password salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, 2, 64*1024, 2, 32)
	return "argon2id-v1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash), nil
}

func (*Argon2) Verify(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 || parts[0] != "argon2id-v1" || len(password) > 1024 {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(parts[1])
	want, e2 := base64.RawStdEncoding.DecodeString(parts[2])
	if e1 != nil || e2 != nil || len(salt) != 16 || len(want) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 2, 64*1024, 2, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}
