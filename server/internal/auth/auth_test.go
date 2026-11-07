package auth

import (
	"crypto/rand"
	"testing"
)

func TestPasswordHash(t *testing.T) {
	h, err := NewPasswords(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := h.Hash("long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if !h.Verify("long-enough-password", encoded) || h.Verify("wrong-password", encoded) || h.Verify("anything", "argon2id-v1$bad$bad") {
		t.Fatal("password verification failed")
	}
	if _, err = h.Hash("short"); err == nil {
		t.Fatal("accepted short password")
	}
}
