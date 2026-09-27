package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseModelSpec(t *testing.T) {
	ok, err := ParseModelSpec("codex/gpt-6-astra:high")
	if err != nil || ok.Provider != "codex" || ok.Model != "gpt-6-astra" || ok.Effort != EffortHigh {
		t.Fatalf("unexpected: %+v %v", ok, err)
	}
	for _, bad := range []string{"", "fable:xhigh", "claude/fable", "claude/fable:", "openai/gpt:high", "claude/fable:ultra", "claude/a b:high"} {
		if _, err := ParseModelSpec(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestReviewerEffortFloorAndRoles(t *testing.T) {
	c := Default()
	c.Reviewer, _ = ParseModelSpec("codex/gpt-6-sol:medium")
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "below the hardcoded minimum") {
		t.Fatalf("expected effort floor error, got %v", err)
	}
	c = Default()
	c.Planner, _ = ParseModelSpec("codex/gpt-6-astra:high")
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "planner must use provider") {
		t.Fatalf("expected fixed-role error, got %v", err)
	}
}

func TestLoadPrecedenceAndPaths(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "shogun", "config.toml"),
		"planner = \"claude/opus:xhigh\"\nplans_dir = \"~/plans\"\nreview_rounds = 4\nclaude_command = \"./bin/claude\"\n")
	mustWrite(t, filepath.Join(ws, ".shogun", "config.toml"),
		"reviewer = \"codex/gpt-6-sol:xhigh\"\nproject = \"portals\"\ncall_deadline = \"45m\"\n")
	l, err := Load(ws, Overrides{MaxCalls: 77}, env(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatal(err)
	}
	c := l.Config
	if c.Planner.String() != "claude/opus:xhigh" || l.Provenance["planner"] != Source("file:"+filepath.Join(home, ".config", "shogun", "config.toml")) {
		t.Errorf("planner layering wrong: %s %s", c.Planner, l.Provenance["planner"])
	}
	if c.Reviewer.String() != "codex/gpt-6-sol:xhigh" || !strings.HasPrefix(string(l.Provenance["reviewer"]), "file:"+ws) {
		t.Errorf("workspace layer should win for reviewer: %s %s", c.Reviewer, l.Provenance["reviewer"])
	}
	if c.PlansDir != filepath.Join(home, "plans") {
		t.Errorf("~ expansion failed: %s", c.PlansDir)
	}
	if want := filepath.Join(home, ".config", "shogun", "bin", "claude"); c.ClaudeCommand != want {
		t.Errorf("relative command should resolve against config dir: %s", c.ClaudeCommand)
	}
	if c.ReviewRounds != 4 || c.CallDeadline != 45*time.Minute || c.Project != "portals" {
		t.Errorf("values not applied: %+v", c)
	}
	if c.MaxCalls != 77 || l.Provenance["max_calls"] != "flag" {
		t.Errorf("flag override lost: %d %s", c.MaxCalls, l.Provenance["max_calls"])
	}
	if l.Provenance["detail_batch"] != "default" {
		t.Errorf("untouched key must stay default")
	}
}

func TestLoadUnknownKeyIsError(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, ".shogun", "config.toml"), "planner = \"claude/fable:xhigh\"\nreviewer_effort = \"low\"\n")
	_, err := Load(ws, Overrides{}, env(map[string]string{"HOME": t.TempDir()}))
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("expected unknown key error, got %v", err)
	}
}

func TestLoadInvalidValuesAreErrors(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, ".shogun", "config.toml"), "reviewer = \"codex/gpt-6-astra:low\"\n")
	if _, err := Load(ws, Overrides{}, env(map[string]string{"HOME": t.TempDir()})); err == nil {
		t.Fatal("reviewer below floor must fail")
	}
	mustWrite(t, filepath.Join(ws, ".shogun", "config.toml"), "detail_batch = 0\n")
	if _, err := Load(ws, Overrides{}, env(map[string]string{"HOME": t.TempDir()})); err == nil {
		t.Fatal("detail_batch 0 must fail")
	}
	mustWrite(t, filepath.Join(ws, ".shogun", "config.toml"), "project = \"../x\"\n")
	if _, err := Load(ws, Overrides{}, env(map[string]string{"HOME": t.TempDir()})); err == nil {
		t.Fatal("unsafe project must fail")
	}
}

func TestDefaultsOnlyWhenNoFiles(t *testing.T) {
	l, err := Load(t.TempDir(), Overrides{}, env(map[string]string{"HOME": t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Files) != 0 || l.Config.Planner.String() != "claude/opus:high" || l.Config.Reviewer.String() != "codex/gpt-6-astra:high" {
		t.Fatalf("defaults wrong: %+v", l.Config)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	dir := filepath.Join(home, "my plans dir", "with spaces")
	mustWrite(t, filepath.Join(ws, ".shogun", "config.toml"),
		"plans_dir = \""+dir+"\"\ncodex_command = \"/Applications/ChatGPT.app/Contents/Resources/codex\"\ncall_deadline = \"45m\"\nmax_time = \"3h30m\"\nreview_rounds = 4\nproject = \"demo-1\"\n")
	l, err := Load(ws, Overrides{MaxCalls: 12}, env(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatal(err)
	}
	data, err := l.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.snapshot.toml")
	os.WriteFile(p, data, 0o600)
	back, err := LoadSnapshot(p)
	if err != nil {
		t.Fatalf("snapshot must be loadable TOML: %v\n%s", err, data)
	}
	if *back != l.Config {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", *back, l.Config)
	}
	if !strings.Contains(string(data), "# provenance: max_calls = flag") {
		t.Fatalf("provenance comments missing:\n%s", data)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
