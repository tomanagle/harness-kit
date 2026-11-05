// Package architecture_test checks the boundaries that directory names alone
// cannot enforce: public API inventory, dependency direction and portability.
package architecture_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/tomanagle/harness-kit/core"

// Add a package here only when it is an intentional embedding API. Everything
// else belongs in internal or cmd. Keep docs/packages.md in sync.
var publicPackages = map[string]bool{
	"": true, "agent": true, "config": true, "event": true,
	"hooks": true, "logging": true, "mcp": true, "mcp/client": true,
	"model": true, "model/contextwindow": true, "permission": true,
	"rpc/protocol": true, "rpc/server": true,
	"runtimes/acp": true, "runtimes/claude": true, "runtimes/codex": true,
	"runtimes/fake": true, "runtimes/transport": true,
	"session": true, "settings": true, "toolhost": true, "tools": true,
	"workspace": true, "workspace/git": true,
}

type goPackage struct {
	ImportPath string
	Imports    []string
	Deps       []string
}

func TestPackageBoundaries(t *testing.T) {
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = filepath.Join("..", "..")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, &stderr)
	}
	seen := map[string]bool{}
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var p goPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(strings.TrimPrefix(p.ImportPath, module), "/")
		seen[name] = true
		if !publicPackages[name] && !strings.HasPrefix(name, "internal/") && !strings.HasPrefix(name, "cmd/") {
			t.Errorf("%s exposes an unreviewed public package; use internal or document its embedding API", name)
		}
		for _, dep := range p.Imports {
			if strings.Contains(strings.Split(dep, "/")[0], ".") && !strings.HasPrefix(dep, module+"/") {
				t.Errorf("%s imports %s: provider/third-party dependencies belong in separate modules", name, dep)
			}
			if strings.HasPrefix(dep, module+"/internal/processtest") {
				t.Errorf("%s imports a test helper in production code", name)
			}
		}
		for _, dep := range p.Deps {
			switch dep {
			case module + "/internal/cli":
				if name != "cmd/harness-kit" {
					t.Errorf("%s depends on CLI composition", name)
				}
			case module + "/internal/protocolgen":
				if name != "cmd/protocolgen" {
					t.Errorf("%s depends on build-time generation", name)
				}
			case module + "/internal/rpcserver":
				if name != "rpc/server" && name != "internal/cli" && name != "cmd/harness-kit" {
					t.Errorf("%s bypasses the public RPC hosting boundary", name)
				}
			}
			if portable(name) && (dep == "os/exec" || dep == module+"/internal/process") {
				t.Errorf("portable package %s transitively imports %s", name, dep)
			}
			if name == "session" && (strings.HasPrefix(dep, module+"/rpc/") || strings.HasPrefix(dep, module+"/runtimes/")) {
				t.Errorf("session orchestration depends on transport or concrete runtime %s", dep)
			}
		}
	}
	for name := range publicPackages {
		if !seen[name] {
			t.Errorf("public package %q disappeared; review compatibility and update the package guide", name)
		}
	}
}

func portable(name string) bool {
	if !publicPackages[name] {
		return false
	}
	switch name {
	case "mcp/client", "toolhost", "workspace/git", "runtimes/acp", "runtimes/claude", "runtimes/codex":
		return false
	default:
		return true
	}
}
