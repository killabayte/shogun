package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

// Multiple requests must invalidate from the earliest dependency-ordered step,
// independently of the order in which the planner lists its requests.
func TestP4ReviewMultipleRequestedChangesReopenEarliest(t *testing.T) {
	_, _, steps := chain(3)
	d := withDeps(stepBatch(steps[2]))
	d["requested_changes"] = []any{
		map[string]any{"target_id": "S-002", "reason": "S-002 needs a changed output"},
		map[string]any{"target_id": "S-001", "reason": "S-001 must produce that output first"},
	}
	f, _ := newChain(t, 3, []reply{batch(steps[0]), batch(steps[1]), fixed(d),
		func(provider.Request) (any, error) {
			return nil, &provider.Error{Class: provider.ClassRateLimit, Msg: "stop after observing the reopened cursor", Attempts: 1}
		}}, []reply{fixed(stepReview("approve", nil, steps[0])), fixed(stepReview("approve", nil, steps[1]))})
	o := f.execute(t)
	if o.Status != run.StatusPaused || !strings.Contains(o.Reason, "rate_limit") {
		t.Fatalf("control stop failed: %+v", o)
	}
	if f.e.State.Cursor.Step != "S-001" || len(f.e.State.Progress.Accepted) != 0 {
		t.Fatalf("earliest requested change was skipped: cursor=%+v accepted=%v", f.e.State.Cursor, f.e.State.Progress.Accepted)
	}
}

// Merely mentioning both steps in one requirement row cannot replace evidence
// for the second step's assigned criterion.
func TestP4ReviewEveryBatchStepNeedsVerificationEvidence(t *testing.T) {
	_, _, steps := chain(2)
	rv := stepReview("approve", nil, steps...)
	rv["coverage"].([]any)[0].(map[string]any)["verification_refs"] = []string{"S-001/V-001"}
	f, _ := newChain(t, 2, []reply{batch(steps...)}, []reply{fixed(rv)})
	f.e.Cfg.DetailBatch, f.e.Cfg.ReviewRounds = 2, 1
	o := f.execute(t)
	if len(f.e.State.Progress.Accepted) != 0 {
		t.Fatalf("batch approved without S-002 verification evidence: outcome=%+v accepted=%v", o, f.e.State.Progress.Accepted)
	}
}

// All cited IDs exist, but each requirement is attributed to the other step.
func TestP4ReviewBatchCoverageCannotCrossWireRequirements(t *testing.T) {
	s1, s2 := step{"S-001", r1.crit}, step{"S-002", r2.crit}
	rv := stepReview("approve", nil, s1, s2)
	rows := rv["coverage"].([]any)
	rows[0].(map[string]any)["target_ids"] = []string{"S-002"}
	rows[0].(map[string]any)["verification_refs"] = []string{"S-002/V-001"}
	rows[1].(map[string]any)["target_ids"] = []string{"S-001"}
	rows[1].(map[string]any)["verification_refs"] = []string{"S-001/V-001"}
	f := newFixture(t,
		[]reply{fixed(research(r1, r2)), fixed(outline(s1, s2)), fixed(stepBatch(s1, s2))},
		[]reply{fixed(review("approve", []row{
			{"R-001", "covered", r1.crit, []string{"FACT-001"}},
			{"R-002", "covered", r2.crit, []string{"FACT-001"}},
		}, nil)), fixed(review("approve", []row{
			{"R-001", "covered", r1.crit, []string{"S-001"}},
			{"R-002", "covered", r2.crit, []string{"S-002"}},
		}, nil)), fixed(rv)})
	f.e.Cfg.DetailBatch, f.e.Cfg.ReviewRounds = 2, 1
	o := f.execute(t)
	if len(f.e.State.Progress.Accepted) != 0 {
		t.Fatalf("cross-wired requirement evidence approved: outcome=%+v accepted=%v", o, f.e.State.Progress.Accepted)
	}
}

// An archived essential source is in the must-read prompt, so its bytes must
// count before the reviewer is invoked, just like an explicit input's bytes.
func TestP4ReviewContextIncludesArchivedEssentialSource(t *testing.T) {
	const url = "https://example.org/required-spec"
	d := research(r1)
	d["web_sources"] = []any{map[string]any{"url": url, "role": "essential", "related_ids": []string{"R-001"}}}
	f := newFixture(t, []reply{fixed(d)}, []reply{func(provider.Request) (any, error) {
		return nil, &provider.Error{Class: provider.ClassRateLimit, Msg: "reviewer should not have been called", Attempts: 1}
	}})
	f.e.Cfg.MaxContextTokens = 8192
	f.e.Fetch = func(context.Context, string, []string) ([]inputs.Source, error) {
		if err := f.e.Run.WriteArtifact("web/spec.txt", []byte(strings.Repeat("required specification\n", 20000))); err != nil {
			t.Fatal(err)
		}
		return []inputs.Source{{ID: "web-1", Origin: url, Status: "ok", StoredPath: filepath.Join("web", "spec.txt")}}, nil
	}
	o := f.execute(t)
	if o.Status != run.StatusPaused || !strings.HasPrefix(o.Reason, "context_limit:") || calls(f.reviewer) != 0 {
		t.Fatalf("oversized mandatory archive ignored: outcome=%+v reviewer_calls=%d (archive=460000 bytes, threshold=8192 tokens)", o, calls(f.reviewer))
	}
}

// A revision call reads both the earlier document and its current findings.
// Each fits alone; their combined size exceeds the configured threshold.
func TestP4ReviewContextIncludesPreviousPlannerRevision(t *testing.T) {
	_, _, steps := chain(1)
	d := withDeps(stepBatch(steps...))
	d["steps"].([]any)[0].(map[string]any)["actions"] = []string{strings.Repeat("action ", 2200)}
	f, _ := newChain(t, 1,
		[]reply{fixed(d), func(provider.Request) (any, error) {
			return nil, &provider.Error{Class: provider.ClassRateLimit, Msg: "revision planner should not have been called", Attempts: 1}
		}},
		[]reply{fixed(stepReview("revise", []map[string]any{finding("S-001", "major", strings.Repeat("revise ", 1700))}, steps...))})
	f.e.Cfg.MaxContextTokens = 8192
	o := f.execute(t)
	if o.Status != run.StatusPaused || !strings.HasPrefix(o.Reason, "context_limit:") || calls(f.planner) != 3 {
		t.Fatalf("previous revision omitted from context: outcome=%+v planner_calls=%d (want 3)", o, calls(f.planner))
	}
}

// A finding on the batch's shared requirement must survive the split of that
// batch. It cannot disappear from both child units solely because their keys differ.
func TestP4ReviewBatchSplitPreservesOpenFindings(t *testing.T) {
	_, _, steps := chain(2)
	large := withDeps(stepBatch(steps...))
	large["steps"].([]any)[0].(map[string]any)["actions"] = []string{strings.Repeat("action ", 20000)}
	f, _ := newChain(t, 2,
		[]reply{batch(steps...), fixed(large), batch(steps[0]), batch(steps[1])},
		[]reply{
			fixed(stepReview("revise", []map[string]any{finding("R-001", "major", "the two steps disagree about the required version format")}, steps...)),
			fixed(stepReview("approve", nil, steps[0])),
			fixed(stepReview("approve", nil, steps[1])),
		})
	f.e.Cfg.DetailBatch, f.e.Cfg.MaxContextTokens = 2, 8192
	// Bound the child units too so the test stops if they correctly reject the
	// empty dispositions rather than requesting a further scripted answer.
	f.reviewer.replies[3] = func(provider.Request) (any, error) {
		f.e.Cfg.ReviewRounds = 1
		return stepReview("approve", nil, steps[0]), nil
	}
	o := f.execute(t)
	if f.e.State.Progress.BatchSize != 1 {
		t.Fatalf("control batch was not split: %+v\n%s", o, f.log.String())
	}
	if f.e.State.Cursor.Stage == StageIntegration || f.e.State.Progress.Accepted["S-001"] != 0 {
		t.Fatalf("split batch forgot an unresolved finding: outcome=%+v accepted=%v open=%v", o, f.e.State.Progress.Accepted, openFindings(&f.e.State.Progress))
	}
}
