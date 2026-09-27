package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/killabayte/shogun/internal/library"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

// planDoc is a whole-plan planner document: the research of r plus the given steps.
func planDoc(r []req, steps ...step) map[string]any {
	d := research(r...)
	d["approach"] = map[string]any{"summary": "add the flag", "alternatives": []any{}}
	d["steps"] = stepBatch(steps...)["steps"]
	d["final_verification_criterion_ids"] = []string{}
	delete(d, "questions")
	d["questions"] = []any{}
	return d
}

func newFast(t *testing.T, planner, reviewer []reply) *fixture {
	f := newFixture(t, planner, reviewer)
	f.e.State.Mode = ""
	return f
}

// The default: one planner call, one review of the rendered candidate, publish.
func TestFastPathPublishesInTwoCalls(t *testing.T) {
	f := newFast(t, []reply{fixed(planDoc([]req{r1}, s1))}, []reply{fixed(finalReview("approve", nil, s1))})
	o := f.execute(t)
	if o.Status != run.StatusApproved {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
	if c := f.e.State.Counters.LogicalCalls; c != 2 {
		t.Fatalf("calls %d, want 2", c)
	}
	if got, note := library.Verify(f.e.State.Publish.Path); got != library.Valid {
		t.Fatalf("verify: %s %s", got, note)
	}
	if !strings.Contains(f.reviewer.prompts[0], "candidate.md") || !strings.Contains(f.planner.prompts[0], "Work economically") {
		t.Fatal("prompts do not carry the review target or the economy rules")
	}
}

// A major finding costs exactly one revision pair.
func TestFastPathOneRevision(t *testing.T) {
	approve := finalReview("approve", nil, s1)
	approve["dispositions"] = []any{disposition("F-001", "resolved")}
	revised := planDoc([]req{r1}, s1)
	revised["approach"] = map[string]any{"summary": "add the flag, print to stdout", "alternatives": []any{}}
	f := newFast(t, []reply{fixed(planDoc([]req{r1}, s1)), fixed(revised)},
		[]reply{fixed(finalReview("revise", []map[string]any{finding("S-001", "major", "no rollback")}, s1)), fixed(approve)})
	if o := f.execute(t); o.Status != run.StatusApproved || f.e.State.Counters.LogicalCalls != 4 {
		t.Fatalf("%+v calls %d\n%s", o, f.e.State.Counters.LogicalCalls, f.log.String())
	}
	if !strings.Contains(f.planner.prompts[1], "F-001") {
		t.Fatal("the revision did not see the finding")
	}
}

// Minor findings never cost a round; they are listed in the plan as review notes.
func TestFastPathMinorDoesNotBlock(t *testing.T) {
	f := newFast(t, []reply{fixed(planDoc([]req{r1}, s1))},
		[]reply{fixed(finalReview("approve", []map[string]any{finding("S-001", "minor", "wording could be tighter")}, s1))})
	if o := f.execute(t); o.Status != run.StatusApproved || f.e.State.Counters.LogicalCalls != 2 {
		t.Fatalf("%+v", o)
	}
	b, _ := os.ReadFile(f.e.State.Publish.Path)
	_ = b // the candidate was rendered before this review; the note is in the ledger
	if len(openMinors(&f.e.State.Progress)) != 1 {
		t.Fatal("the minor finding was lost")
	}
}

// Shogun's contract checks still apply and cost no review call.
func TestFastPathContractProblemSkipsReview(t *testing.T) {
	bad := planDoc([]req{r1}, step{"S-001", []string{"R-009.C1"}})
	f := newFast(t, []reply{fixed(bad), fixed(planDoc([]req{r1}, s1))}, []reply{fixed(finalReview("approve", nil, s1))})
	if o := f.execute(t); o.Status != run.StatusApproved || calls(f.reviewer) != 1 {
		t.Fatalf("%+v reviewer calls %d", o, calls(f.reviewer))
	}
	if !strings.Contains(f.planner.prompts[1], "unknown criterion R-009.C1") {
		t.Fatal("contract problem not handed back")
	}
}

// At most two reviews: a plan that still does not converge stops instead of spending more.
func TestFastPathRoundCap(t *testing.T) {
	rev := func(n string) reply {
		return fixed(finalReview("revise", []map[string]any{finding("S-001", "major", n)}, s1))
	}
	second := planDoc([]req{r1}, s1)
	second["approach"] = map[string]any{"summary": "second try", "alternatives": []any{}}
	f := newFast(t, []reply{fixed(planDoc([]req{r1}, s1)), fixed(second)}, []reply{rev("a"), rev("b")})
	o := f.execute(t)
	if o.Status != run.StatusPaused || !strings.Contains(o.Reason, "limit") || f.e.State.Counters.LogicalCalls != 4 {
		t.Fatalf("%+v calls %d", o, f.e.State.Counters.LogicalCalls)
	}
}

type usageRunner struct {
	usage string
	inner *script
}

func (u usageRunner) Run(ctx context.Context, req provider.Request) (*provider.Result, error) {
	res, err := u.inner.Run(ctx, req)
	if res != nil {
		res.Usage = json.RawMessage(u.usage)
		res.Usages = []json.RawMessage{res.Usage}
	}
	return res, err
}

// Every run accounts tokens and Claude's list-price cost, so its spend is visible.
func TestFastPathRecordsTokensAndCost(t *testing.T) {
	f := newFast(t, []reply{fixed(planDoc([]req{r1}, s1))}, []reply{fixed(finalReview("approve", nil, s1))})
	f.e.Planner = usageRunner{`{"claude-opus-5-5":{"inputTokens":1000,"cacheReadInputTokens":9000,"outputTokens":500,"thinkingTokens":100,"costUSD":0.25}}`, f.planner}
	f.e.Reviewer = usageRunner{`{"input_tokens":20000,"output_tokens":300,"reasoning_output_tokens":200}`, f.reviewer}
	if o := f.execute(t); o.Status != run.StatusApproved {
		t.Fatalf("%+v", o)
	}
	c := f.e.State.Counters
	// Cache reads and reasoning are kept apart, not folded into input/output.
	if c.InputTokens != 21000 || c.CacheReadTokens != 9000 || c.OutputTokens != 800 || c.ReasoningTokens != 300 || c.CostUSD != 0.25 || c.UsageIncomplete {
		t.Fatalf("counters %+v", c)
	}
}

// Missing telemetry is "incomplete", never a silent zero.
func TestFastPathMissingUsageIsIncomplete(t *testing.T) {
	f := newFast(t, []reply{fixed(planDoc([]req{r1}, s1))}, []reply{fixed(finalReview("approve", nil, s1))})
	if o := f.execute(t); o.Status != run.StatusApproved || !f.e.State.Counters.UsageIncomplete {
		t.Fatalf("%+v %+v", o, f.e.State.Counters)
	}
	if !strings.Contains(SpendLine(f.e.State.Counters, f.e.State.Limits), "INCOMPLETE") {
		t.Fatal("the spend line hides incomplete usage")
	}
}

// Minor notes of the approving review reach an artifact (the approved body predates that review).
func TestFastPathFinalMinorNotesArtifact(t *testing.T) {
	f := newFast(t, []reply{fixed(planDoc([]req{r1}, s1))},
		[]reply{fixed(finalReview("approve", []map[string]any{finding("S-001", "minor", "wording could be tighter")}, s1))})
	if o := f.execute(t); o.Status != run.StatusApproved {
		t.Fatalf("%+v", o)
	}
	b, err := os.ReadFile(f.e.Run.Dir + "/review-notes.md")
	if err != nil || !strings.Contains(string(b), "wording could be tighter") {
		t.Fatalf("review notes: %v %s", err, b)
	}
}

// The ten-minute budget keeps a third for the review: a planner call is not started without it.
func TestFastPathReservesTimeForReview(t *testing.T) {
	f := newFast(t, []reply{fixed(planDoc([]req{r1}, s1))}, nil)
	f.e.State.Limits.MaxActiveSeconds = 600
	f.e.State.Counters.ActiveSeconds = 360 // 240 s left, 200 s reserved for review → 40 s for the planner
	o := f.execute(t)
	if o.Status != run.StatusPaused || !strings.Contains(o.Reason, "not enough for another planner call") || calls(f.planner) != 0 {
		t.Fatalf("%+v planner calls %d", o, calls(f.planner))
	}
}

// Live 2026-09-27 (demo run): the planner put a criterion both in S-001 and in the end-to-end check;
// the reviewer cited only S-001's verifications. That is complete evidence and passes at round 1.
func TestFastPathFinalIsOptionalWhenAStepProvesTheCriterion(t *testing.T) {
	d := planDoc([]req{r1}, s1)
	d["final_verification_criterion_ids"] = []string{"R-001.C1"}
	f := newFast(t, []reply{fixed(d)}, []reply{fixed(finalReview("approve", nil, s1))})
	if o := f.execute(t); o.Status != run.StatusApproved || f.e.State.Counters.LogicalCalls != 2 {
		t.Fatalf("%+v calls %d\n%s", o, f.e.State.Counters.LogicalCalls, f.log.String())
	}
}

// Live 2026-09-27: the reviewer "resolved" Shogun's gate notes under invented ids F-001…F-004. A
// disposition for an id that is not open closes nothing and must not block an approval.
func TestFastPathIgnoresDispositionsForUnknownFindings(t *testing.T) {
	rev := finalReview("approve", nil, s1)
	rev["dispositions"] = []any{disposition("F-001", "resolved"), disposition("F-004", "resolved")}
	f := newFast(t, []reply{fixed(planDoc([]req{r1}, s1))}, []reply{fixed(rev)})
	if o := f.execute(t); o.Status != run.StatusApproved || f.e.State.Counters.LogicalCalls != 2 {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
}

// Every reviewer is told what makes a finding blocking: violated requirement, evidence, consequence.
func TestReviewerPromptsCarryTheMaterialityRule(t *testing.T) {
	for _, stage := range []string{"plan", "research", "outline", "detail", "integration"} {
		b, err := promptFiles.ReadFile("prompts/" + stage + ".reviewer.tmpl")
		if err != nil || !strings.Contains(string(b), `{{template "materiality" .}}`) {
			t.Errorf("%s reviewer prompt lacks the materiality rule: %v", stage, err)
		}
	}
	f := newFast(t, []reply{fixed(planDoc([]req{r1}, s1))}, []reply{fixed(finalReview("approve", nil, s1))})
	f.execute(t)
	if !strings.Contains(f.reviewer.prompts[0], "cannot name all three is minor at most") {
		t.Fatal("the rendered review prompt lacks the rule")
	}
}
