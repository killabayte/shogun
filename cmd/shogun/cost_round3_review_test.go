package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/pipeline"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

func costRound3PausedRun(t *testing.T) (string, *run.Run, *run.State) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ws := gitRepo(t)
	useFakeModels(t)
	code, out, errs := runCLI(t, ws, "plan", "--auto", "--json", "Add version")
	if code != ExitOK {
		t.Fatalf("setup plan: %d %s", code, errs)
	}
	var reply struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatal(err)
	}
	r, err := run.Open(filepath.Join(ws, ".shogun", "runs", reply.RunID))
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	st.Status, st.Reason = run.StatusPaused, "limit: active time used"
	st.Publish.Done = false
	st.Cursor = run.Cursor{Stage: pipeline.StagePlan}
	st.Limits.MaxActiveSeconds, st.Counters.ActiveSeconds = 600, 600
	if err := r.SaveState(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	return ws, r, st
}

func TestCostRound3ResumeAppliesRaisedTimeBeforePreparation(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		name := "resume"
		if refresh {
			name = "refresh"
		}
		t.Run(name, func(t *testing.T) {
			ws, r, before := costRound3PausedRun(t)
			reached := false
			runnersHook = func(context.Context, config.Config) (provider.Runner, provider.Runner, error) {
				reached = true
				return nil, nil, errors.New("test stops before any provider is started")
			}
			args := []string{"resume", before.RunID, "--max-time", "20m"}
			if refresh {
				args = append(args, "--refresh")
			}
			_, _, errs := runCLI(t, ws, args...)
			after, err := r.LoadState()
			if err != nil {
				t.Fatal(err)
			}
			if !reached || after.Limits.MaxActiveSeconds != 1200 {
				t.Errorf("new allowance was not applied before preparation: reached=%v cap=%.0f stderr=%s", reached, after.Limits.MaxActiveSeconds, errs)
			}
			if after.Counters.Attempts != before.Counters.Attempts || after.Counters.ActiveSeconds < before.Counters.ActiveSeconds {
				t.Errorf("resume reset consumption: before=%+v after=%+v", before.Counters, after.Counters)
			}
		})
	}
}

func TestCostRound3ExpiredResumeIsLimitExit(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		name := "resume"
		if refresh {
			name = "refresh"
		}
		t.Run(name, func(t *testing.T) {
			ws, _, st := costRound3PausedRun(t)
			args := []string{"resume", st.RunID}
			if refresh {
				args = append(args, "--refresh")
			}
			code, _, errs := runCLI(t, ws, args...)
			if code != ExitLimit {
				t.Errorf("expired allowance returned %d instead of limit exit %d: %s", code, ExitLimit, errs)
			}
		})
	}
}
