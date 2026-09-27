package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

func TestP3ReviewEssentialSourceAssessmentBlocksApproval(t *testing.T) {
	const url = "https://example.org/required-spec"
	d := research(r1)
	d["web_sources"] = []any{map[string]any{"url": url, "role": "supporting", "related_ids": []string{"R-001"}}}
	rev := review("approve", researchOK, nil)
	rev["source_assessments"] = []any{map[string]any{"source_id": url, "role": "essential", "verdict": "unverified", "note": "The only authoritative specification was not archived."}}
	f := newFixture(t,
		[]reply{fixed(d), fixed(outline(step{"S-001", []string{"R-001.C1"}}))},
		[]reply{fixed(rev), fixed(review("approve", outlineOK, nil))})
	f.e.Fetch = func(context.Context, string, []string) ([]inputs.Source, error) {
		return []inputs.Source{{ID: "web-1", Origin: url, Status: "error", Error: "HTTP 403"}}, inputs.ErrUnavailable
	}
	o := f.execute(t)
	if o.Status != run.StatusNeedsInput || f.e.State.Progress.Approved[StageResearch] != 0 {
		t.Fatalf("unavailable source upgraded to essential was ignored: outcome=%+v approvals=%v", o, f.e.State.Progress.Approved)
	}
}

func TestP3ReviewBlockingQuestionCanRevisitAnAssumption(t *testing.T) {
	const question = "Which public version API must be preserved?"
	f := newFixture(t, []reply{fixed(research(r1))}, []reply{func(provider.Request) (any, error) {
		d := review("needs_input", researchOK, nil)
		d["questions"] = []any{map[string]any{"id": "Q-001", "question": question, "why": "The existing assumption changes the public API.", "impact": "compatibility", "options": []string{"keep", "replace"}, "proposed_assumption": "", "blocking": true}}
		return d, nil
	}})
	f.e.Cfg.ReviewRounds = 1
	f.e.State.Progress.NextQuestion = 1
	if err := saveDecisions(f.e.Run, []Decision{{ID: "Q-001", Question: question, Answer: "replace", Source: "assumption"}}); err != nil {
		t.Fatal(err)
	}
	o := f.execute(t)
	if o.Status != run.StatusNeedsInput || !hasBlocking(f.e.State.Progress.Pending) {
		t.Fatalf("material question suppressed by a provisional assumption: outcome=%+v pending=%v", o, f.e.State.Progress.Pending)
	}
}

func TestP3ReviewLedgerRestatementSurvivesAppend(t *testing.T) {
	for _, status := range []string{"open", "resolved"} {
		t.Run(status, func(t *testing.T) {
			p := &run.Progress{NextFinding: 1, Ledger: make([]run.Finding, 1, 1)}
			p.Ledger[0] = run.Finding{ID: "F-001", Stage: StageResearch, TargetID: "R-001", Severity: "minor", Status: status}
			rev := &schema.Review{Verdict: "revise", Findings: []schema.Finding{
				{Severity: "minor", TargetID: "R-001", Problem: "new issue", RequestedChange: "fix"},
				{ID: "F-001", Severity: "major", TargetID: "R-001", Problem: "old issue is still present", RequestedChange: "fix"},
			}}
			notes := applyReview(p, StageResearch, "research r2", rev, relevant(p, StageResearch))
			// Restating a closed ID may instead be rejected as a contract error.
			if status == "resolved" && len(notes) > 0 {
				return
			}
			if p.Ledger[0].Status != "open" || p.Ledger[0].Severity != "major" || p.Ledger[0].Problem != "old issue is still present" {
				t.Errorf("restated finding lost after slice growth: %+v", p.Ledger[0])
			}
			applyReview(p, StageResearch, "research r3", &schema.Review{Verdict: "approve", Dispositions: []schema.Disposition{{FindingID: "F-002", Status: "resolved", Reason: "fixed"}}}, relevant(p, StageResearch))
			if len(relevant(p, StageResearch)) == 0 {
				t.Error("ledger permits approval after resolving only F-002; reopened F-001 disappeared")
			}
		})
	}
}

func TestP3ReviewPhysicalAttemptCapAppliesInsideRunner(t *testing.T) {
	f := newFixture(t, nil, nil)
	dir := t.TempDir()
	bin, counter := filepath.Join(dir, "fake-claude"), filepath.Join(dir, "attempts")
	body := "#!/bin/sh\n/bin/cat >/dev/null\nprintf x >> '" + counter + "'\nexit 1\n"
	if err := os.WriteFile(bin, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	f.e.Planner = &provider.Claude{Bin: bin}
	f.e.State.Limits.MaxAttempts, f.e.State.Limits.Source = 1, "flag"
	o := f.execute(t)
	b, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 1 || f.e.State.Counters.Attempts > 1 {
		t.Fatalf("--max-calls=1 launched %d processes; attempts=%d outcome=%+v", len(b), f.e.State.Counters.Attempts, o)
	}
}

type p3ReviewRunnerFunc func(context.Context, provider.Request) (*provider.Result, error)

func (f p3ReviewRunnerFunc) Run(ctx context.Context, req provider.Request) (*provider.Result, error) {
	return f(ctx, req)
}

func TestP3ReviewActiveTimeBoundsTheCurrentCall(t *testing.T) {
	f := newFixture(t, nil, nil)
	f.e.State.Limits.MaxActiveSeconds = 1
	f.e.State.Counters.ActiveSeconds = 0.9
	f.e.Cfg.CallDeadline = time.Minute
	f.e.Planner = p3ReviewRunnerFunc(func(ctx context.Context, req provider.Request) (*provider.Result, error) {
		// The effective deadline can be supplied through the request or context.
		deadline := req.Deadline
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if time.Until(deadline) > 250*time.Millisecond {
			t.Errorf("100ms remaining active budget permits a call for %s", time.Until(deadline))
		}
		return nil, &provider.Error{Class: provider.ClassCanceled, Msg: "test stops here"}
	})
	f.execute(t)
}

func TestP3ReviewDuplicateQuestionEscalatesPendingAssumption(t *testing.T) {
	p := &run.Progress{Pending: []run.Pending{{ID: "Q-001", Question: "API choice?", ProposedAssumption: "replace"}}}
	addQuestions(p, StageResearch, "reviewer", []schema.Question{{Question: "API choice?", Blocking: true, Why: "material compatibility choice"}}, nil)
	if !hasBlocking(p.Pending) {
		t.Fatalf("blocking duplicate downgraded to a non-blocking assumption: %+v", p.Pending)
	}
}
