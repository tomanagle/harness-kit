package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/tomanagle/harness-kit/core/session"
)

func TestAccountingCheckpointRemainsOpaqueJSONAcrossStores(t *testing.T) {
	// This is the pre-organization ledger's on-disk shape. A custom store need
	// not import or construct private accounting types to preserve it.
	checkpoint := json.RawMessage(`{"tokens":{"total":42,"nativeKnown":true},"cost":{"total":0.5,"known":true},"active":false}`)
	for _, name := range []string{"memory", "file"} {
		t.Run(name, func(t *testing.T) {
			var store session.Store = session.NewMemoryStore()
			if name == "file" {
				var err error
				store, err = session.NewFileStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			record := session.Record{Info: session.Info{ID: "checkpoint"}, Accounting: append(json.RawMessage(nil), checkpoint...)}
			if err := store.Put(ctx, record); err != nil {
				t.Fatal(err)
			}
			record.Accounting[0] = '[' // Mutating input must not corrupt stored bytes.
			saved, err := store.Get(ctx, "checkpoint")
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			if err := json.Unmarshal(checkpoint, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(saved.Accounting, &got); err != nil {
				t.Fatal(err)
			}
			wantJSON, _ := json.Marshal(want)
			gotJSON, _ := json.Marshal(got)
			if !bytes.Equal(wantJSON, gotJSON) {
				t.Fatalf("checkpoint changed: %s", saved.Accounting)
			}
			raw, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(object["accounting"], []byte("{")) {
				t.Fatalf("checkpoint must remain an object, not encoded text: %s", raw)
			}
			saved.Accounting[0] = '[' // Get must also return an independent copy.
			again, err := store.Get(ctx, "checkpoint")
			if err != nil || !json.Valid(again.Accounting) {
				t.Fatalf("caller mutated stored checkpoint: %s, %v", again.Accounting, err)
			}
		})
	}
}
