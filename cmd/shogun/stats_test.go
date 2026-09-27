package main

import (
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
}
