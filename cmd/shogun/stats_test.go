package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/run"
)

// stats reads every run, stopped and damaged ones included, and never writes to a run.
func TestStatsCountsEveryRunAndFlagsMissingUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	root := run.RunsRoot(ws)
	mk := func(id string, status run.Status, c run.Counters) {
		r, err := run.Create(root, id)
		if err != nil {
			t.Fatal(err)
		}
		st := run.NewState(id, time.Now())
		st.Status, st.Counters = status, c
		if err := r.SaveState(st, time.Now()); err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Join(r.Dir, "plan"), 0o700)
		os.WriteFile(filepath.Join(r.Dir, "plan", "1.json"), []byte("{}"), 0o600)
		os.WriteFile(filepath.Join(r.Dir, "config.snapshot.toml"), []byte("planner = \"claude/opus:high\"\nreviewer = \"codex/gpt-6-astra:high\"\n"), 0o600)
	}
	mk("20260927-000001-a", run.StatusApproved, run.Counters{Attempts: 2, ActiveSeconds: 150, InputTokens: 100, CacheReadTokens: 1000,
		OutputTokens: 50, ReasoningTokens: 20, CostUSD: 0.5})
	mk("20260927-000002-b", run.StatusPaused, run.Counters{Attempts: 4, ActiveSeconds: 600}) // no usage recorded
	// An old fast run that stopped at intake: no mode, no cursor, no drafts, but fast limits.
	mk("20260927-000004-d", run.StatusNeedsInput, run.Counters{})
	edit := func(id string, f func(*run.State)) {
		r, _ := run.Open(filepath.Join(root, id))
		st, _ := r.LoadState()
		f(st)
		r.SaveState(st, time.Now())
		os.RemoveAll(filepath.Join(root, id, "plan"))
	}
	edit("20260927-000004-d", func(st *run.State) { st.Limits.Source = "fast" })
	// An old record with nothing to tell the mode by.
	mk("20260927-000005-e", run.StatusFailed, run.Counters{})
	edit("20260927-000005-e", func(st *run.State) {})
	os.MkdirAll(filepath.Join(root, "20260927-000003-c"), 0o700)
	os.WriteFile(filepath.Join(root, "20260927-000003-c", "state.json"), []byte("{"), 0o600)
	before, _ := os.ReadFile(filepath.Join(root, "20260927-000001-a", "state.json"))

	code, out, errs := runCLI(t, ws, "stats")
	if code != ExitOK {
		t.Fatalf("stats: %d %q", code, errs)
	}
	for _, want := range []string{"claude/opus:high", "approved", "paused", "damaged", "unknown",
		"5 run(s) (1 approved, 1 failed, 1 needs_input, 1 paused, 1 damaged); at least 6 attempt(s), at least 12.5 min active",
		"at least 100 input, 1000 cache read, 0 cache write, 50 output (20 of it reasoning)", "at least $0.50",
		"unknown in 1 run(s), 1 damaged"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	mode := func(id string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, id) {
				return strings.Fields(l)[2]
			}
		}
		return ""
	}
	if mode("20260927-000004-d") != "fast" || mode("20260927-000005-e") != "unknown" || mode("20260927-000001-a") != "fast" {
		t.Fatalf("modes:\n%s", out)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "20260927-000001-a", "state.json")); string(after) != string(before) {
		t.Fatal("stats changed a run")
	}

	// --json: the same data as one document; every run object has every key, damaged runs with null.
	code, out, errs = runCLI(t, ws, "stats", "--json")
	if code != ExitOK || strings.Contains(out, "CLAUDE $") || strings.Contains(out, "total:") {
		t.Fatalf("stats --json: %d %q %q", code, out, errs)
	}
	var doc struct {
		Runs   []map[string]any `json:"runs"`
		Totals map[string]any   `json:"totals"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stats --json is not one JSON document: %v\n%s", err, out)
	}
	if len(doc.Runs) != 5 || doc.Totals == nil {
		t.Fatalf("runs %d totals %v", len(doc.Runs), doc.Totals)
	}
	keys := []string{"id", "status", "mode", "planner", "reviewer", "attempts", "active_seconds", "input_tokens", "cache_read_tokens",
		"cache_write_tokens", "output_tokens", "reasoning_tokens", "cost_usd", "usage", "damaged"}
	byID := map[string]map[string]any{}
	for i, r := range doc.Runs {
		for _, k := range keys {
			if _, ok := r[k]; !ok {
				t.Fatalf("run %d lacks key %q: %v", i, k, r)
			}
		}
		if i > 0 && doc.Runs[i-1]["id"].(string) > r["id"].(string) {
			t.Fatalf("runs not sorted by id: %v", doc.Runs)
		}
		byID[r["id"].(string)] = r
	}
	a := byID["20260927-000001-a"]
	for k, want := range map[string]any{"status": "approved", "mode": "fast", "planner": "claude/opus:high", "reviewer": "codex/gpt-6-astra:high",
		"attempts": 2.0, "active_seconds": 150.0, "input_tokens": 100.0, "cache_read_tokens": 1000.0, "cache_write_tokens": 0.0,
		"output_tokens": 50.0, "reasoning_tokens": 20.0, "cost_usd": 0.5, "usage": "ok", "damaged": nil} {
		if a[k] != want {
			t.Fatalf("run a %s = %v, want %v", k, a[k], want)
		}
	}
	if byID["20260927-000002-b"]["usage"] != "unknown" {
		t.Fatalf("run b usage %v", byID["20260927-000002-b"]["usage"])
	}
	d := byID["20260927-000003-c"]
	for _, k := range keys[2:14] { // mode … usage
		if v, ok := d[k]; !ok || v != nil {
			t.Fatalf("damaged run %s = %v (present %v), want null", k, v, ok)
		}
	}
	if d["status"] != "damaged" || d["damaged"] == nil || d["damaged"].(string) == "" {
		t.Fatalf("damaged run: %v", d)
	}
	for k, want := range map[string]any{"runs": 5.0, "damaged": 1.0, "attempts": 6.0, "active_seconds": 750.0, "input_tokens": 100.0,
		"cache_read_tokens": 1000.0, "cache_write_tokens": 0.0, "output_tokens": 50.0, "reasoning_tokens": 20.0, "cost_usd": 0.5,
		"spend_lower_bound": true, "tokens_lower_bound": true} {
		if doc.Totals[k] != want {
			t.Fatalf("totals %s = %v, want %v", k, doc.Totals[k], want)
		}
	}
	if bs, _ := doc.Totals["by_status"].(map[string]any); len(bs) != 4 || bs["approved"] != 1.0 || bs["failed"] != 1.0 || bs["needs_input"] != 1.0 || bs["paused"] != 1.0 {
		t.Fatalf("by_status %v", doc.Totals["by_status"])
	}
	if after, _ := os.ReadFile(filepath.Join(root, "20260927-000001-a", "state.json")); string(after) != string(before) {
		t.Fatal("stats --json changed a run")
	}
}

// An empty runs directory: --json still prints one document (empty runs, zero totals, exit 0);
// the text mode keeps its stderr note and empty stdout.
func TestStatsJSONEmptyDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	if err := os.MkdirAll(run.RunsRoot(ws), 0o700); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runCLI(t, ws, "stats", "--json")
	if code != ExitOK || strings.Contains(errs, "no runs in") {
		t.Fatalf("stats --json on an empty dir: %d %q %q", code, out, errs)
	}
	var doc struct {
		Runs   []any          `json:"runs"`
		Totals map[string]any `json:"totals"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, out)
	}
	if doc.Runs == nil || len(doc.Runs) != 0 {
		t.Fatalf("runs = %v, want []", doc.Runs)
	}
	if bs, ok := doc.Totals["by_status"].(map[string]any); !ok || len(bs) != 0 {
		t.Fatalf("by_status = %v, want {}", doc.Totals["by_status"])
	}
	for _, k := range []string{"runs", "damaged", "attempts", "active_seconds", "input_tokens", "cache_read_tokens", "cache_write_tokens",
		"output_tokens", "reasoning_tokens", "cost_usd"} {
		if v, ok := doc.Totals[k]; !ok || v != 0.0 {
			t.Fatalf("totals %s = %v (present %v), want 0", k, v, ok)
		}
	}
	if doc.Totals["spend_lower_bound"] != false || doc.Totals["tokens_lower_bound"] != false {
		t.Fatalf("lower-bound flags: %v", doc.Totals)
	}
	code, out, errs = runCLI(t, ws, "stats")
	if code != ExitOK || out != "" || !strings.Contains(errs, "no runs in") {
		t.Fatalf("text mode on an empty dir: %d %q %q", code, out, errs)
	}
}
