package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/provider"
)

// A closed-choice input error must be corrected without spending the repair pair.
// The exact recorded responses still suffice after the user supplies a valid choice.
func TestPR10Round2ReviewInvalidSourceChoiceDoesNotSpend(t *testing.T) {
	oldInteractive := interactive
	interactive = func() bool { return false }
	t.Cleanup(func() { interactive = oldInteractive })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	ws := gitRepo(t)
	data := filepath.Join("testdata", "live-2026-09-28-stats-json-input")
	planner := loadReplay(t, data, "0001-plan-planner", "0003-plan-planner")
	reviewer := loadReplay(t, data, "0002-plan-reviewer", "0004-plan-reviewer")
	old := runnersHook
	runnersHook = func(ctx context.Context, cfg config.Config) (provider.Runner, provider.Runner, error) {
		return planner, reviewer, nil
	}
	t.Cleanup(func() { runnersHook = old })
	task, _ := filepath.Abs(filepath.Join(data, "task.md"))
	input, _ := filepath.Abs(filepath.Join(data, "prior-plan.md"))
	code, out, errs := runCLI(t, ws, "plan", "--auto", "--json", "--input", input, "--task-file", task)
	var res struct {
		RunID        string `json:"run_id"`
		Status, Path string
	}
	json.Unmarshal([]byte(out), &res)
	if code != ExitNeedsInput || res.Status != "needs_input" || len(planner.payloads) != 1 || len(reviewer.payloads) != 1 {
		t.Fatalf("plan: %d %q (planner answers left %d)\n%s", code, out, len(planner.payloads), errs)
	}
	_, st, _ := runCLI(t, ws, "status", res.RunID, "--json")
	var state struct {
		Progress struct {
			Pending []struct{ ID, Question, Origin string }
		}
	}
	json.Unmarshal([]byte(st), &state)
	if len(state.Progress.Pending) != 1 || state.Progress.Pending[0].Origin != "shogun" || !strings.Contains(state.Progress.Pending[0].Question, "explicit source in-1") {
		t.Fatalf("pending: %+v", state.Progress.Pending)
	}
	answers := filepath.Join(t.TempDir(), "answers.json")
	os.WriteFile(answers, []byte(`{"schema_version":1,"answers":[{"question_id":"`+state.Progress.Pending[0].ID+`","answer":"reference","files":[]}]}`), 0o600)
	badCode, _, badErrs := runCLI(t, ws, "resume", res.RunID, "--answers", answers)
	remaining := len(planner.payloads) + len(reviewer.payloads)
	_, st, _ = runCLI(t, ws, "status", res.RunID, "--json")
	state.Progress.Pending = nil
	if err := json.Unmarshal([]byte(st), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Progress.Pending) != 1 {
		t.Fatalf("invalid source choice must leave a question pending: code=%d pending=%+v\n%s", badCode, state.Progress.Pending, badErrs)
	}
	os.WriteFile(answers, []byte(`{"schema_version":1,"answers":[{"question_id":"`+state.Progress.Pending[0].ID+`","answer":"1","files":[]}]}`), 0o600)
	code, _, errs = runCLI(t, ws, "resume", res.RunID, "--answers", answers)
	if remaining != 2 || code != ExitOK {
		t.Fatalf("bad choice consumed the remaining pair: payloads left after invalid answer=%d (want 2), corrected resume exit=%d (want 0)\ninvalid resume:\n%s\ncorrected resume:\n%s", remaining, code, badErrs, errs)
	}
	_, st, _ = runCLI(t, ws, "status", res.RunID, "--json")
	var done struct{ Publish struct{ Path string } }
	json.Unmarshal([]byte(st), &done)
	if code, out, _ := runCLI(t, ws, "verify", done.Publish.Path); code != ExitOK || !strings.HasPrefix(out, "valid") {
		t.Fatalf("verify: %d %q", code, out)
	}
}
