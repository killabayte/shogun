package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/run"
)

// The fast path (default since 2026-09-27, cost retrospective): one planner call writes the whole
// plan, Shogun checks it mechanically and renders the candidate, one reviewer call reviews exactly
// those bytes. A revision costs one more pair. At most FastRounds reviews; the staged pipeline stays
// available as `--thorough`.

// StagePlan is the fast path's single unit.
const StagePlan = "plan"

// FastRounds caps the fast path's reviews: a draft and at most one revision.
const FastRounds = 2

// ModeFast is recorded for new fast-path runs; older ones have an empty mode, which also means fast.
const ModeFast = "fast"

// ModeThorough selects the staged pipeline (research → outline → steps → final review).
const ModeThorough = "thorough"

func (e *Engine) fast(ctx context.Context) *Outcome {
	p := &e.State.Progress
	key := StagePlan
	for e.State.Cursor.Stage == StagePlan {
		if len(p.Pending) > 0 {
			if o := e.resolveQuestions(ctx); o != nil {
				return o
			}
		}
		if p.Rounds[key] >= FastRounds {
			return e.stop(run.StatusPaused, fmt.Sprintf("limit: the plan used all %d review rounds without passing the gate (%d blocking finding(s); last gate notes: %s)",
				FastRounds, len(blocking(relevant(p, key))), strings.Join(p.GateNotes, "; "))+e.draftNote())
		}
		rev := p.Revisions[key] + 1
		prompt, err := e.prompt(StagePlan, "planner", rev)
		if err != nil {
			return e.fail("prompt", err)
		}
		if o, _ := e.fitsContext(StagePlan, prompt, rev, false); o != nil {
			return o
		}
		res, o := e.call(ctx, "planner", key, prompt, schema.KindPlan, true)
		if o != nil {
			return o
		}
		if err := e.Run.WriteArtifact(e.docRel(rev), res.Payload); err != nil {
			return e.fail("write revision", err)
		}
		p.Revisions[key] = rev
		plan, problems, err := e.checkPlan(res.Payload)
		if err != nil {
			return e.fail("parse plan", err)
		}
		recordDisputes(p, plan.ResponsesToFindings)
		settled, _ := loadDecisions(e.Run.Dir)
		addQuestions(p, key, "planner", plan.Questions, settled)
		if len(problems) > 0 { // contract problems go back to the planner without a review
			p.GateNotes = problems
			p.Rounds[key]++
			e.logf("[plan] r%d: %d contract problem(s), back to the planner", rev, len(problems))
			if o := e.checkpoint(); o != nil {
				return o
			}
			continue
		}
		research := planResearch(plan)
		if o := e.archiveWeb(ctx, research); o != nil {
			return o
		}
		if hasBlocking(p.Pending) {
			if o := e.checkpoint(); o != nil {
				return o
			}
			continue
		}
		if o := e.resolveQuestions(ctx); o != nil {
			return o
		}
		if err := e.materialize(plan, research, rev); err != nil {
			return e.fail("plan", err)
		}
		d, err := e.loadPlanData()
		if err != nil {
			return e.fail("plan", err)
		}
		candidate, err := e.render(d)
		if err != nil {
			return e.fail("render", err)
		}
		if err := e.Run.WriteArtifact("candidate.md", candidate); err != nil {
			return e.fail("write candidate", err)
		}
		if err := e.Run.WriteArtifact(fmt.Sprintf("plan/%d.md", rev), candidate); err != nil {
			return e.fail("write candidate", err)
		}

		prompt, err = e.prompt(StagePlan, "reviewer", rev)
		if err != nil {
			return e.fail("prompt", err)
		}
		if o, _ := e.fitsContext(StagePlan, prompt, rev, true); o != nil {
			return o
		}
		rres, o := e.call(ctx, "reviewer", key, prompt, schema.KindReview, false)
		if o != nil {
			return o
		}
		review, err := schema.Parse[schema.Review](schema.KindReview, rres.Payload)
		if err != nil {
			return e.fail("parse review", err)
		}
		reviewRel := fmt.Sprintf("reviews/plan-%d.json", rev)
		if err := e.Run.WriteArtifact(reviewRel, rres.Payload); err != nil {
			return e.fail("write review", err)
		}
		notes := applyReview(p, key, fmt.Sprintf("plan r%d", rev), review, blocking(relevant(p, key)))
		p.Rounds[key]++
		e.State.Counters.ReviewRounds++
		addQuestions(p, key, "reviewer", review.Questions, settled)
		notes = append(notes, e.integrationGate(d, review)...)
		notes = append(notes, e.sourceGate(key, review)...)
		open := blocking(relevant(p, key))
		e.logf("[plan] r%d: reviewer %s, %d blocking finding(s), %d gate note(s)", rev, review.Verdict, len(open), len(notes))
		if review.Verdict == "approve" && len(notes) == 0 && len(open) == 0 && len(p.Pending) == 0 {
			p.GateNotes = nil
			if err := e.recordApproval(candidate, reviewRel); err != nil {
				return e.fail("approval", err)
			}
			if err := e.writeReviewNotes(); err != nil {
				return e.fail("review notes", err)
			}
			e.logf("[plan] approved at r%d", rev)
			e.State.Cursor = run.Cursor{Stage: StagePublish}
			return e.checkpoint()
		}
		p.GateNotes = notes
		if o := e.stalemate(key, res.Payload); o != nil {
			return o
		}
		if o := e.checkpoint(); o != nil {
			return o
		}
	}
	return nil
}

// checkPlan parses the single plan document and applies the same contracts as the staged
// pipeline: the registry and sources (research), the skeleton (every step cites a criterion, every
// mandatory criterion assigned, no cycles) and every step (criteria, dependencies, known repos).
func (e *Engine) checkPlan(payload []byte) (*schema.Plan, []string, error) {
	plan, err := schema.Parse[schema.Plan](schema.KindPlan, payload)
	if err != nil {
		return nil, nil, err
	}
	research := planResearch(plan)
	problems := e.checkResearchSources(research)
	o := planOutline(plan)
	problems = append(problems, schema.CheckOutline(o, research.Requirements)...)
	repos := map[string]bool{}
	for _, r := range e.Manifest.Repos {
		repos[r.ID] = true
	}
	for i := range plan.Steps {
		s := &plan.Steps[i]
		problems = append(problems, schema.CheckStep(s, o, research.Requirements)...)
		for _, t := range s.Targets {
			if !repos[t.RepoID] {
				problems = append(problems, fmt.Sprintf("%s targets unknown repository %s", s.ID, t.RepoID))
			}
		}
	}
	return plan, problems, nil
}

func planResearch(p *schema.Plan) *schema.Research {
	return &schema.Research{SchemaVersion: 1, Facts: p.Facts, Requirements: p.Requirements, SourceCoverage: p.SourceCoverage,
		Questions: p.Questions, WebSources: p.WebSources, RequestedChanges: p.RequestedChanges, ResponsesToFindings: p.ResponsesToFindings}
}

func planOutline(p *schema.Plan) *schema.Outline {
	o := &schema.Outline{SchemaVersion: 1, FinalVerificationCriterionIDs: p.FinalVerificationCriterionIDs,
		Questions: p.Questions, RequestedChanges: p.RequestedChanges, ResponsesToFindings: p.ResponsesToFindings}
	o.Approach.Summary = p.Approach.Summary
	o.Approach.Alternatives = []struct {
		Name   string `json:"name"`
		WhyNot string `json:"why_not"`
	}{} // an empty list, not null: the stored outline must satisfy its schema
	for _, a := range p.Approach.Alternatives {
		o.Approach.Alternatives = append(o.Approach.Alternatives, struct {
			Name   string `json:"name"`
			WhyNot string `json:"why_not"`
		}{a.Name, a.WhyNot})
	}
	for _, s := range p.Steps {
		o.Steps = append(o.Steps, schema.OutlineStep{ID: s.ID, Title: s.Title, Objective: s.Objective, Deliverable: s.Objective,
			DependsOn: s.DependsOn, CriterionIDs: s.CriterionIDs})
	}
	if o.FinalVerificationCriterionIDs == nil {
		o.FinalVerificationCriterionIDs = []string{}
	}
	return o
}

// materialize stores the draft as research/outline/steps artifacts so the renderer, the final gate
// and publication work exactly as in the staged pipeline. The pointers name the current draft; what
// is approved is recorded only by approval.json after the review.
func (e *Engine) materialize(plan *schema.Plan, research *schema.Research, rev int) error {
	p := &e.State.Progress
	for name, v := range map[string]any{StageResearch: research, StageOutline: planOutline(plan)} {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := e.Run.WriteArtifact(fmt.Sprintf("%s/%d.json", name, rev), b); err != nil {
			return err
		}
		p.Approved[name] = rev
	}
	p.Accepted = map[string]int{}
	for _, s := range plan.Steps {
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		n := e.nextStepRevision(s.ID)
		if err := e.Run.WriteArtifact(fmt.Sprintf("steps/%s/%d.json", s.ID, n), b); err != nil {
			return err
		}
		p.Accepted[s.ID] = n
	}
	return nil
}

// writeReviewNotes keeps the minor findings left open by the approving review next to the run
// (review-notes.md): the approved body was rendered before that review, so they cannot be in it.
func (e *Engine) writeReviewNotes() error {
	minors := openMinors(&e.State.Progress)
	if len(minors) == 0 {
		return nil
	}
	b := []byte("# Review notes (minor, not blocking)\n\n")
	for _, f := range minors {
		b = append(b, fmt.Sprintf("- %s on %s: %s — %s\n", f.ID, f.TargetID, clean(f.Problem), clean(f.RequestedChange))...)
	}
	e.logf("[plan] %d minor note(s) from the final review: %s", len(minors), filepath.Join(e.Run.Dir, "review-notes.md"))
	return e.Run.WriteArtifact("review-notes.md", b)
}

// blocking keeps blocker and major findings: minor ones never cost a round (retrospective §4).
func blocking(fs []run.Finding) []run.Finding {
	var out []run.Finding
	for _, f := range fs {
		if f.Severity == "blocker" || f.Severity == "major" {
			out = append(out, f)
		}
	}
	return out
}
