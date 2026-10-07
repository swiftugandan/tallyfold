// Package pluginpkg checks the plugin package against the Agent Plugins 1.0.0
// specification and keeps the Claude Code extension, marketplace entry and
// release file in step with it.
package pluginpkg

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	root          = "../../plugin"
	pluginSchema  = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	mcpSchema     = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
	claudeExtDir  = "com.anthropic.claude-code"
	launcherToken = "./bin/tallyfold"
)

func readJSON(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func str(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("expected a string, got %s", raw)
	}
	return s
}

func releaseVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "bin", "release"))
	if err != nil {
		t.Fatal(err)
	}
	v, ok := strings.CutPrefix(strings.SplitN(string(data), "\n", 2)[0], "version ")
	if !ok {
		t.Fatal("bin/release does not start with a version line")
	}
	return v
}

// §5: closed manifest, required $schema and name, name constraints.
func TestManifest(t *testing.T) {
	m := readJSON(t, filepath.Join(root, "plugin.json"))
	allowed := []string{"$schema", "name", "version", "description", "author", "homepage", "repository", "license", "keywords", "extensions"}
	for k := range m {
		if !slices.Contains(allowed, k) {
			t.Errorf("plugin.json has field %q, which the closed manifest schema does not allow", k)
		}
	}
	if got := str(t, m["$schema"]); got != pluginSchema {
		t.Errorf("$schema = %q", got)
	}
	name := str(t, m["name"])
	if !regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,62}[a-z0-9])?$`).MatchString(name) || strings.Contains(name, "--") || strings.Contains(name, "..") {
		t.Errorf("name %q breaks the naming rules", name)
	}
	if got := str(t, m["version"]); got != releaseVersion(t) {
		t.Errorf("plugin.json version %q differs from bin/release %q", got, releaseVersion(t))
	}
	var author map[string]any
	_ = json.Unmarshal(m["author"], &author)
	for k := range author {
		if k != "name" && k != "email" && k != "url" {
			t.Errorf("author has field %q", k)
		}
	}
}

// §7.2 and §9.2: closed mcp.json, stdio command as one plugin-relative token, no
// reserved variables in env.
func TestPortableMCP(t *testing.T) {
	m := readJSON(t, filepath.Join(root, "mcp.json"))
	for k := range m {
		if k != "$schema" && k != "mcpServers" {
			t.Errorf("mcp.json has top-level field %q", k)
		}
	}
	if got := str(t, m["$schema"]); got != mcpSchema {
		t.Errorf("$schema = %q", got)
	}
	var servers map[string]map[string]json.RawMessage
	if err := json.Unmarshal(m["mcpServers"], &servers); err != nil || len(servers) == 0 {
		t.Fatalf("mcpServers: %v", err)
	}
	for name, s := range servers {
		for k := range s {
			if !slices.Contains([]string{"type", "command", "args", "env", "cwd"}, k) {
				t.Errorf("server %s has field %q, which stdio does not allow", name, k)
			}
		}
		if str(t, s["type"]) != "stdio" {
			t.Errorf("server %s is not stdio", name)
		}
		cmd := str(t, s["command"])
		if cmd != launcherToken || strings.ContainsAny(cmd, " $") {
			t.Errorf("server %s command %q must be the plugin-relative launcher", name, cmd)
		}
		var env map[string]string
		_ = json.Unmarshal(s["env"], &env)
		if _, ok := env["PLUGIN_ROOT"]; ok {
			t.Errorf("server %s sets PLUGIN_ROOT in env", name)
		}
		if _, ok := env["PLUGIN_DATA"]; ok {
			t.Errorf("server %s sets PLUGIN_DATA in env", name)
		}
	}
	fi, err := os.Stat(filepath.Join(root, launcherToken))
	if err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("%s is missing or not executable", launcherToken)
	}
}

// §7.1 and the Agent Skills spec: one skill per directory, name matches.
func TestSkills(t *testing.T) {
	dirs, err := os.ReadDir(filepath.Join(root, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		f, err := os.Open(filepath.Join(root, "skills", d.Name(), "SKILL.md"))
		if err != nil {
			t.Errorf("skills/%s has no SKILL.md", d.Name())
			continue
		}
		front := map[string]string{}
		sc := bufio.NewScanner(f)
		sc.Scan()
		if sc.Text() != "---" {
			t.Errorf("skills/%s/SKILL.md has no frontmatter", d.Name())
		}
		for sc.Scan() && sc.Text() != "---" {
			k, v, _ := strings.Cut(sc.Text(), ":")
			front[k] = strings.TrimSpace(v)
		}
		f.Close()
		if front["name"] != d.Name() {
			t.Errorf("skills/%s: name %q must match the directory", d.Name(), front["name"])
		}
		if n := len(front["description"]); n == 0 || n > 1024 {
			t.Errorf("skills/%s: description is %d characters; the limit is 1024", d.Name(), n)
		}
	}
}

// §8: client-specific files live in the client's namespace directory.
func TestPackageHoldsOnlyPortableFilesAndNamespaces(t *testing.T) {
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{"plugin.json", "mcp.json", "skills", "bin", "LICENSE", claudeExtDir}
	for _, e := range entries {
		if !slices.Contains(allowed, e.Name()) {
			t.Errorf("plugin/%s is neither a portable file nor a client namespace directory", e.Name())
		}
	}
	a, _ := os.ReadFile("../../LICENSE")
	b, _ := os.ReadFile(filepath.Join(root, "LICENSE"))
	if !bytes.Equal(a, b) {
		t.Error("plugin/LICENSE differs from LICENSE")
	}
}

// Claude Code ignores the portable files, so its extension and marketplace entry
// must point at the same launcher and version.
func TestClaudeCodeEntryMatchesThePackage(t *testing.T) {
	var ext struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	data, _ := os.ReadFile(filepath.Join(root, claudeExtDir, "mcp.json"))
	if err := json.Unmarshal(data, &ext); err != nil {
		t.Fatal(err)
	}
	s := ext.MCPServers["tallyfold"]
	if s.Command != "${CLAUDE_PLUGIN_ROOT}/bin/tallyfold" || !slices.Equal(s.Args, []string{"mcp"}) {
		t.Errorf("Claude Code server runs %q %v", s.Command, s.Args)
	}
	if s.Env["PLUGIN_ROOT"] != "${CLAUDE_PLUGIN_ROOT}" || s.Env["PLUGIN_DATA"] != "${CLAUDE_PLUGIN_DATA}" {
		t.Errorf("Claude Code server does not map the Agent Plugins variables: %v", s.Env)
	}

	var market struct {
		Plugins []struct {
			Name, Source, Version, MCPServers string
			Strict                            *bool `json:"strict"`
			Hooks                             map[string][]struct {
				Hooks []struct{ Command string } `json:"hooks"`
			} `json:"hooks"`
		} `json:"plugins"`
	}
	data, _ = os.ReadFile("../../.claude-plugin/marketplace.json")
	if err := json.Unmarshal(data, &market); err != nil {
		t.Fatal(err)
	}
	if len(market.Plugins) != 1 {
		t.Fatalf("marketplace lists %d plugins", len(market.Plugins))
	}
	p := market.Plugins[0]
	if p.Source != "./plugin" || p.Version != releaseVersion(t) || p.Strict == nil || *p.Strict {
		t.Errorf("marketplace entry: source %q, version %q, strict %v", p.Source, p.Version, p.Strict)
	}
	if _, err := os.Stat(filepath.Join(root, p.MCPServers)); err != nil {
		t.Errorf("marketplace mcpServers %q: %v", p.MCPServers, err)
	}
	start := p.Hooks["SessionStart"]
	if len(start) != 1 || len(start[0].Hooks) != 1 || start[0].Hooks[0].Command != "${CLAUDE_PLUGIN_ROOT}/bin/tallyfold hook session-start" {
		t.Errorf("SessionStart hook = %+v", start)
	}
}
