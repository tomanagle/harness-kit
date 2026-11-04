package mcp

import (
	"bytes"
	"path/filepath"
	"testing"
)

type applicationTrustPolicy struct {
	path     string
	approved []byte
	checked  int
}

func (p *applicationTrustPolicy) Trusted(path string, content []byte) bool {
	p.checked++
	return path == p.path && bytes.Equal(content, p.approved)
}

func TestResolverAcceptsApplicationTrustPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ProjectFileName)
	content, err := SaveFile(path, Config{Servers: map[string]Server{"project": {Command: "approved-server"}}})
	if err != nil {
		t.Fatal(err)
	}
	policy := &applicationTrustPolicy{path: path, approved: content}
	resolver := Resolver{Project: true, Trust: policy}
	resolved, err := resolver.Resolve(dir, "", Config{})
	if err != nil || !resolved.ProjectTrusted || resolved.Config.Servers["project"].Command != "approved-server" || policy.checked != 1 {
		t.Fatalf("application policy bypassed: %+v %v checks=%d", resolved, err, policy.checked)
	}
	if _, err := SaveFile(path, Config{Servers: map[string]Server{"project": {Command: "changed-server"}}}); err != nil {
		t.Fatal(err)
	}
	resolved, err = resolver.Resolve(dir, "", Config{})
	if err != nil || resolved.ProjectTrusted || len(resolved.Config.Servers) != 0 || policy.checked != 2 {
		t.Fatalf("changed contents reused prior trust: %+v %v checks=%d", resolved, err, policy.checked)
	}
}

func TestResolverMissingTrustPolicyDeniesProjectServers(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveFile(filepath.Join(dir, ProjectFileName), Config{Servers: map[string]Server{"project": {Command: "server"}}}); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []TrustChecker{nil, TrustStore{}} {
		resolved, err := (Resolver{Project: true, Trust: policy}).Resolve(dir, "", Config{})
		if err != nil || resolved.ProjectTrusted || len(resolved.Config.Servers) != 0 {
			t.Fatalf("empty trust policy accepted project servers: %+v %v", resolved, err)
		}
	}
}
