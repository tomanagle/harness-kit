package rpcserver

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
)

func sessionID(req *jsonrpc.Request) (protocol.SessionIDParams, error) {
	var p protocol.SessionIDParams
	if err := req.UnmarshalParams(&p); err != nil {
		return p, err
	}
	if p.SessionID == "" {
		return p, invalidParams("sessionId is required")
	}
	return p, nil
}

// strictParams rejects typoed setting-operation fields instead of ignoring them.
func strictParams(req *jsonrpc.Request, target any) error {
	if len(req.Params) == 0 {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(req.Params), []byte("null")) {
		return invalidParams("params must be an object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(req.Params, &fields); err != nil {
		return invalidParams("params must be an object")
	}
	for _, name := range []string{"target", "expectedRevision", "reset"} {
		if value, ok := fields[name]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return invalidParams(name + " cannot be null; omit it instead")
		}
	}
	dec := json.NewDecoder(bytes.NewReader(req.Params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return invalidParams(err.Error())
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return invalidParams("trailing parameters")
	}
	return nil
}
