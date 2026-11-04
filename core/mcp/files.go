package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ProjectFileName is the per-project config file, found by walking up from
// a session's working directory.
const ProjectFileName = "harness-kit.json"

// UserConfigPath is the per-user config file: HARNESS_KIT_CONFIG, or
// <user config dir>/harness-kit/config.json.
func UserConfigPath() (string, error) {
	if p := os.Getenv("HARNESS_KIT_CONFIG"); p != "" {
		return p, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "harness-kit", "config.json"), nil
}

// TrustPath is where project trust decisions are kept: trust.json next to
// the user config file.
func TrustPath() (string, error) {
	p, err := UserConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "trust.json"), nil
}

// FindProjectFile looks for harness-kit.json in dir and its parents, up to
// the filesystem root.
func FindProjectFile(dir string) (string, bool) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		p := filepath.Join(dir, ProjectFileName)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// LoadFile reads a config file. A missing file is an empty config.
func LoadFile(path string) (Config, []byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil, nil
	}
	if err != nil {
		return Config{}, nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return Config{}, b, fmt.Errorf("%s: %w", path, err)
	}
	return c, b, nil
}

// SaveFile writes c to path atomically (mode 0600, parent dirs 0700) and
// returns the bytes written.
func SaveFile(path string, c Config) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	for n, s := range c.Servers {
		s.Source = "" // derived on load; never stored
		c.Servers[n] = s
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	return b, writeAtomic(path, b)
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// TrustStore records which project config files the user has approved.
// Trust is tied to the file's exact content: any change requires trusting
// it again, so a pulled commit cannot silently add a server.
type TrustStore struct{ Path string }

// TrustChecker decides whether a project configuration's exact content may be
// loaded. Implementations can use application policy, a database, or TrustStore.
// A changed file must be evaluated again; trusting only its path is insufficient.
type TrustChecker interface {
	Trusted(file string, content []byte) bool
}

var _ TrustChecker = TrustStore{}

type trustFile struct {
	Projects map[string]string `json:"projects"` // abs path -> sha256 of content
}

func (t TrustStore) load() (trustFile, error) {
	tf := trustFile{Projects: map[string]string{}}
	b, err := os.ReadFile(t.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return tf, nil
	}
	if err != nil {
		return tf, err
	}
	if err := json.Unmarshal(b, &tf); err != nil {
		return tf, fmt.Errorf("%s: %w", t.Path, err)
	}
	if tf.Projects == nil {
		tf.Projects = map[string]string{}
	}
	return tf, nil
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// Trusted reports whether file, with exactly this content, is trusted.
func (t TrustStore) Trusted(file string, content []byte) bool {
	if t.Path == "" {
		return false
	}
	tf, err := t.load()
	if err != nil {
		return false
	}
	abs, _ := filepath.Abs(file)
	return tf.Projects[abs] == digest(content)
}

// Trust records file's current content as trusted.
func (t TrustStore) Trust(file string, content []byte) error {
	tf, err := t.load()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	tf.Projects[abs] = digest(content)
	b, _ := json.MarshalIndent(tf, "", "  ")
	return writeAtomic(t.Path, append(b, '\n'))
}

// Untrust forgets file.
func (t TrustStore) Untrust(file string) error {
	tf, err := t.load()
	if err != nil {
		return err
	}
	abs, _ := filepath.Abs(file)
	delete(tf.Projects, abs)
	b, _ := json.MarshalIndent(tf, "", "  ")
	return writeAtomic(t.Path, append(b, '\n'))
}

// Resolver builds a session's effective MCP config from the user file,
// the project file and the session's own servers.
type Resolver struct {
	// UserFile is the user config path; empty skips the user layer.
	UserFile string
	// Project enables looking for harness-kit.json from the session cwd up.
	Project bool
	// Trust decides whether project servers may load. Nil or a zero TrustStore
	// trusts nothing. File discovery/loading remains the resolver's concern.
	Trust TrustChecker
}

// Resolution is the result of Resolve.
type Resolution struct {
	// Config is the effective config for the requested runtime.
	Config Config
	// All is the merged config before filtering by runtime.
	All Config
	// ProjectFile is the project file found, if any.
	ProjectFile string
	// ProjectTrusted reports whether its servers were loaded.
	ProjectTrusted bool
	// Warnings explain servers that were skipped.
	Warnings []string
}

// Resolve merges user < project < session, keeps the servers that apply to
// runtime, and drops project servers unless the project file is trusted.
// Malformed files are errors.
func (r Resolver) Resolve(cwd, runtime string, session Config) (Resolution, error) {
	var res Resolution
	var layers []Config
	if r.UserFile != "" {
		c, _, err := LoadFile(r.UserFile)
		if err != nil {
			return res, err
		}
		layers = append(layers, tag(c, "user"))
	}
	if r.Project && cwd != "" {
		if p, ok := FindProjectFile(cwd); ok {
			c, content, err := LoadFile(p)
			if err != nil {
				return res, err
			}
			res.ProjectFile = p
			res.ProjectTrusted = r.Trust != nil && r.Trust.Trusted(p, content)
			if !res.ProjectTrusted && len(c.Servers) > 0 {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"skipped %d MCP server(s) from untrusted %s; review it, then run `harness-kit mcp trust`", len(c.Servers), p))
				c.Servers = nil // inheritNative may still only restrict
			}
			layers = append(layers, tag(c, "project"))
		}
	}
	layers = append(layers, tag(session, "session"))
	res.All = Merge(layers...)
	res.Config = res.All.ForRuntime(runtime)
	return res, nil
}

func tag(c Config, source string) Config {
	out := Config{InheritNative: c.InheritNative, Servers: make(map[string]Server, len(c.Servers))}
	for n, s := range c.Servers {
		s.Source = source
		out.Servers[n] = s
	}
	return out
}

// Entry is one server in an Overview.
type Entry struct {
	Name   string `json:"name"`
	Source string `json:"source"` // "user" or "project"
	// Loaded is false for servers that will not load (untrusted project file).
	Loaded bool   `json:"loaded"`
	Note   string `json:"note,omitempty"`
	Server Server `json:"server"`
}

// Overview describes the kit MCP configuration seen from a directory: what
// will load, from where, and what is held back.
type Overview struct {
	Servers        []Entry `json:"servers"`
	InheritNative  bool    `json:"inheritNative"`
	UserFile       string  `json:"userFile,omitempty"`
	ProjectFile    string  `json:"projectFile,omitempty"`
	ProjectTrusted bool    `json:"projectTrusted"`
}

// Overview lists the effective servers for cwd (all runtimes), plus the
// servers of an untrusted project file, marked as not loaded.
func (r Resolver) Overview(cwd string) (Overview, error) {
	res, err := r.Resolve(cwd, "", Config{})
	if err != nil {
		return Overview{}, err
	}
	ov := Overview{Servers: []Entry{}, InheritNative: res.All.Inherit(), UserFile: r.UserFile, ProjectFile: res.ProjectFile, ProjectTrusted: res.ProjectTrusted}
	for _, name := range res.All.Names() {
		s := res.All.Servers[name]
		ov.Servers = append(ov.Servers, Entry{Name: name, Source: s.Source, Loaded: true, Server: s})
	}
	if res.ProjectFile != "" && !res.ProjectTrusted {
		if pc, _, err := LoadFile(res.ProjectFile); err == nil {
			for _, name := range pc.Names() {
				ov.Servers = append(ov.Servers, Entry{Name: name, Source: "project", Loaded: false,
					Note: "untrusted: run `harness-kit mcp trust`", Server: pc.Servers[name]})
			}
		}
	}
	return ov, nil
}
