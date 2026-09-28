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

// Live 2026-09-27 (P6 self-run, stats --json): the reviewer approved revision 2 citing only the test
// step's verifications for criteria that the implementation step also carries; the per-step gate
// paused the run at the round cap. Replayed offline, the same four answers publish a valid plan.
func TestReplayStatsJSONSelfRunPublishes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	ws := gitRepo(t)
	data := filepath.Join("testdata", "live-2026-09-27-stats-json")
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
