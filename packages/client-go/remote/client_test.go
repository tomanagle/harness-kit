package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNeverFollowCredentialRedirect(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		t.Error("followed redirect with credential")
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing token")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	c, err := New(origin.URL, nil, func(context.Context) (string, error) { return "secret", nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ListTenants(context.Background(), Empty{}); err == nil {
		t.Fatal("accepted redirect")
	}
	if reached {
		t.Fatal("redirect was followed")
	}
}
func TestRejectInsecureOrigin(t *testing.T) {
	for _, origin := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com/path", "https://example.com?token=secret"} {
		if _, err := New(origin, nil, nil); err == nil {
			t.Errorf("accepted %s", origin)
		}
	}
}
