package settings

import (
	"strings"
	"testing"
)

func TestServerConnections(t *testing.T) {
	for _, raw := range []string{
		`{"sync":{"paused":true,"pollIntervalMs":100,"batchEvents":1,"workPerCycle":2}}`,
		`{"servers":{"connections":[{"id":"work","url":"https://work.example","name":"Work","enabled":true},{"id":"personal","url":"http://localhost:8080","name":"Personal","enabled":true}]}}`,
		`{"servers":{"connections":[]}}`,
	} {
		v, err := Decode(strings.NewReader(raw), Defaults())
		if err != nil {
			t.Fatal(err)
		}
		if err = Validate(v); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{
		`{"sync":{"pollIntervalMs":0}}`,
		`{"sync":{"batchEvents":101}}`,
		`{"sync":{"workPerCycle":0}}`,
		`{"servers":{"connections":[{"id":"work","url":"http://work.example"}]}}`,
		`{"servers":{"connections":[{"id":"work","url":"https://token@work.example"}]}}`,
		`{"servers":{"connections":[{"id":"work","url":"https://work.example?token=secret"}]}}`,
		`{"servers":{"connections":[{"id":"work","url":"https://work.example","accessToken":"secret"}]}}`,
		`{"servers":{"connections":[{"id":"same","url":"https://a.example"},{"id":"same","url":"https://b.example"}]}}`,
	} {
		if _, err := Decode(strings.NewReader(raw), Defaults()); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if Describe()["servers.connections"].When != "live" {
		t.Fatal("server settings require restart")
	}
}
