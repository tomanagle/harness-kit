package client_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/tomanagle/harness-kit/rpc/client"
	"github.com/tomanagle/harness-kit/settings"
)

func TestConnectRejectsBootSettingsWithoutTouchingTransport(t *testing.T) {
	_, err := client.Connect(context.Background(), strings.NewReader(""), io.Discard, client.Options{
		Settings: settings.Patch{"process": json.RawMessage(`{"store":"/new/store"}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "require Spawn") {
		t.Fatalf("boot settings were ignored: %v", err)
	}
}
