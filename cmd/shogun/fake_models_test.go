package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/provider"
)

// fakeModel answers every stage with a minimal valid document chosen by the output schema:
// research with one requirement covering all sources named in the prompt, a one-step outline, and
// approving reviews with complete coverage. No model is called.
type fakeModel struct{}

var reSourceLine = regexp.MustCompile(`(?m)^- ((?:repo|in|ans)-\d+): `)

// askFirst makes the research planner ask a blocking question until the answer is in its prompt.
var askFirst bool

func (fakeModel) Run(ctx context.Context, req provider.Request) (*provider.Result, error) {
	var doc any
	switch s := string(req.Schema); {
	case strings.Contains(s, `"verdict"`):
		target, refs := "FACT-001", []string{}
		switch {
		case strings.Contains(req.Prompt, "stage OUTLINE"):
			target = "S-001"
		case strings.Contains(req.Prompt, "stage DETAIL"):
			target, refs = "S-001", []string{"S-001/V-001"}
		}
		doc = map[string]any{"schema_version": 1, "verdict": "approve", "summary": "ok", "findings": []any{}, "dispositions": []any{},
			"coverage":           []any{map[string]any{"requirement_id": "R-001", "criterion_ids": []string{"R-001.C1"}, "target_ids": []string{target}, "verification_refs": refs, "status": "covered"}},
			"source_assessments": []any{}, "questions": []any{}}
	case strings.Contains(s, `"rollback_or_why_not_applicable"`):
		doc = map[string]any{"schema_version": 1, "questions": []any{}, "requested_changes": []any{}, "responses_to_findings": []any{},
			"steps": []any{map[string]any{"id": "S-001", "title": "t", "objective": "o", "requirement_ids": []string{"R-001"}, "criterion_ids": []string{"R-001.C1"},
				"depends_on": []string{}, "targets": []any{map[string]any{"repo_id": "repo-1", "path": "a.go", "operation": "modify"}},
				"actions": []string{"edit a.go"}, "verification": []any{map[string]any{"id": "V-001", "repo_id": "repo-1", "method": "command", "expected": "ok"}},
				"risks": []any{}, "rollback_or_why_not_applicable": "git revert"}}}
	case strings.Contains(s, `"approach"`):
		doc = map[string]any{"schema_version": 1, "approach": map[string]any{"summary": "s", "alternatives": []any{}},
			"steps":                            []any{map[string]any{"id": "S-001", "title": "t", "objective": "o", "deliverable": "d", "depends_on": []string{}, "criterion_ids": []string{"R-001.C1"}}},
			"final_verification_criterion_ids": []string{}, "questions": []any{}, "requested_changes": []any{}, "responses_to_findings": []any{}}
	default:
		var cov []any
		for _, m := range reSourceLine.FindAllStringSubmatch(req.Prompt, -1) {
			cov = append(cov, map[string]any{"source_id": m[1], "studied": "x", "relevance": "relevant", "notes": ""})
		}
		questions := []any{}
		if askFirst && !strings.Contains(req.Prompt, "→ semver") {
			questions = append(questions, map[string]any{"id": "Q-001", "question": "Semver or date?", "why": "w", "impact": "i", "options": []string{"semver", "date"}, "proposed_assumption": "", "blocking": true})
		}
		doc = map[string]any{"schema_version": 1,
			"facts":           []any{map[string]any{"id": "FACT-001", "source_id": "task", "location": "task", "quote": "q", "kind": "observation", "text": "t"}},
			"requirements":    []any{map[string]any{"id": "R-001", "statement": "s", "source_ids": []string{"task"}, "type": "functional", "mandatory": true, "criteria": []any{map[string]any{"id": "R-001.C1", "text": "c"}}}},
			"source_coverage": cov, "questions": questions, "web_sources": []any{}, "requested_changes": []any{}, "responses_to_findings": []any{}}
	}
	b, _ := json.Marshal(doc)
	if err := provider.ValidatePayload(req.Schema, b); err != nil {
		return nil, err
	}
	return &provider.Result{Payload: b, Attempts: 1}, nil
}

func useFakeModels(t *testing.T) {
	old := runnersHook
	runnersHook = func(ctx context.Context, cfg config.Config) (provider.Runner, provider.Runner, error) {
		return fakeModel{}, fakeModel{}, nil
	}
	t.Cleanup(func() { runnersHook = old })
}

// plan --auto stops with needs_input on a blocking question; resume --answers records the answer
// and the run continues to the approved outline.
func TestPlanNeedsInputThenResumeWithAnswers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := gitRepo(t)
	useFakeModels(t)
	askFirst = true
	defer func() { askFirst = false }()
	code, out, errs := runCLI(t, ws, "plan", "--auto", "--json", "Add a version flag")
	var res struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	json.Unmarshal([]byte(out), &res)
	if code != ExitNeedsInput || res.Status != "needs_input" {
		t.Fatalf("plan: %d %q %q", code, out, errs)
	}
	answers := filepath.Join(ws, "answers.json")
	os.WriteFile(filepath.Join(ws, "versioning.md"), []byte("use semver\n"), 0o600)
	os.WriteFile(answers, []byte(`{"schema_version":1,"answers":[{"question_id":"Q-001","answer":"semver","files":["versioning.md"]}]}`), 0o600)
	code, _, errs = runCLI(t, ws, "resume", res.RunID, "--answers", answers)
	if code != ExitError || !strings.Contains(errs, "[outline] approved") || !strings.Contains(errs, "1 answer(s) recorded") {
		t.Fatalf("resume: %d %q", code, errs)
	}
	if code, out, _ := runCLI(t, ws, "status", res.RunID); code != ExitOK || !strings.Contains(out, "stage:      integration") {
		t.Fatalf("status after resume: %q", out)
	}
	// The answer's file became a snapshot input the models were told to read.
	runDir := filepath.Join(ws, ".shogun", "runs", res.RunID)
	man, _ := os.ReadFile(filepath.Join(runDir, "manifest.json"))
	if !strings.Contains(string(man), `"ans-1"`) || !strings.Contains(errs, "ans-1 versioning.md") {
		t.Fatalf("answer file not attached: %s", man)
	}
	prompts, _ := filepath.Glob(filepath.Join(runDir, "calls", "*-research-planner", "prompt.md"))
	last, _ := os.ReadFile(prompts[len(prompts)-1])
	if !strings.Contains(string(last), "- ans-1: ") {
		t.Fatalf("answer file not in the prompt:\n%s", last)
	}
}
