package config

import (
	"log/slog"
	"testing"
)

func TestLoadUsesInjectedEnvironment(t *testing.T) {
	values := map[string]string{"DATABASE_URL": "postgres://local/harness", "DATABASE_MAX_CONNECTIONS": "7", "LOG_LEVEL": "debug", "ALLOW_REGISTRATION": "true"}
	cfg, err := Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.MaxConnections != 7 || cfg.Logging.Level != slog.LevelDebug || !cfg.Registration {
		t.Fatalf("options not applied: %+v", cfg)
	}
	if cfg.HTTP.ShutdownTimeout <= 0 || cfg.Database.ConnectTimeout <= 0 {
		t.Fatal("missing startup bounds")
	}
	for key, value := range map[string]string{"DATABASE_MAX_CONNECTIONS": "0", "LOG_LEVEL": "invalid", "ALLOW_REGISTRATION": "typo", "PUBLIC_URL": "https://credentials@server.test"} {
		t.Run(key, func(t *testing.T) {
			_, err := Load(func(name string) string {
				if name == key {
					return value
				}
				return values[name]
			})
			if err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}
