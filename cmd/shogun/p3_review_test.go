package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/pipeline"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

func TestP3ReviewResumeCannotSkipMissingExplicitInput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := gitRepo(t)
	useFakeModels(t)
	code, _, errs := runCLI(t, ws, "plan", "--auto", "--input", "missing-mandatory-spec.md", "Add a version flag")
	if code != ExitNeedsInput {
		t.Fatalf("intake control: code=%d stderr=%s", code, errs)
	}
	runs, err := os.ReadDir(filepath.Join(ws, ".shogun", "runs"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("find run: %v %v", err, runs)
	}
	code, _, errs = runCLI(t, ws, "resume", runs[0].Name())
	if code != ExitNeedsInput || strings.Contains(errs, "[research] approved") || strings.Contains(errs, "[outline] approved") {
		t.Fatalf("resume bypassed unavailable mandatory input: code=%d stderr=%s", code, errs)
	}
}

type p3ReviewStopRunner struct{}

func (p3ReviewStopRunner) Run(context.Context, provider.Request) (*provider.Result, error) {
	return nil, &provider.Error{Class: provider.ClassCanceled, Msg: "stop after recording the answer"}
}

func TestP3ReviewEnterAcceptsRecommendedAnswer(t *testing.T) {
	for _, blocking := range []bool{true, false} {
		t.Run(map[bool]string{true: "blocking", false: "nonblocking"}[blocking], func(t *testing.T) {
			r, err := run.Create(run.RunsRoot(t.TempDir()), "20260926-000000-review-abcd")
			if err != nil {
				t.Fatal(err)
			}
			st := run.NewState("20260926-000000-review-abcd", time.Now())
			st.Progress.Pending = []run.Pending{{ID: "Q-001", Blocking: blocking, Question: "Version format?", ProposedAssumption: "semver"}}
			e := pipeline.Engine{Run: r, State: st, Manifest: &inputs.Manifest{}, Task: "Add a version flag", Cfg: config.Default(),
				Planner: p3ReviewStopRunner{}, Asker: &terminalAsker{in: bufio.NewReader(strings.NewReader("\n")), out: io.Discard}, Now: time.Now}
			o := e.Execute(context.Background())
			var ds []pipeline.Decision
			b, err := os.ReadFile(filepath.Join(r.Dir, "decisions.json"))
			if err == nil {
				err = json.Unmarshal(b, &ds)
			}
			if err != nil || len(ds) != 1 || ds[0].Source != "user" || ds[0].Answer != "semver" {
				t.Fatalf("Enter did not record the offered recommendation as a user decision: outcome=%+v decisions=%v error=%v", o, ds, err)
			}
		})
	}
}
