package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryCredentials struct {
	mu       sync.Mutex
	grant    Grant
	deleted  bool
	failSave bool
}

func (m *memoryCredentials) Load(context.Context, string) (Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.grant, nil
}
func (m *memoryCredentials) Save(_ context.Context, _ string, g Grant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSave {
		return errors.New("disk full")
	}
	m.grant = g
	return nil
}
func (m *memoryCredentials) Delete(context.Context, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = true
	return nil
}
func TestConnectionSerializesRotationAndFailsClosed(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "concurrent", true: "uncertain persistence"}[fail], func(t *testing.T) {
			var refreshes atomic.Int32
			grant := Grant{AccountID: "a", TenantID: "t", ApplicationID: "app", DeviceID: "d", AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Second).Format(time.RFC3339)}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/devices/refresh" {
					t.Error("unexpected request", r.URL.Path)
					return
				}
				refreshes.Add(1)
				next := grant
				next.AccessToken = "new"
				next.RefreshToken = "next"
				next.ExpiresAt = time.Now().Add(time.Hour).Format(time.RFC3339)
				_ = json.NewEncoder(w).Encode(next)
			}))
			defer server.Close()
			credentials := &memoryCredentials{grant: grant, failSave: fail}
			connection, err := NewConnection(ConnectionOptions{Scope: Scope{ServerID: "s", URL: server.URL, AccountID: "a", TenantID: "t", ApplicationID: "app"}, Credentials: credentials})
			if err != nil {
				t.Fatal(err)
			}
			changed := connection.Scope()
			changed.TenantID = "other"
			if connection.Scope().TenantID != "t" {
				t.Fatal("scope mutable")
			}
			var wg sync.WaitGroup
			for range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					token, e := connection.token(context.Background())
					if fail {
						if e == nil {
							t.Error("uncertain save accepted")
						}
					} else if e != nil || token != "new" {
						t.Errorf("token=%s error=%v", token, e)
					}
				}()
			}
			wg.Wait()
			if refreshes.Load() != 1 {
				t.Fatal("refresh repeated", refreshes.Load())
			}
			if credentials.deleted != fail {
				t.Fatal("invalid credential retention")
			}
		})
	}
}
