package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests feed streams recorded from the real CLIs in P0–P2 (testdata/) to the parsers.

func claudeSchemaFromArgv(t *testing.T, runJSON string) []byte {
	var r struct{ Argv []string }
	b, err := os.ReadFile(runJSON)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(b, &r)
	for i, a := range r.Argv {
		if a == "--json-schema" {
			return []byte(r.Argv[i+1])
		}
	}
	t.Fatal("no --json-schema in " + runJSON)
	return nil
}

func TestRecordedClaude(t *testing.T) {
	td := "testdata/"
	// Single-line result file → stream file.
	unknownModel := filepath.Join(t.TempDir(), "stdout.jsonl")
	b, _ := os.ReadFile(td + "claude/unknown-model.result.json")
	os.WriteFile(unknownModel, append(append([]byte{}, b...), '\n'), 0o600)

	for _, tc := range []struct {
		name, stdout, stderr string
		schema               []byte
		model                string
		exit                 int
		class                Class
		degraded             string
	}{
		{"probe-xhigh success", td + "p0a/claude-probe-xhigh/stdout.jsonl", td + "p0a/claude-probe-xhigh/stderr.log",
			claudeSchemaFromArgv(t, td+"p0a/claude-probe-xhigh/run.json"), "fable", 0, "", ""},
		{"exact model id", td + "p0a/claude-probe-xhigh/stdout.jsonl", "", []byte(`{}`), "claude-fable-5-1", 0, "", ""},
		{"other model requested", td + "p0a/claude-probe-xhigh/stdout.jsonl", "", []byte(`{}`), "opus", 0, ClassProtocol, ""},
		{"payload does not match schema", td + "p0a/claude-probe-xhigh/stdout.jsonl", "", []byte(testSchema), "fable", 0, ClassPayload, ""},
		{"unknown model", unknownModel, td + "claude/unknown-model.stderr.txt", []byte(`{}`), "claude-nonexistent-9", 1, ClassConfig, ""},
		{"root outside cwd denied", td + "claude/probe-outside-root-denied.stream.jsonl", "", []byte(`{}`), "fable", 0, "", "permission denied: Read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr := tc.stderr
			if stderr == "" {
				stderr = filepath.Join(t.TempDir(), "empty")
				os.WriteFile(stderr, nil, 0o600)
			}
			res, err := parseClaude(tc.stdout, stderr, procResult{exit: tc.exit}, Request{Model: tc.model, Effort: "xhigh", Schema: tc.schema})
			if tc.class != "" {
				if err == nil || err.Class != tc.class {
					t.Fatalf("got %v / %+v, want %s", err, res, tc.class)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Reported.Model != "claude-fable-5-1" || res.Reported.Effort != Unknown {
				t.Errorf("reported %+v (Claude does not report effort)", res.Reported)
			}
			if tc.degraded != "" && !strings.Contains(strings.Join(res.Degraded, "\n"), tc.degraded) {
				t.Errorf("degraded %v, want %q", res.Degraded, tc.degraded)
			}
		})
	}
}

func TestRecordedCodex(t *testing.T) {
	td := "testdata/"
	probe3, _ := os.ReadFile("../../scripts/p0a/probe3-schema.json")
	for _, tc := range []struct {
		name, dir, stdout, stderr, last string
		schema                          []byte
		model, effort                   string
		exit                            int
		class                           Class
	}{
		{"final profile smoke", "p0b/codex-final-full-agentsoff", "stdout.jsonl", "stderr.log", "last.full.json", probe3, "gpt-6-astra", "high", 0, ""},
		// Historical run: passed its old assertions and has no collab event on stdout, but stderr shows a
		// real spawn ToolCall. The adapter must reject it (§13 P2).
		{"historical spawn in stderr", "p0b/codex-final-full", "stdout.jsonl", "stderr.log", "last.full.json", probe3, "gpt-6-astra", "high", 0, ClassProtocol},
		{"spawn control", "p0b/codex-spawn-control-debug", "stdout.jsonl", "stderr.log", "last.spawn.json", []byte(`{}`), "gpt-6-astra", "high", 0, ClassProtocol},
		{"collab event on stdout", "p0b/codex154-spawn-disable", "stdout.jsonl", "stderr.log", "last.spawn.json", []byte(`{}`), "gpt-6-astra", "high", 0, ClassProtocol},
		{"effort mismatch", "p0b/codex-final-full-agentsoff", "stdout.jsonl", "stderr.log", "last.full.json", probe3, "gpt-6-astra", "xhigh", 0, ClassProtocol},
		{"model mismatch", "p0b/codex-final-full-agentsoff", "stdout.jsonl", "stderr.log", "last.full.json", probe3, "gpt-6-sol", "high", 0, ClassProtocol},
		// Recorded without RUST_LOG: no SessionConfiguredEvent → unverified → rejected.
		{"no session telemetry", "p2/codex-review", "stdout.jsonl", "stderr.log", "last.json", []byte(`{}`), "gpt-6-astra", "high", 0, ClassProtocol},
		{"unknown model", "codex", "unknown-model.events.jsonl", "unknown-model.stderr.txt", "", []byte(`{}`), "gpt-nonexistent-9", "high", 1, ClassConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := td + tc.dir + "/"
			start := time.Now()
			last := filepath.Join(t.TempDir(), "last.json")
			if tc.last != "" {
				b, _ := os.ReadFile(dir + tc.last)
				os.WriteFile(last, b, 0o600)
			}
			res, err := parseCodex(dir+tc.stdout, dir+tc.stderr, last, start, procResult{exit: tc.exit},
				Request{Model: tc.model, Effort: tc.effort, Schema: tc.schema})
			if tc.class != "" {
				if err == nil || err.Class != tc.class {
					t.Fatalf("got %v / %+v, want %s", err, res, tc.class)
				}
				if strings.Contains(tc.name, "spawn") && !strings.Contains(err.Msg, "sub-agent tool call") {
					t.Fatalf("rejected for the wrong reason: %s", err.Msg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Reported.Model != "gpt-6-astra" || res.Reported.Effort != "high" || res.Reported.ApprovalPolicy != "Never" ||
				!strings.Contains(res.Reported.PermissionProfile, "network: Restricted") {
				t.Errorf("reported %+v", res.Reported)
			}
		})
	}
}

// Unparsable telemetry (event present but fields missing) is unverified, not a pass.
func TestCodexIncompleteSessionTelemetry(t *testing.T) {
	d := t.TempDir()
	stderr := filepath.Join(d, "stderr.log")
	os.WriteFile(stderr, []byte("INFO codex_exec: Codex initialized with event: SessionConfiguredEvent { session_id: SessionId { uuid: x }\n"), 0o600)
	res := &Result{}
	b, _ := os.ReadFile(stderr)
	if err := checkSession(b, Request{Model: "gpt-6-astra", Effort: "high"}, res); err == nil || err.Class != ClassProtocol {
		t.Fatalf("got %v", err)
	}
}

// An output file older than the attempt (left from an earlier run) is never accepted.
func TestCodexStaleOutputFile(t *testing.T) {
	dir := "testdata/p0b/codex-final-full-agentsoff/"
	last := filepath.Join(t.TempDir(), "last.json")
	b, _ := os.ReadFile(dir + "last.full.json")
	os.WriteFile(last, b, 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(last, old, old)
	probe3, _ := os.ReadFile("../../scripts/p0a/probe3-schema.json")
	_, err := parseCodex(dir+"stdout.jsonl", dir+"stderr.log", last, time.Now(), procResult{},
		Request{Model: "gpt-6-astra", Effort: "high", Schema: probe3})
	if err == nil || err.Class != ClassProtocol || !strings.Contains(err.Msg, "predates") {
		t.Fatalf("got %v", err)
	}
}

// Traces of the live `doctor --live` run on 2026-09-26 (claude 2.1.283, codex 0.155.0-alpha.16.4)
// parse as successful calls under the current versions.
func TestRecordedPreflightTraces(t *testing.T) {
	d := "testdata/p2/preflight-claude/"
	res, err := parseClaude(d+"stdout.jsonl", d+"stderr.log", procResult{}, Request{Model: "fable", Effort: "xhigh", Schema: preflightSchema})
	if err != nil || res.Reported.Model != "claude-fable-5-1" {
		t.Fatalf("claude: %v %+v", err, res)
	}
	d = "testdata/p2/preflight-codex/"
	last := filepath.Join(t.TempDir(), "last.json")
	b, _ := os.ReadFile(d + "last.json")
	start := time.Now()
	os.WriteFile(last, b, 0o600)
	res, err = parseCodex(d+"stdout.jsonl", d+"stderr.log", last, start, procResult{}, Request{Model: "gpt-6-astra", Effort: "high", Schema: preflightSchema})
	if err != nil || res.Reported.Effort != "high" {
		t.Fatalf("codex: %v %+v", err, res)
	}
}

func TestCodexPolicyAndProfileMismatch(t *testing.T) {
	b, _ := os.ReadFile("testdata/p0b/codex-final-full-agentsoff/stderr.log")
	for name, edit := range map[string][2]string{
		"approval policy": {"approval_policy: Never", "approval_policy: OnRequest"},
		"writable root":   {"access: Read", "access: Write"},
		"open network":    {"network: Restricted", "network: Enabled"},
	} {
		changed := strings.Replace(string(b), edit[0], edit[1], 1)
		if changed == string(b) {
			t.Fatalf("%s: fixture has no %q", name, edit[0])
		}
		if err := checkSession([]byte(changed), Request{Model: "gpt-6-astra", Effort: "high"}, &Result{}); err == nil || err.Class != ClassProtocol {
			t.Errorf("%s: got %v", name, err)
		}
	}
}
