package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

// ---- scripted runners ----

type reply func(req provider.Request) (any, error)

type script struct {
	t       *testing.T
	name    string
	replies []reply
	prompts []string
}

func (s *script) Run(ctx context.Context, req provider.Request) (*provider.Result, error) {
	s.prompts = append(s.prompts, req.Prompt)
	if len(s.prompts) > len(s.replies) {
		s.t.Fatalf("%s: unexpected call %d:\n%s", s.name, len(s.prompts), req.Prompt)
	}
	v, err := s.replies[len(s.prompts)-1](req)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(v)
	if err := provider.ValidatePayload(req.Schema, b); err != nil {
		s.t.Fatalf("%s: test document does not satisfy the stage schema: %v\n%s", s.name, err, b)
	}
	return &provider.Result{Payload: b, Attempts: 1}, nil
}

func fixed(v any) reply { return func(provider.Request) (any, error) { return v, nil } }

func calls(s *script) int { return len(s.prompts) }

// ---- documents ----

type req struct {
	id, typ   string
	mandatory bool
	crit      []string
}

func research(reqs ...req) map[string]any {
	var rs []any
	for _, r := range reqs {
		var cs []any
		for _, c := range r.crit {
			cs = append(cs, map[string]any{"id": c, "text": "criterion " + c})
		}
		rs = append(rs, map[string]any{"id": r.id, "statement": "statement " + r.id, "source_ids": []string{"task"},
			"type": r.typ, "mandatory": r.mandatory, "criteria": cs})
	}
	return map[string]any{"schema_version": 1,
		"facts":           []any{map[string]any{"id": "FACT-001", "source_id": "repo-1", "location": "main.go:1", "quote": "package main", "kind": "observation", "text": "a Go program"}},
		"requirements":    rs,
		"source_coverage": []any{map[string]any{"source_id": "repo-1", "studied": "main.go", "relevance": "relevant", "notes": ""}},
		"questions":       []any{}, "web_sources": []any{}, "requested_changes": []any{}, "responses_to_findings": []any{}}
}

var r1 = req{"R-001", "functional", true, []string{"R-001.C1"}}
var r2 = req{"R-002", "constraint", true, []string{"R-002.C1"}}

type step struct {
	id   string
	crit []string
}

func outline(steps ...step) map[string]any {
	var ss []any
	for _, s := range steps {
		ss = append(ss, map[string]any{"id": s.id, "title": "t", "objective": "o", "deliverable": "d", "depends_on": []string{}, "criterion_ids": s.crit})
	}
	return map[string]any{"schema_version": 1, "approach": map[string]any{"summary": "s", "alternatives": []any{}},
		"steps": ss, "final_verification_criterion_ids": []string{}, "questions": []any{}, "requested_changes": []any{}, "responses_to_findings": []any{}}
}

type row struct {
	id, status string
	crit       []string
	targets    []string
}

func review(verdict string, rows []row, findings []map[string]any, dispositions ...map[string]any) map[string]any {
	var cov []any
	for _, r := range rows {
		cov = append(cov, map[string]any{"requirement_id": r.id, "criterion_ids": r.crit, "target_ids": r.targets, "verification_refs": []string{}, "status": r.status})
	}
	if findings == nil {
		findings = []map[string]any{}
	}
	if dispositions == nil {
		dispositions = []map[string]any{}
	}
	return map[string]any{"schema_version": 1, "verdict": verdict, "summary": "s", "findings": findings,
		"dispositions": dispositions, "coverage": nonNil(cov), "source_assessments": []any{}, "questions": []any{}}
}

func nonNil(a []any) []any {
	if a == nil {
		return []any{}
	}
	return a
}

func finding(target, severity, problem string) map[string]any {
	return map[string]any{"id": nil, "severity": severity, "target_id": target, "problem": problem, "evidence": []string{}, "requested_change": "fix " + problem}
}

func disposition(id, status string) map[string]any {
	return map[string]any{"finding_id": id, "status": status, "reason": "r", "evidence": []string{}}
}

var researchOK = []row{{"R-001", "covered", []string{"R-001.C1"}, []string{"FACT-001"}}}
var outlineOK = []row{{"R-001", "covered", []string{"R-001.C1"}, []string{"S-001"}}}

// ---- engine setup ----

type fixture struct {
	e                 *Engine
	planner, reviewer *script
	log               strings.Builder
}

func newFixture(t *testing.T, planner, reviewer []reply) *fixture {
	t.Helper()
	ws := t.TempDir()
	r, err := run.Create(run.RunsRoot(ws), "20260926-000000-test-abcd")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.Dir, "task.md"), []byte("Add a --version flag"), 0o600)
	cfg := config.Default()
	cfg.ReviewRounds, cfg.CallDeadline = 6, time.Minute
	st := run.NewState("20260926-000000-test-abcd", time.Now())
	st.Limits = run.Limits{MaxLogicalCalls: 4 * cfg.ReviewRounds, MaxAttempts: 12 * cfg.ReviewRounds, Source: "pre-outline"}
	f := &fixture{planner: &script{t: t, name: "planner", replies: planner}, reviewer: &script{t: t, name: "reviewer", replies: reviewer}}
	f.e = &Engine{Run: r, State: st, Task: "Add a --version flag", Cfg: cfg, Planner: f.planner, Reviewer: f.reviewer,
		Manifest: &inputs.Manifest{Repos: []inputs.Repo{{ID: "repo-1", Root: ws}}}, Log: &f.log, Now: time.Now,
		Fetch: func(ctx context.Context, dir string, urls []string) ([]inputs.Source, error) { return nil, nil }}
	return f
}

func (f *fixture) execute(t *testing.T) Outcome {
	t.Helper()
	return f.e.Execute(context.Background())
}

// ---- tests ----

func TestHappyPathStopsAfterApprovedOutline(t *testing.T) {
	f := newFixture(t,
		[]reply{fixed(research(r1)), fixed(outline(step{"S-001", []string{"R-001.C1"}}))},
		[]reply{fixed(review("approve", researchOK, nil)), fixed(review("approve", outlineOK, nil))})
	o := f.execute(t)
	if o.Status != run.StatusPaused || !strings.Contains(o.Reason, "P4") {
		t.Fatalf("outcome %+v\n%s", o, f.log.String())
	}
	st := f.e.State
	if st.Cursor.Stage != StageDetail || st.Progress.Approved[StageResearch] != 1 || st.Progress.Approved[StageOutline] != 1 {
		t.Fatalf("state %+v", st.Progress)
	}
	// C = 2R(B+3) for N=1, k=1, R=6 → 48 logical calls, 144 attempts.
	if st.Limits.MaxLogicalCalls != 48 || st.Limits.MaxAttempts != 144 || st.Limits.Source != "derived" {
		t.Fatalf("limits %+v", st.Limits)
	}
	if st.Counters.LogicalCalls != 4 || st.Counters.Attempts != 4 || st.Counters.ReviewRounds != 2 {
		t.Fatalf("counters %+v", st.Counters)
	}
	for _, p := range append(f.planner.prompts, f.reviewer.prompts...) {
		if !strings.Contains(p, "Minimal plan rule") || !strings.Contains(p, "Add a --version flag") {
			t.Fatalf("prompt lacks the minimal rule or the task:\n%s", p)
		}
	}
	for _, a := range []string{"research/1.json", "outline/1.json", "reviews/research-1.json", "reviews/outline-1.json", "calls/0001-research-planner/prompt.md", "calls/0004-outline-reviewer/result.json"} {
		if _, err := os.Stat(filepath.Join(f.e.Run.Dir, a)); err != nil {
			t.Errorf("artifact %s: %v", a, err)
		}
	}
	loaded, err := f.e.Run.LoadState()
	if err != nil || loaded.Cursor.Stage != StageDetail {
		t.Fatalf("checkpoint: %v %+v", err, loaded)
	}
}

// §13 P3: an outline step without a valid criterion never reaches the reviewer. An empty
// criterion_ids list is refused by the schema already in the adapter (minItems, payload class →
// format correction); a step citing a criterion that is not in the registry is refused here by
// CheckOutline and handed back to the planner.
func TestOutlineStepWithoutCriterionNeverReachesReviewer(t *testing.T) {
	bad := outline(step{"S-001", []string{"R-001.C1"}}, step{"S-002", []string{"R-009.C1"}})
	f := newFixture(t,
		[]reply{fixed(research(r1)), fixed(bad), fixed(outline(step{"S-001", []string{"R-001.C1"}}))},
		[]reply{fixed(review("approve", researchOK, nil)), fixed(review("approve", outlineOK, nil))})
	o := f.execute(t)
	if o.Status != run.StatusPaused || f.e.State.Cursor.Stage != StageDetail {
		t.Fatalf("outcome %+v\n%s", o, f.log.String())
	}
	if calls(f.reviewer) != 2 || calls(f.planner) != 3 {
		t.Fatalf("reviewer calls %d (want 2: research + fixed outline), planner %d", calls(f.reviewer), calls(f.planner))
	}
	if !strings.Contains(f.planner.prompts[2], "unknown criterion R-009.C1") {
		t.Fatalf("the contract problem was not handed back to the planner:\n%s", f.planner.prompts[2])
	}
}

// §13 P3: a requirement missing from the registry sends the work back to research.
func TestMissingRequirementReturnsToResearch(t *testing.T) {
	f := newFixture(t,
		[]reply{fixed(research(r1)), fixed(outline(step{"S-001", []string{"R-001.C1"}})),
			fixed(research(r1, r2)), fixed(outline(step{"S-001", []string{"R-001.C1"}}, step{"S-002", []string{"R-002.C1"}}))},
		[]reply{
			fixed(review("approve", researchOK, nil)),
			fixed(review("revise", outlineOK, []map[string]any{finding("research", "blocker", "the task also requires keeping the API (R-002 missing)")})),
			fixed(review("approve", []row{{"R-001", "covered", []string{"R-001.C1"}, []string{"FACT-001"}}, {"R-002", "covered", []string{"R-002.C1"}, []string{"FACT-001"}}}, nil, disposition("F-001", "resolved"))),
			fixed(review("approve", []row{{"R-001", "covered", []string{"R-001.C1"}, []string{"S-001"}}, {"R-002", "covered", []string{"R-002.C1"}, []string{"S-002"}}}, nil)),
		})
	o := f.execute(t)
	if o.Status != run.StatusPaused || f.e.State.Cursor.Stage != StageDetail {
		t.Fatalf("outcome %+v\n%s", o, f.log.String())
	}
	if f.e.State.Progress.Approved[StageResearch] != 2 || !strings.Contains(f.planner.prompts[2], "F-001") {
		t.Fatalf("research redo did not see the finding: %+v", f.e.State.Progress)
	}
}

// A research revision may not drop or weaken an accepted requirement.
func TestAcceptedRequirementCannotDisappear(t *testing.T) {
	f := newFixture(t, nil, nil)
	f.e.State.Progress = run.Progress{Approved: map[string]int{StageResearch: 1}}
	b, _ := json.Marshal(research(r1, r2))
	f.e.Run.WriteArtifact("research/1.json", b)
	weaker := r2
	weaker.mandatory = false
	for name, doc := range map[string]map[string]any{"removed": research(r1), "optional": research(r1, weaker)} {
		b, _ := json.Marshal(doc)
		r, _ := schema.Parse[schema.Research](schema.KindResearch, b)
		p := strings.Join(f.e.checkResearch(r), "\n")
		if !strings.Contains(p, "R-002") {
			t.Errorf("%s: %q", name, p)
		}
	}
}

// §13 P3: a finding that silently disappears from the next review is not closed.
func TestDisappearedFindingBlocksApproval(t *testing.T) {
	f := newFixture(t,
		[]reply{fixed(research(r1)), fixed(research(r1)), fixed(research(r1)), fixed(outline(step{"S-001", []string{"R-001.C1"}}))},
		[]reply{
			fixed(review("revise", researchOK, []map[string]any{finding("R-001", "major", "criterion is not testable")})),
			fixed(review("approve", researchOK, nil)), // F-001 vanished without a disposition
			fixed(review("approve", researchOK, nil, disposition("F-001", "resolved"))),
			fixed(review("approve", outlineOK, nil)),
		})
	// Make every planner revision differ so the stalemate rule does not fire.
	for i := range f.planner.replies[:3] {
		n := i
		f.planner.replies[i] = func(provider.Request) (any, error) {
			d := research(r1)
			d["facts"].([]any)[0].(map[string]any)["text"] = strings.Repeat("x", n+1)
			return d, nil
		}
	}
	o := f.execute(t)
	if o.Status != run.StatusPaused || f.e.State.Progress.Approved[StageResearch] != 3 {
		t.Fatalf("outcome %+v approved %v\n%s", o, f.e.State.Progress.Approved, f.log.String())
	}
	if !strings.Contains(f.planner.prompts[2], "F-001 has no disposition") {
		t.Fatalf("gate note missing:\n%s", f.planner.prompts[2])
	}
}

// §13 P3: partial or empty coverage never passes, even with verdict approve.
func TestPartialOrEmptyCoverageBlocks(t *testing.T) {
	for name, rows := range map[string][]row{
		"partial":        {{"R-001", "partial", []string{"R-001.C1"}, []string{"FACT-001"}}},
		"empty":          nil,
		"unknown target": {{"R-001", "covered", []string{"R-001.C1"}, []string{"FACT-999"}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, []reply{fixed(research(r1))}, []reply{fixed(review("approve", rows, nil))})
			f.e.Cfg.ReviewRounds = 1
			o := f.execute(t)
			if f.e.State.Progress.Approved[StageResearch] != 0 || !strings.Contains(o.Reason, "limit") {
				t.Fatalf("approved with %s coverage: %+v", name, o)
			}
			if len(f.e.State.Progress.GateNotes) == 0 {
				t.Fatal("no gate note")
			}
		})
	}
}

// §13 P3: --auto stops on a blocking question; resume applies the answers and the next planner
// revision sees them.
func TestBlockingQuestionNeedsInputThenResume(t *testing.T) {
	q := research(r1)
	q["questions"] = []any{map[string]any{"id": "Q-001", "question": "Semver or date?", "why": "format", "impact": "output", "options": []string{"semver", "date"}, "proposed_assumption": "", "blocking": true}}
	f := newFixture(t,
		[]reply{fixed(q), fixed(research(r1)), fixed(outline(step{"S-001", []string{"R-001.C1"}}))},
		[]reply{fixed(review("approve", researchOK, nil)), fixed(review("approve", outlineOK, nil))})
	o := f.execute(t)
	if o.Status != run.StatusNeedsInput || calls(f.reviewer) != 0 {
		t.Fatalf("outcome %+v, reviewer calls %d", o, calls(f.reviewer))
	}
	b, err := os.ReadFile(filepath.Join(f.e.Run.Dir, "questions.json"))
	if err != nil || !strings.Contains(string(b), "Semver or date?") {
		t.Fatalf("questions.json: %v %s", err, b)
	}
	if err := ApplyAnswers(f.e.Run, f.e.State, &schema.Answers{Answers: []schema.Answer{{QuestionID: "Q-002", Answer: "x"}}}, time.Now()); err == nil {
		t.Fatal("an answer for an unknown question was accepted")
	}
	if err := ApplyAnswers(f.e.Run, f.e.State, &schema.Answers{Answers: []schema.Answer{}}, time.Now()); err == nil {
		t.Fatal("a blocking question without an answer was accepted")
	}
	if err := ApplyAnswers(f.e.Run, f.e.State, &schema.Answers{Answers: []schema.Answer{{QuestionID: "Q-001", Answer: "semver"}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	o = f.execute(t)
	if o.Status != run.StatusPaused || f.e.State.Cursor.Stage != StageDetail {
		t.Fatalf("after resume: %+v\n%s", o, f.log.String())
	}
	if !strings.Contains(f.planner.prompts[1], "Semver or date? → semver") {
		t.Fatalf("the answer did not reach the planner:\n%s", f.planner.prompts[1])
	}
}

// Non-blocking questions in --auto proceed on the stated assumption, which is recorded.
func TestAutoTakesAssumptionForNonBlockingQuestion(t *testing.T) {
	q := research(r1)
	q["questions"] = []any{map[string]any{"id": "Q-001", "question": "Flag name?", "why": "w", "impact": "i", "options": []string{}, "proposed_assumption": "--version", "blocking": false}}
	f := newFixture(t, []reply{fixed(q), fixed(outline(step{"S-001", []string{"R-001.C1"}}))},
		[]reply{fixed(review("approve", researchOK, nil)), fixed(review("approve", outlineOK, nil))})
	if o := f.execute(t); o.Status != run.StatusPaused {
		t.Fatalf("%+v", o)
	}
	ds, _ := loadDecisions(f.e.Run.Dir)
	if len(ds) != 1 || ds[0].Source != "assumption" || ds[0].Answer != "--version" || !strings.Contains(f.reviewer.prompts[0], "Flag name? → --version") {
		t.Fatalf("decisions %+v", ds)
	}
}

type answerAll struct{ asked [][]run.Pending }

func (a *answerAll) Ask(ctx context.Context, qs []run.Pending) (map[string]string, error) {
	a.asked = append(a.asked, qs)
	out := map[string]string{}
	for _, q := range qs {
		out[q.ID] = "yes"
	}
	return out, nil
}

// Interactive: questions are asked in the terminal; after two rounds only needs_input remains.
func TestInteractiveQuestionsAtMostTwoRounds(t *testing.T) {
	ask := func(n int) map[string]any {
		d := research(r1)
		d["questions"] = []any{map[string]any{"id": "Q-001", "question": fmt.Sprintf("New question %d?", n), "why": "w", "impact": "i", "options": []string{}, "proposed_assumption": "", "blocking": true}}
		return d
	}
	f := newFixture(t, []reply{fixed(ask(1)), fixed(ask(2)), fixed(ask(3))}, nil)
	a := &answerAll{}
	f.e.Asker = a
	o := f.execute(t)
	if o.Status != run.StatusNeedsInput || len(a.asked) != 2 {
		t.Fatalf("outcome %+v after %d rounds", o, len(a.asked))
	}
}

// An unavailable essential web source stops research with needs_input.
func TestEssentialWebSourceUnavailable(t *testing.T) {
	d := research(r1)
	d["web_sources"] = []any{map[string]any{"url": "https://example.org/spec", "role": "essential", "related_ids": []string{"R-001"}}}
	f := newFixture(t, []reply{fixed(d)}, nil)
	f.e.Fetch = func(ctx context.Context, dir string, urls []string) ([]inputs.Source, error) {
		return []inputs.Source{{ID: "web-1", Origin: urls[0], Status: "error", Error: "HTTP 403"}}, inputs.ErrUnavailable
	}
	o := f.execute(t)
	if o.Status != run.StatusNeedsInput || !strings.Contains(o.Reason, "example.org") || calls(f.reviewer) != 0 {
		t.Fatalf("%+v", o)
	}
}

// A reviewer failure never approves: the run fails (protocol) or pauses (rate limit).
func TestReviewerErrorNeverApproves(t *testing.T) {
	for class, want := range map[provider.Class]run.Status{provider.ClassProtocol: run.StatusFailed, provider.ClassRateLimit: run.StatusPaused} {
		f := newFixture(t, []reply{fixed(research(r1))},
			[]reply{func(provider.Request) (any, error) {
				return nil, &provider.Error{Class: class, Msg: "boom", Attempts: 1}
			}})
		o := f.execute(t)
		if o.Status != want || f.e.State.Progress.Approved[StageResearch] != 0 {
			t.Fatalf("%s: %+v", class, o)
		}
	}
}

// The same document and the same open findings twice in a row is a stalemate: a question to the
// user (needs_input in --auto), not an automatic win for either side.
func TestStalemateBecomesQuestion(t *testing.T) {
	rev := review("revise", researchOK, []map[string]any{finding("R-001", "major", "too broad")})
	again := review("revise", researchOK, nil, disposition("F-001", "retained"))
	f := newFixture(t, []reply{fixed(research(r1)), fixed(research(r1))}, []reply{fixed(rev), fixed(again)})
	o := f.execute(t)
	if o.Status != run.StatusNeedsInput || !strings.Contains(string(mustRead(filepath.Join(f.e.Run.Dir, "questions.json"))), "do not converge on F-001") {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
}

// Budgets: the pre-outline reserve stops the run before an extra call.
func TestPreOutlineBudgetStops(t *testing.T) {
	f := newFixture(t, []reply{fixed(research(r1))}, []reply{fixed(review("revise", researchOK, []map[string]any{finding("R-001", "minor", "x")}))})
	f.e.State.Limits.MaxLogicalCalls = 2
	o := f.execute(t)
	if o.Status != run.StatusPaused || !strings.Contains(o.Reason, "limit: 2 logical calls") {
		t.Fatalf("%+v", o)
	}
}

// Every repository and explicit input must have a source_coverage row; citations must resolve.
func TestResearchMustCoverEverySource(t *testing.T) {
	f := newFixture(t, nil, nil)
	f.e.Manifest.Repos = append(f.e.Manifest.Repos, inputs.Repo{ID: "repo-2", Root: "/elsewhere"})
	f.e.Manifest.Inputs = []inputs.Source{{ID: "in-1", Origin: "spec.md", Status: "ok"}}
	d := research(r1)
	d["facts"].([]any)[0].(map[string]any)["source_id"] = "in-9"
	b, _ := json.Marshal(d)
	r, _ := schema.Parse[schema.Research](schema.KindResearch, b)
	p := strings.Join(f.e.checkResearch(r), "\n")
	for _, want := range []string{"repo-2", "in-1", "unknown source in-9"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in %q", want, p)
		}
	}
}

// §7: an assumption taken in --auto may not become a requirement; a user answer may.
func TestRequirementCannotRestOnAssumption(t *testing.T) {
	f := newFixture(t, nil, nil)
	saveDecisions(f.e.Run, []Decision{{ID: "Q-001", Source: "assumption", Answer: "override via ldflags"}, {ID: "Q-002", Source: "user", Answer: "semver"}})
	for id, bad := range map[string]bool{"Q-001": true, "Q-002": false} {
		d := research(r1)
		d["requirements"].([]any)[0].(map[string]any)["source_ids"] = []string{id}
		b, _ := json.Marshal(d)
		r, _ := schema.Parse[schema.Research](schema.KindResearch, b)
		got := strings.Contains(strings.Join(f.e.checkResearch(r), "\n"), "an assumption is not a user requirement")
		if got != bad {
			t.Errorf("%s: flagged=%v, want %v", id, got, bad)
		}
	}
	prompt, err := f.e.prompt(StageResearch, "planner", 1)
	if err != nil || !strings.Contains(prompt, "never create a requirement") {
		t.Fatalf("prompt does not state the assumption rule: %v", err)
	}
}

// Found live: the planner repeated settled questions in every revision. They are asked once.
func TestSettledQuestionsAreNotAskedAgain(t *testing.T) {
	p := &run.Progress{}
	q := []schema.Question{{Question: "Where does the version come from?", ProposedAssumption: "a constant"}}
	addQuestions(p, StageResearch, "planner", q, []Decision{{Question: "where does the  version come from?"}})
	if len(p.Pending) != 0 {
		t.Fatalf("settled question queued again: %+v", p.Pending)
	}
	addQuestions(p, StageResearch, "planner", append(q, q...), nil)
	if len(p.Pending) != 1 {
		t.Fatalf("duplicate in one batch: %+v", p.Pending)
	}
}
