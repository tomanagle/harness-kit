// Package config loads the server's settings from an injected environment reader.
// Reading process-global environment belongs to main, not application packages.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/tomanagle/harness-kit/server/internal/database"
	"github.com/tomanagle/harness-kit/server/internal/email"
	"github.com/tomanagle/harness-kit/server/internal/httpserver"
	"github.com/tomanagle/harness-kit/server/internal/logger"
)

type Config struct {
	Mail                  email.Config
	RequireVerifiedEmail  bool
	AuthRequestsPerMinute int
	TrustedProxies        []netip.Prefix
	Address               string
	PublicURL             string
	Registration          bool
	Database              database.Config
	HTTP                  httpserver.Config
	Logging               logger.Config
}

func Load(getenv func(string) string) (Config, error) {
	c := Config{Address: "127.0.0.1:8080", PublicURL: "http://localhost:8080", Database: database.Defaults(), HTTP: httpserver.Defaults()}
	if getenv == nil {
		return c, errors.New("config: environment reader is required")
	}
	c.AuthRequestsPerMinute = 120
	if value := getenv("AUTH_REQUESTS_PER_MINUTE"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return c, errors.New("AUTH_REQUESTS_PER_MINUTE must be positive")
		}
		c.AuthRequestsPerMinute = n
	}
	if value := getenv("TRUSTED_PROXY_CIDRS"); value != "" {
		for _, raw := range strings.Split(value, ",") {
			prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
			if err != nil {
				return c, errors.New("invalid TRUSTED_PROXY_CIDRS")
			}
			c.TrustedProxies = append(c.TrustedProxies, prefix)
		}
	}
	if address := getenv("LISTEN_ADDR"); address != "" {
		c.Address = address
	}
	if origin := getenv("PUBLIC_URL"); origin != "" {
		c.PublicURL = strings.TrimRight(origin, "/")
	}
	if value := getenv("ALLOW_REGISTRATION"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return c, errors.New("ALLOW_REGISTRATION must be a boolean")
		}
		c.Registration = enabled
	}
	if level := getenv("LOG_LEVEL"); level != "" {
		if err := c.Logging.Level.UnmarshalText([]byte(level)); err != nil {
			return c, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}
	if limit := getenv("DATABASE_MAX_CONNECTIONS"); limit != "" {
		n, err := strconv.ParseInt(limit, 10, 32)
		if err != nil || n < 1 {
			return c, errors.New("DATABASE_MAX_CONNECTIONS must be a positive 32-bit integer")
		}
		c.Database.MaxConnections = int32(n)
	}
	c.Mail = email.Config{Address: getenv("SMTP_ADDR"), From: getenv("SMTP_FROM"), Username: getenv("SMTP_USERNAME"), Password: getenv("SMTP_PASSWORD")}
	for key, target := range map[string]*bool{"REQUIRE_VERIFIED_EMAIL": &c.RequireVerifiedEmail, "SMTP_INSECURE_LOOPBACK": &c.Mail.AllowInsecureLoopback} {
		if raw := getenv(key); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				return c, fmt.Errorf("%s must be boolean", key)
			}
			*target = value
		}
	}
	if c.RequireVerifiedEmail && c.Mail.Address == "" {
		return c, errors.New("REQUIRE_VERIFIED_EMAIL needs SMTP_ADDR")
	}
	c.Database.URL = getenv("DATABASE_URL")
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return c, errors.New("PUBLIC_URL must be an HTTPS origin (HTTP is allowed only on loopback)")
	}
	if c.Database.URL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	return c, nil
}

// MustLoad is the fail-fast startup variant of Load. Call it only from main.
func MustLoad(getenv func(string) string) Config {
	cfg, err := Load(getenv)
	if err != nil {
		panic(err)
	}
	return cfg
}
