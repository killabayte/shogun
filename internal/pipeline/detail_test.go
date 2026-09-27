package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

// chain is an outline of n steps S-001 → S-002 → … each depending on the previous one and each
// carrying one criterion of R-001 (so every step has something to cover).
func chain(n int) (map[string]any, req, []step) {
	r := req{"R-001", "functional", true, nil}
	var steps []step
	var ss []any
	for i := 1; i <= n; i++ {
		id, c := fmt.Sprintf("S-%03d", i), fmt.Sprintf("R-001.C%d", i)
		r.crit = append(r.crit, c)
		deps := []string{}
		if i > 1 {
			deps = []string{fmt.Sprintf("S-%03d", i-1)}
		}
		steps = append(steps, step{id, []string{c}})
		ss = append(ss, map[string]any{"id": id, "title": "t", "objective": "o", "deliverable": "d", "depends_on": deps, "criterion_ids": []string{c}})
	}
	o := outline()
	o["steps"] = ss
	return o, r, steps
}

// withDeps sets depends_on of a detail document to the outline chain.
func withDeps(d map[string]any) map[string]any {
	for _, s := range d["steps"].([]any) {
		m := s.(map[string]any)
		var n int
		fmt.Sscanf(m["id"].(string), "S-%03d", &n)
		if n > 1 {
			m["depends_on"] = []string{fmt.Sprintf("S-%03d", n-1)}
		}
	}
	return d
}

func chainCoverage(r req) []row {
	return []row{{"R-001", "covered", r.crit, []string{"FACT-001"}}}
}

func outlineCoverage(r req, steps []step) []row {
	var ids []string
	for _, s := range steps {
		ids = append(ids, s.id)
	}
	return []row{{"R-001", "covered", r.crit, ids}}
}

// newChain builds a run whose research and outline are approved at r1, then scripts the detail
// exchanges given by the caller.
func newChain(t *testing.T, n int, planner, reviewer []reply) (*fixture, []step) {
	o, r, steps := chain(n)
	pl := append([]reply{fixed(research(r)), fixed(o)}, planner...)
	rv := append([]reply{fixed(review("approve", chainCoverage(r), nil)), fixed(review("approve", outlineCoverage(r, steps), nil))}, reviewer...)
	return newFixture(t, pl, rv), steps
}

func batch(s ...step) reply { return fixed(withDeps(stepBatch(s...))) }

// §13 P4: a fake Runner passes three steps with edits (revise, then approve each).
func TestThreeStepsWithEdits(t *testing.T) {
	_, _, steps := chain(3)
	var pl, rv []reply
	for _, s := range steps {
		pl = append(pl, batch(s), batch(s))
		rv = append(rv, fixed(stepReview("revise", []map[string]any{finding(s.id, "major", "verification is vague")}, s)),
			fixed(stepReview("approve", nil, s)))
	}
	// dispositions: the second review of each step resolves its finding F-00(i+1)
	for i := range steps {
		d := stepReview("approve", nil, steps[i])
		d["dispositions"] = []any{disposition(fmt.Sprintf("F-%03d", i+1), "resolved")}
		rv[2*i+1] = fixed(d)
		// make the revised document differ from the first one
		doc := withDeps(stepBatch(steps[i]))
		doc["steps"].([]any)[0].(map[string]any)["actions"] = []string{"edit main.go", "run go test ./..."}
		pl[2*i+1] = fixed(doc)
	}
	f, _ := newChain(t, 3, pl, rv)
	o := f.execute(t)
	if o.Status != run.StatusPaused || f.e.State.Cursor.Stage != StageIntegration {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
	for _, s := range steps {
		if f.e.State.Progress.Accepted[s.id] != 1 {
			t.Errorf("%s accepted as %d", s.id, f.e.State.Progress.Accepted[s.id])
		}
		if _, err := os.Stat(filepath.Join(f.e.Run.Dir, "steps", s.id, "1.json")); err != nil {
			t.Error(err)
		}
	}
	// the later step's planner saw the accepted dependency
	if !strings.Contains(f.planner.prompts[4], "steps/S-001/1.json") {
		t.Errorf("S-002 prompt does not point at accepted S-001:\n%s", f.planner.prompts[4])
	}
}

// §13 P4: an accepted step is not rewritten without a revision.
func TestAcceptedStepIsNotRewritten(t *testing.T) {
	_, _, steps := chain(2)
	f, _ := newChain(t, 2,
		[]reply{batch(steps[0]), batch(steps[0], steps[1]), batch(steps[1])},
		[]reply{fixed(stepReview("approve", nil, steps[0])), fixed(stepReview("approve", nil, steps[1]))})
	o := f.execute(t)
	if o.Status != run.StatusPaused || f.e.State.Cursor.Stage != StageIntegration {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
	if !strings.Contains(f.planner.prompts[4], "S-001 is not in this batch") {
		t.Fatalf("rewriting accepted S-001 was not refused:\n%s", f.planner.prompts[4])
	}
	if entries, _ := os.ReadDir(filepath.Join(f.e.Run.Dir, "steps", "S-001")); len(entries) != 1 {
		t.Fatalf("S-001 archived %d times", len(entries))
	}
}

// §13 P4: editing an early step invalidates the suffix; the new acceptance is a new revision file.
func TestEditingEarlyStepInvalidatesSuffix(t *testing.T) {
	_, _, steps := chain(3)
	f, _ := newChain(t, 3,
		[]reply{batch(steps[0]), batch(steps[1]), batch(steps[2]), batch(steps[0]), batch(steps[1]), batch(steps[2])},
		[]reply{
			fixed(stepReview("approve", nil, steps[0])), fixed(stepReview("approve", nil, steps[1])),
			fixed(stepReview("revise", []map[string]any{finding("S-001", "blocker", "S-001 creates the wrong file")}, steps[2])),
			func(provider.Request) (any, error) {
				d := stepReview("approve", nil, steps[0])
				d["dispositions"] = []any{disposition("F-001", "resolved")}
				return d, nil
			},
			fixed(stepReview("approve", nil, steps[1])), fixed(stepReview("approve", nil, steps[2])),
		})
	o := f.execute(t)
	if o.Status != run.StatusPaused || f.e.State.Cursor.Stage != StageIntegration {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
	acc := f.e.State.Progress.Accepted
	if acc["S-001"] != 2 || acc["S-002"] != 2 || acc["S-003"] != 1 {
		t.Fatalf("accepted revisions %v (S-001 and S-002 re-accepted as new files, S-003 first accepted after the redo)", acc)
	}
	if !strings.Contains(f.log.String(), "S-001 must change") {
		t.Fatalf("no invalidation logged:\n%s", f.log.String())
	}
}

// A planner request to change an accepted step reopens it and the suffix too.
func TestPlannerRequestedChangeReopensStep(t *testing.T) {
	_, _, steps := chain(2)
	req := withDeps(stepBatch(steps[1]))
	req["requested_changes"] = []any{map[string]any{"target_id": "S-001", "reason": "S-001 must also create the test file"}}
	f, _ := newChain(t, 2,
		[]reply{batch(steps[0]), fixed(req), batch(steps[0]), batch(steps[1])},
		[]reply{fixed(stepReview("approve", nil, steps[0])), fixed(stepReview("approve", nil, steps[0])), fixed(stepReview("approve", nil, steps[1]))})
	if o := f.execute(t); o.Status != run.StatusPaused || f.e.State.Progress.Accepted["S-001"] != 2 {
		t.Fatalf("%+v %v\n%s", o, f.e.State.Progress.Accepted, f.log.String())
	}
	if !strings.Contains(f.planner.prompts[4], "S-001 must also create the test file") {
		t.Fatalf("the reason did not reach the S-001 redo:\n%s", f.planner.prompts[4])
	}
}

// §13 P4: dependency changes and cycles are refused mechanically.
func TestDetailDependencyCycleRejected(t *testing.T) {
	_, _, steps := chain(2)
	cyc := withDeps(stepBatch(steps[0]))
	cyc["steps"].([]any)[0].(map[string]any)["depends_on"] = []string{"S-002"}
	f, _ := newChain(t, 2, []reply{fixed(cyc), batch(steps[0]), batch(steps[1])},
		[]reply{fixed(stepReview("approve", nil, steps[0])), fixed(stepReview("approve", nil, steps[1]))})
	if o := f.execute(t); o.Status != run.StatusPaused || f.e.State.Cursor.Stage != StageIntegration {
		t.Fatalf("%+v", o)
	}
	for _, want := range []string{"dependency cycle", "adds dependency S-002"} {
		if !strings.Contains(f.planner.prompts[3], want) {
			t.Errorf("missing %q in:\n%s", want, f.planner.prompts[3])
		}
	}
}

// §13 P4: a batch with a skipped step does not pass, neither in the document nor in the coverage.
func TestBatchWithSkippedStepDoesNotPass(t *testing.T) {
	_, _, steps := chain(2)
	partial := stepReview("approve", nil, steps[0], steps[1])
	partial["coverage"].([]any)[0].(map[string]any)["target_ids"] = []string{"S-001"}
	partial["coverage"].([]any)[0].(map[string]any)["verification_refs"] = []string{"S-001/V-001"}
	f, _ := newChain(t, 2, []reply{batch(steps[0]), batch(steps[0], steps[1]), batch(steps[0], steps[1])},
		[]reply{fixed(partial), fixed(stepReview("approve", nil, steps[0], steps[1]))})
	f.e.Cfg.DetailBatch = 2
	o := f.execute(t)
	if o.Status != run.StatusPaused || f.e.State.Cursor.Stage != StageIntegration {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
	if !strings.Contains(f.planner.prompts[3], "the batch is missing step S-002") {
		t.Errorf("skipped step in the document not refused:\n%s", f.planner.prompts[3])
	}
	if !strings.Contains(f.planner.prompts[4], "step S-002 does not appear in any coverage row") {
		t.Errorf("skipped step in the coverage not refused:\n%s", f.planner.prompts[4])
	}
}

// §13 P4: context overflow pauses (after shrinking a larger batch), never approves.
func TestContextOverflowPauses(t *testing.T) {
	f, _ := newChain(t, 2, nil, nil)
	f.e.Cfg.DetailBatch = 2
	// Research and outline run under the normal threshold; when the outline review arrives the
	// threshold drops so that every detail call is far above it.
	approveOutline := f.reviewer.replies[1]
	f.reviewer.replies[1] = func(r provider.Request) (any, error) {
		f.e.Cfg.MaxContextTokens = 10
		return approveOutline(r)
	}
	o := f.execute(t)
	if o.Status != run.StatusPaused || !strings.Contains(o.Reason, "context_limit") {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
	if f.e.State.Progress.BatchSize != 1 || !strings.Contains(f.log.String(), "batch shrinks to 1") {
		t.Fatalf("batch did not shrink before pausing: %d\n%s", f.e.State.Progress.BatchSize, f.log.String())
	}
	if len(f.e.State.Progress.Accepted) != 0 || calls(f.planner) != 2 {
		t.Fatalf("a detail call was made or a step accepted over the context limit (planner calls %d)", calls(f.planner))
	}
}

// §13 P4 budgets: 2R(B+3) fixed at the first approved outline, never replenished.
func TestDerivedBudgets(t *testing.T) {
	f := newFixture(t, nil, nil)
	if f.e.State.Limits.MaxLogicalCalls != 24 { // preliminary 4R, R=6
		t.Fatalf("preliminary reserve %d", f.e.State.Limits.MaxLogicalCalls)
	}
	for n, want := range map[int][2]int{10: {156, 468}, 30: {396, 1188}} {
		g := newFixture(t, nil, nil)
		g.e.deriveBudget(n)
		if l := g.e.State.Limits; l.MaxLogicalCalls != want[0] || l.MaxAttempts != want[1] {
			t.Errorf("N=%d: %d/%d, want %d/%d", n, l.MaxLogicalCalls, l.MaxAttempts, want[0], want[1])
		}
		g.e.deriveBudget(3 * n) // a repeated outline with more steps does not replenish
		if l := g.e.State.Limits; l.MaxLogicalCalls != want[0] {
			t.Errorf("N=%d: re-derived to %d", n, l.MaxLogicalCalls)
		}
	}
	h := newFixture(t, nil, nil)
	h.e.State.Limits.MaxAttempts, h.e.State.Limits.Source = 7, "flag"
	h.e.deriveBudget(10)
	if h.e.State.Limits.MaxAttempts != 7 {
		t.Error("an explicit --max-calls must take precedence over 3C")
	}
}

// §13 P4: six review rounds on every step fit in the derived budget without retries; an explicit
// user cap may stop earlier.
func TestSixRoundsPerStepFitTheBudget(t *testing.T) {
	const n = 2
	_, _, steps := chain(n)
	var pl, rv []reply
	fid := 0
	for _, s := range steps {
		for round := 1; round <= 6; round++ {
			doc := withDeps(stepBatch(s))
			doc["steps"].([]any)[0].(map[string]any)["actions"] = []string{fmt.Sprintf("edit main.go (round %d)", round)}
			pl = append(pl, fixed(doc))
			var d map[string]any
			if round < 6 {
				d = stepReview("revise", []map[string]any{finding(s.id, "minor", fmt.Sprintf("round %d detail", round))}, s)
			} else {
				d = stepReview("approve", nil, s)
			}
			if round > 1 {
				d["dispositions"] = []any{disposition(fmt.Sprintf("F-%03d", fid), "resolved")}
			}
			if round < 6 {
				fid++
			}
			rv = append(rv, fixed(d))
		}
	}
	f, _ := newChain(t, n, pl, rv)
	o := f.execute(t)
	if o.Status != run.StatusPaused || f.e.State.Cursor.Stage != StageIntegration {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
	// +1: the unscripted final review that stops the test is counted as a call too.
	if c, l := f.e.State.Counters.LogicalCalls, f.e.State.Limits.MaxLogicalCalls; c != 4+2*6*n+1 || c > l {
		t.Fatalf("calls %d within limit %d", c, l)
	}
	g, _ := newChain(t, n, pl, rv)
	g.e.State.Limits.MaxAttempts, g.e.State.Limits.Source = 10, "flag"
	if o := g.execute(t); o.Status != run.StatusPaused || !strings.Contains(o.Reason, "limit") {
		t.Fatalf("explicit cap: %+v", o)
	}
}

// Resume continues a detail run from its cursor with generation and counters intact.
func TestResumeKeepsDetailProgressAndCounters(t *testing.T) {
	_, _, steps := chain(2)
	f, _ := newChain(t, 2, []reply{batch(steps[0]), func(provider.Request) (any, error) {
		return nil, &provider.Error{Class: provider.ClassRateLimit, Msg: "five-hour window", Attempts: 1}
	}}, []reply{fixed(stepReview("approve", nil, steps[0]))})
	if o := f.execute(t); o.Status != run.StatusPaused || !strings.Contains(o.Reason, "rate_limit") {
		t.Fatalf("%+v", o)
	}
	st, err := f.e.Run.LoadState()
	if err != nil || st.Cursor.Step != "S-002" || st.Progress.Accepted["S-001"] != 1 || st.Counters.LogicalCalls != 7 || st.Generation != 1 {
		t.Fatalf("checkpoint %+v %v", st, err)
	}
	g := newFixture(t, []reply{batch(steps[1])}, []reply{fixed(stepReview("approve", nil, steps[1]))})
	g.e.Run, g.e.State = f.e.Run, st
	if o := g.execute(t); o.Status != run.StatusPaused || g.e.State.Cursor.Stage != StageIntegration || g.e.State.Counters.LogicalCalls != 10 {
		t.Fatalf("resume: %+v %+v", o, g.e.State.Counters)
	}
}

// §5: a detail review can send the work back to the outline; the re-approved outline resets every
// detail approval and does not replenish the budget.
func TestDetailReviewReturnsToOutline(t *testing.T) {
	o, r, steps := chain(2)
	f, _ := newChain(t, 2,
		[]reply{batch(steps[0]), batch(steps[1]), fixed(o), batch(steps[0]), batch(steps[1])},
		[]reply{
			fixed(stepReview("approve", nil, steps[0])),
			fixed(stepReview("revise", []map[string]any{finding("outline", "blocker", "S-002 needs S-001's output that the outline never produces")}, steps[1])),
			func(provider.Request) (any, error) {
				d := review("approve", outlineCoverage(r, steps), nil)
				d["dispositions"] = []any{disposition("F-001", "resolved")}
				return d, nil
			},
			fixed(stepReview("approve", nil, steps[0])), fixed(stepReview("approve", nil, steps[1])),
		})
	out := f.execute(t)
	p := f.e.State.Progress
	if out.Status != run.StatusPaused || f.e.State.Cursor.Stage != StageIntegration || p.Approved[StageOutline] != 2 {
		t.Fatalf("%+v approved %v\n%s", out, p.Approved, f.log.String())
	}
	if p.Accepted["S-001"] != 2 || p.Accepted["S-002"] != 1 {
		t.Fatalf("detail approvals were not reset by the new outline: %v", p.Accepted)
	}
	if l := f.e.State.Limits.MaxLogicalCalls; l != 2*6*(2+3) {
		t.Fatalf("budget %d was re-derived by the second outline", l)
	}
	// Found live (PORTALS-3426): rounds of a step spent under an earlier skeleton must not count
	// against the same step of the new one.
	if r := p.Rounds["detail:S-001"]; r != 1 {
		t.Fatalf("detail:S-001 rounds after the new outline = %d, want 1", r)
	}
}
