// Command protocolgen derives the public wire schema from the Go server's
// types, then generates independent clients from that schema. It uses only the
// standard library and is not a runtime dependency of either client.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	check := flag.Bool("check", false, "fail if committed schema or generated clients are stale")
	flag.Parse()
	if err := run(*check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "core", "rpc", "protocol", "protocol.go")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("run protocolgen inside the harness-kit checkout")
		}
		dir = parent
	}
}

func run(check bool) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	doc, err := buildSchema(root)
	if err != nil {
		return err
	}
	schema, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	schema = append(schema, '\n')
	// Render from the serialized contract, not reflection state: the JSON
	// document is sufficient to reproduce both clients' generated surfaces.
	var contract Document
	if err := json.Unmarshal(schema, &contract); err != nil {
		return err
	}
	outputs, err := render(&contract)
	if err != nil {
		return err
	}
	outputs["protocol/schema.json"] = schema
	stale := false
	for _, name := range sortedKeys(outputs) {
		path := filepath.Join(root, filepath.FromSlash(name))
		previous, err := os.ReadFile(path)
		if err == nil && bytes.Equal(previous, outputs[name]) {
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if check {
			fmt.Fprintln(os.Stderr, "stale generated file:", name)
			stale = true
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, outputs[name], 0644); err != nil {
			return err
		}
		fmt.Println("generated", name)
	}
	if stale {
		return fmt.Errorf("run make generate and commit the updated outputs")
	}
	return nil
}
