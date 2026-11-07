package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// Scope is immutable per connection; selecting another tenant in a UI must create
// another connection, not redirect queued operations or reuse its credentials.
type Scope struct{ ServerID, URL, AccountID, TenantID, ApplicationID string }

func (s Scope) Key() string {
	raw, _ := json.Marshal([]string{s.ServerID, s.URL, s.AccountID, s.TenantID, s.ApplicationID})
	return string(raw)
}

type CredentialStore interface {
	Load(context.Context, string) (Grant, error)
	Save(context.Context, string, Grant) error
	Delete(context.Context, string) error
}
type ConnectionOptions struct {
	Scope       Scope
	HTTP        *http.Client
	Credentials CredentialStore
}
type Connection struct {
	Client      *Client
	scope       Scope
	credentials CredentialStore
	http        *http.Client
	mu          sync.Mutex
	reauthorize bool
}

func NewConnection(opts ConnectionOptions) (*Connection, error) {
	if opts.Credentials == nil || opts.Scope.AccountID == "" || opts.Scope.TenantID == "" || opts.Scope.ApplicationID == "" {
		return nil, errors.New("remote: scope and credential store required")
	}
	c := &Connection{scope: opts.Scope, credentials: opts.Credentials, http: opts.HTTP}
	client, err := New(opts.Scope.URL, opts.HTTP, c.token)
	if err != nil {
		return nil, err
	}
	c.scope.URL = client.baseURL
	c.Client = client
	return c, nil
}
func (c *Connection) Scope() Scope { return c.scope }
func (c *Connection) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reauthorize {
		return "", errors.New("remote: reauthorization required")
	}
	grant, err := c.credentials.Load(ctx, c.scope.Key())
	if err != nil {
		return "", err
	}
	if grant.AccountID != c.scope.AccountID || grant.TenantID != c.scope.TenantID || grant.ApplicationID != c.scope.ApplicationID {
		return "", errors.New("remote: credential scope mismatch")
	}
	expires, err := time.Parse(time.RFC3339, grant.ExpiresAt)
	if err != nil {
		return "", err
	}
	if time.Until(expires) > 30*time.Second {
		return grant.AccessToken, nil
	}
	anonymous, err := New(c.scope.URL, c.http, nil)
	if err != nil {
		return "", err
	}
	next, err := anonymous.RefreshDevice(ctx, Refresh{RefreshToken: grant.RefreshToken})
	if err == nil && (next.AccountID != grant.AccountID || next.TenantID != grant.TenantID || next.ApplicationID != grant.ApplicationID || next.DeviceID != grant.DeviceID) {
		err = errors.New("remote: refresh scope mismatch")
	}
	if err == nil {
		err = c.credentials.Save(ctx, c.scope.Key(), next)
	}
	if err != nil {
		c.reauthorize = true
		_ = c.credentials.Delete(ctx, c.scope.Key())
		return "", err
	}
	return next.AccessToken, nil
}

// Authorize runs the device flow; the host opens verificationUri and displays
// userCode through onCode. Credentials are returned only to the calling host.
func Authorize(ctx context.Context, client *Client, input DeviceBegin, onCode func(DeviceAuthorization) error) (Grant, error) {
	if client == nil || onCode == nil {
		return Grant{}, errors.New("remote: client and onCode required")
	}
	code, err := client.BeginDevice(ctx, input)
	if err != nil {
		return Grant{}, err
	}
	if err = onCode(code); err != nil {
		return Grant{}, err
	}
	deadline := time.NewTimer(time.Duration(code.ExpiresIn) * time.Second)
	defer deadline.Stop()
	interval := time.Duration(max(code.Interval, 1)) * time.Second
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Grant{}, ctx.Err()
		case <-deadline.C:
			timer.Stop()
			return Grant{}, errors.New("remote: device code expired")
		case <-timer.C:
		}
		grant, err := client.PollDevice(ctx, DevicePoll{DeviceCode: code.DeviceCode})
		if err == nil {
			if grant.ApplicationID != input.ApplicationID {
				return Grant{}, errors.New("remote: authorization scope mismatch")
			}
			return grant, nil
		}
		var response *HTTPError
		if !errors.As(err, &response) {
			return Grant{}, err
		}
		if response.Code == "slow_down" {
			interval += 5 * time.Second
			continue
		}
		if response.Code != "authorization_pending" {
			return Grant{}, err
		}
	}
}
