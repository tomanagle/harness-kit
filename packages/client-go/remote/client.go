// Package remote is the generated HTTP client for a Harness Kit coordination
// server. It is independent of the local sidecar client and has no SDK dependency.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
	token   func(context.Context) (string, error)
}

// New binds the client to one server and token source. Create separate clients
// for each account/tenant/application grant. Tokens are never forwarded on redirects.
func New(baseURL string, httpClient *http.Client, token func(context.Context) (string, error)) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return nil, fmt.Errorf("remote: expected HTTPS origin or loopback HTTP")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 35 * time.Second}
	}
	copyClient := *httpClient
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: u.String(), http: &copyClient, token: token}, nil
}

type HTTPError struct {
	Status int
	Code   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("remote: %s (HTTP %d)", e.Code, e.Status) }
func (c *Client) request(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != nil {
		token, e := c.token(ctx)
		if e != nil {
			return e
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024+1))
	if err != nil {
		return err
	}
	if len(body) > 2*1024*1024 {
		return fmt.Errorf("remote: response exceeds 2 MiB")
	}
	if res.StatusCode != 200 {
		var e Error
		_ = json.Unmarshal(body, &e)
		if e.Code == "" {
			e.Code = "http_error"
		}
		return &HTTPError{Status: res.StatusCode, Code: e.Code}
	}
	return json.Unmarshal(body, out)
}
