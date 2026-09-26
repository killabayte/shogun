package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/killabayte/shogun/internal/run"
	toml "github.com/pelletier/go-toml/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestP1ReviewDocumentedTrailingFlags(t *testing.T) {
	ws := t.TempDir()
	r, err := run.Create(run.RunsRoot(ws), "test-run")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.SaveState(run.NewState("test-run", time.Now()), time.Now()); err != nil {
		t.Fatal(err)
	}
	answers := filepath.Join(ws, "answers.json")
	os.WriteFile(answers, []byte(`{"schema_version":1,"answers":[{"question_id":"Q-001","answer":"yes"}]}`), 0600)
	for _, name := range []string{"status", "resume"} {
		t.Run(name, func(t *testing.T) {
			var out, errs bytes.Buffer
			a := app{ctx: context.Background(), cwd: ws, stdout: &out, stderr: &errs}
			if name == "status" {
				code := a.cmdStatus([]string{r.Dir, "--json"})
				if code != 0 {
					t.Fatalf("documented status syntax rejected: %s", errs.String())
				}
			}
			if name == "resume" {
				a.cmdResume([]string{r.Dir, "--answers", answers})
				if !bytes.Contains(errs.Bytes(), []byte("answers file is valid")) {
					t.Fatalf("documented resume syntax never validates answers: %s", errs.String())
				}
			}
		})
	}
}

func TestP1ReviewSnapshotIsTOML(t *testing.T) {
	ws := t.TempDir()
	var out, errs bytes.Buffer
	a := app{ctx: context.Background(), cwd: ws, stdout: &out, stderr: &errs, getenv: func(string) string { return "" }, now: time.Now}
	a.cmdPlan([]string{"--json", "Task"})
	var res struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err, errs.String())
	}
	data, err := os.ReadFile(filepath.Join(run.RunsRoot(ws), res.RunID, "config.snapshot.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := toml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("persisted config snapshot is not loadable TOML: %v", err)
	}
}
