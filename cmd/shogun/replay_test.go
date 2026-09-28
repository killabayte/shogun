package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/provider"
)

// replay answers each role with the recorded payloads of a live run, in order.
type replay struct {
	mu       sync.Mutex
	payloads [][]byte
}

func (r *replay) Run(ctx context.Context, req provider.Request) (*provider.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.payloads) == 0 {
		return nil, &provider.Error{Class: provider.ClassConfig, Msg: "replay: no recorded answer left"}
	}
	b := r.payloads[0]
	r.payloads = r.payloads[1:]
	if err := provider.ValidatePayload(req.Schema, b); err != nil {
		return nil, err
	}
	return &provider.Result{Payload: b, Attempts: 1, Usages: []json.RawMessage{json.RawMessage(`{"input_tokens":1,"output_tokens":1}`)}}, nil
}

func loadReplay(t *testing.T, dir string, names ...string) *replay {
	r := &replay{}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n+".json"))
		if err != nil {
			t.Fatal(err)
		}
		r.payloads = append(r.payloads, b)
	}
	return r
}

// Live P6 self-runs (stats --json), each paused at the round cap after the reviewer approved revision
// 2. 2026-09-27: the reviewer cited only the test step's verifications for criteria the
// implementation step also carries (the per-step gate). 2026-09-28: the reviewer listed the
// implementation step as an extra target of the read-only constraint only the test step carries,
// with the correct verification cited. Replayed offline, both sets of four answers publish a plan
// that verifies as valid.
func TestReplayStatsJSONSelfRunsPublish(t *testing.T) {
	for _, run := range []string{"live-2026-09-27-stats-json", "live-2026-09-28-stats-json"} {
		t.Run(run, func(t *testing.T) { replayPublishes(t, filepath.Join("testdata", run)) })
	}
}

func replayPublishes(t *testing.T, data string) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	ws := gitRepo(t)
	planner := loadReplay(t, data, "0001-plan-planner", "0003-plan-planner")
	reviewer := loadReplay(t, data, "0002-plan-reviewer", "0004-plan-reviewer")
	old := runnersHook
	runnersHook = func(ctx context.Context, cfg config.Config) (provider.Runner, provider.Runner, error) {
		return planner, reviewer, nil
	}
	t.Cleanup(func() { runnersHook = old })
	task, _ := filepath.Abs(filepath.Join(data, "task.md"))
	code, out, errs := runCLI(t, ws, "plan", "--auto", "--json", "--task-file", task)
	var res struct{ Status, Path string }
	json.Unmarshal([]byte(out), &res)
	if code != ExitOK || res.Status != "approved" || !strings.Contains(errs, "[plan] r1: reviewer revise") || !strings.Contains(errs, "[plan] approved at r2") {
		t.Fatalf("replay: %d %q\n%s", code, out, errs)
	}
	if code, out, _ := runCLI(t, ws, "verify", res.Path); code != ExitOK || !strings.HasPrefix(out, "valid") {
		t.Fatalf("verify: %d %q", code, out)
	}
	if len(planner.payloads)+len(reviewer.payloads) != 0 {
		t.Fatal("not every recorded answer was used")
	}
}

// Live 2026-09-28 (P6 self-run with the previous plan as --input): both reviews approved and marked
// the input contradicted, which the planner cannot change; the run spent its second round and paused.
// Replayed, the contradiction becomes one blocking question right after the first review (no second
// planner call); once the user answers, the same answers publish a plan that verifies as valid and the
// repeated verdict is not asked again.
func TestReplayContradictedInputAsksTheUserOnce(t *testing.T) {
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
	os.WriteFile(answers, []byte(`{"schema_version":1,"answers":[{"question_id":"`+state.Progress.Pending[0].ID+`","answer":"1","files":[]}]}`), 0o600)
	code, _, errs = runCLI(t, ws, "resume", res.RunID, "--answers", answers)
	if code != ExitOK || !strings.Contains(errs, "[plan] approved at r2") || strings.Contains(errs, "needs_input") {
		t.Fatalf("resume: %d\n%s", code, errs)
	}
	_, st, _ = runCLI(t, ws, "status", res.RunID, "--json")
	var done struct {
		Publish struct{ Path string }
	}
	json.Unmarshal([]byte(st), &done)
	if code, out, _ := runCLI(t, ws, "verify", done.Publish.Path); code != ExitOK || !strings.HasPrefix(out, "valid") {
		t.Fatalf("verify: %d %q", code, out)
	}
	if len(planner.payloads)+len(reviewer.payloads) != 0 {
		t.Fatal("not every recorded answer was used")
	}
}

// The same live answers with the earlier plan given as --reference: the precedence of the task is
// stated up front, so the first approving review publishes; nothing is asked.
func TestReplayReferenceInputPublishesAtTheFirstReview(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	ws := gitRepo(t)
	data := filepath.Join("testdata", "live-2026-09-28-stats-json-input")
	planner := loadReplay(t, data, "0001-plan-planner")
	reviewer := loadReplay(t, data, "0002-plan-reviewer")
	old := runnersHook
	runnersHook = func(ctx context.Context, cfg config.Config) (provider.Runner, provider.Runner, error) {
		return planner, reviewer, nil
	}
	t.Cleanup(func() { runnersHook = old })
	task, _ := filepath.Abs(filepath.Join(data, "task.md"))
	ref, _ := filepath.Abs(filepath.Join(data, "prior-plan.md"))
	code, out, errs := runCLI(t, ws, "plan", "--auto", "--json", "--reference", ref, "--task-file", task)
	var res struct{ Status, Path string }
	json.Unmarshal([]byte(out), &res)
	if code != ExitOK || res.Status != "approved" || !strings.Contains(errs, "[plan] approved at r1") {
		t.Fatalf("plan: %d %q\n%s", code, out, errs)
	}
	if code, out, _ := runCLI(t, ws, "verify", res.Path); code != ExitOK || !strings.HasPrefix(out, "valid") {
		t.Fatalf("verify: %d %q", code, out)
	}
}
