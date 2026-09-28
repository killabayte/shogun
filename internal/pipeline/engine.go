// Package pipeline runs the planning stages (§5): each unit is planner → Shogun's mechanical
// checks → independent review, repeated until the stage gate passes or a limit, stalemate,
// question or error stops the run. P3 covers research and outline; detailing arrives in P4.
package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

// Stages in order; publish (integrate.go) follows integration.
const (
	StageResearch    = "research"
	StageOutline     = "outline"
	StageDetail      = "detail"
	StageIntegration = "integration"
)

// Fetcher archives web sources found by the planner (nil = inputs.Materializer).
type Fetcher func(ctx context.Context, runDir string, urls []string) ([]inputs.Source, error)

// Engine drives one run. It owns State while it runs and checkpoints it after every step.
type Engine struct {
	Run      *run.Run
	State    *run.State
	Manifest *inputs.Manifest
	Task     string
	Cfg      config.Config
	Planner  provider.Runner
	Reviewer provider.Runner
	Asker    Asker // nil = --auto or no terminal
	Fetch    Fetcher
	// Project names the plan's library folder and frontmatter; CLIVersions go into the receipt.
	Project     string
	CLIVersions map[string]string

	segStart time.Time // start of the current active segment (engine work, not user waits)
	Log      io.Writer
	Now      func() time.Time
}

// Outcome is where the run stopped.
type Outcome struct {
	Status run.Status
	Reason string
}

// Execute runs from the current cursor until the run stops.
func (e *Engine) Execute(ctx context.Context) Outcome {
	p := &e.State.Progress
	for _, m := range []*map[string]int{&p.Revisions, &p.Approved, &p.Rounds, &p.Accepted} {
		if *m == nil {
			*m = map[string]int{}
		}
	}
	if p.Stalemate == nil {
		p.Stalemate = map[string]string{}
	}
	if e.State.Cursor.Stage == "" || e.State.Cursor.Stage == "intake" {
		e.State.Cursor = run.Cursor{Stage: StagePlan}
		if e.State.Mode == ModeThorough {
			e.State.Cursor = run.Cursor{Stage: StageResearch}
		}
	}
	if e.State.Status == run.StatusApproved && e.State.Publish.Done {
		return Outcome{Status: run.StatusApproved, Reason: "published " + e.State.Publish.Path}
	}
	if missing := UnavailableInputs(e.Manifest); len(missing) > 0 {
		return *e.stop(run.StatusNeedsInput, MissingInputsReason(missing))
	}
	if err := e.checkSnapshots(); err != nil {
		return *e.stop(run.StatusFailed, err.Error())
	}
	e.State.Status, e.State.Reason = run.StatusRunning, ""
	e.segStart = time.Now()
	for {
		var o *Outcome
		switch e.State.Cursor.Stage {
		case StagePlan:
			o = e.fast(ctx)
		case StageIntegration:
			o = e.integrate(ctx)
		case StagePublish:
			o = e.publish(ctx)
		default:
			o = e.unit(ctx)
		}
		if o != nil {
			return *o
		}
	}
}

// unit runs review rounds of the unit under the cursor (research, outline, or one detail batch)
// until it is approved or sent back (cursor moves, nil) or the run stops.
func (e *Engine) unit(ctx context.Context) *Outcome {
	p := &e.State.Progress
	stage := e.State.Cursor.Stage
	if stage == StageDetail && e.State.Cursor.Step == "" {
		return e.nextBatch()
	}
	key := e.unitKey()
	for e.unitKey() == key {
		if len(p.Pending) > 0 {
			if o := e.resolveQuestions(ctx); o != nil {
				return o
			}
		}
		if p.Rounds[key] >= e.Cfg.ReviewRounds {
			return e.stop(run.StatusPaused, fmt.Sprintf("limit: %s used all %d review rounds", key, e.Cfg.ReviewRounds))
		}
		// Planner.
		rev := p.Revisions[key] + 1
		prompt, err := e.prompt(stage, "planner", rev)
		if err != nil {
			return e.fail("prompt", err)
		}
		if o, shrunk := e.fitsContext(stage, prompt, rev, false); o != nil || shrunk {
			return o
		}
		kind := map[string]schema.Kind{StageResearch: schema.KindResearch, StageOutline: schema.KindOutline, StageDetail: schema.KindStep}[stage]
		res, o := e.call(ctx, "planner", e.unitLabel(), prompt, kind, stage == StageResearch)
		if o != nil {
			return o
		}
		if err := e.Run.WriteArtifact(e.docRel(rev), res.Payload); err != nil {
			return e.fail("write revision", err)
		}
		p.Revisions[key] = rev
		doc, problems, err := e.check(stage, res.Payload)
		if err != nil {
			return e.fail("parse "+stage, err)
		}
		recordDisputes(p, doc.responses)
		settled, err := loadDecisions(e.Run.Dir)
		if err != nil {
			return e.fail("decisions", err)
		}
		addQuestions(p, key, "planner", doc.questions, settled)
		if len(problems) > 0 { // Shogun's contract checks fail: back to the planner, no review.
			p.GateNotes = problems
			p.Rounds[key]++
			e.logf("[%s] r%d: %d contract problem(s), back to the planner", key, rev, len(problems))
			if o := e.checkpoint(); o != nil {
				return o
			}
			continue
		}
		if stage == StageResearch {
			if o := e.archiveWeb(ctx, doc.research); o != nil {
				return o
			}
		}
		if doc.back != "" { // the planner itself says approved earlier work must change
			return e.backTo(doc.back, doc.backReasons)
		}
		if hasBlocking(p.Pending) { // answer first, then a new revision under the answers
			if o := e.checkpoint(); o != nil {
				return o
			}
			continue
		}
		if o := e.resolveQuestions(ctx); o != nil {
			return o
		}

		// Reviewer.
		prompt, err = e.prompt(stage, "reviewer", rev)
		if err != nil {
			return e.fail("prompt", err)
		}
		if o, shrunk := e.fitsContext(stage, prompt, rev, true); o != nil || shrunk {
			return o
		}
		rres, o := e.call(ctx, "reviewer", e.unitLabel(), prompt, schema.KindReview, false)
		if o != nil {
			return o
		}
		review, err := schema.Parse[schema.Review](schema.KindReview, rres.Payload)
		if err != nil {
			return e.fail("parse review", err)
		}
		where := fmt.Sprintf("%s r%d", key, rev)
		if err := e.Run.WriteArtifact(fmt.Sprintf("reviews/%s-%d.json", e.unitLabel(), rev), rres.Payload); err != nil {
			return e.fail("write review", err)
		}
		notes := applyReview(p, key, where, review, blocking(relevant(p, key)))
		p.Rounds[key]++
		e.State.Counters.ReviewRounds++
		addQuestions(p, key, "reviewer", review.Questions, settled)
		notes = append(notes, e.gate(stage, doc, review)...)
		notes = append(notes, e.sourceGate(key, review)...)
		e.logf("[%s] r%d: reviewer %s, %d blocking finding(s), %d gate note(s)", key, rev, review.Verdict, len(blocking(relevant(p, key))), len(notes))

		if target := e.backTarget(key); target != "" {
			return e.backTo(target, notes)
		}
		if review.Verdict == "approve" && len(notes) == 0 && len(blocking(relevant(p, key))) == 0 && len(p.Pending) == 0 {
			p.GateNotes = nil
			e.logf("[%s] approved at r%d", key, rev)
			return e.approve(stage, rev, doc, res.Payload)
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

// approve records an accepted unit and moves the cursor on (§5).
func (e *Engine) approve(stage string, rev int, doc *stageDoc, payload []byte) *Outcome {
	p := &e.State.Progress
	switch stage {
	case StageResearch:
		p.Approved[StageResearch] = rev
		freshUnits(p, func(k string) bool { return k == StageOutline || strings.HasPrefix(k, StageDetail+":") })
		e.State.Cursor = run.Cursor{Stage: StageOutline}
		e.State.Hashes["requirements"] = digest(payload)
	case StageOutline:
		p.Approved[StageOutline] = rev
		p.Accepted = map[string]int{} // a new skeleton resets every detail approval
		freshUnits(p, func(k string) bool { return strings.HasPrefix(k, StageDetail+":") })
		e.State.Cursor = run.Cursor{Stage: StageDetail}
		e.deriveBudget(len(doc.outline.Steps))
	case StageDetail:
		for _, st := range doc.steps.Steps {
			b, _ := json.Marshal(st)
			n := e.nextStepRevision(st.ID)
			if err := e.Run.WriteArtifact(fmt.Sprintf("steps/%s/%d.json", st.ID, n), b); err != nil {
				return e.fail("write step", err)
			}
			p.Accepted[st.ID] = n
		}
		e.State.Cursor = run.Cursor{Stage: StageDetail}
	}
	return e.checkpoint()
}

// freshUnits gives the units rebuilt on a new approval their own review rounds (§7: R rounds per
// unit). A step of a new skeleton is a new unit; the run's call ceiling C still bounds the total,
// and nothing is closed or forgotten in the ledger.
func freshUnits(p *run.Progress, rebuilt func(key string) bool) {
	for k := range p.Rounds {
		if rebuilt(k) {
			delete(p.Rounds, k)
			delete(p.Stalemate, k)
		}
	}
}

// stageDoc is the parsed planner document of either stage.
type stageDoc struct {
	research    *schema.Research
	outline     *schema.Outline
	steps       *schema.StepBatch
	questions   []schema.Question
	responses   []schema.ResponseToFinding
	back        string // research | outline | an accepted step id: approved work the planner asks to change
	backReasons []string
}

// check parses the planner document and applies Shogun's mechanical contract checks.
func (e *Engine) check(stage string, payload []byte) (*stageDoc, []string, error) {
	d := &stageDoc{}
	if stage == StageDetail {
		return e.checkDetail(payload)
	}
	if stage == StageResearch {
		r, err := schema.Parse[schema.Research](schema.KindResearch, payload)
		if err != nil {
			return nil, nil, err
		}
		d.research, d.questions, d.responses = r, r.Questions, r.ResponsesToFindings
		return d, e.checkResearch(r), nil
	}
	o, err := schema.Parse[schema.Outline](schema.KindOutline, payload)
	if err != nil {
		return nil, nil, err
	}
	d.outline, d.questions, d.responses = o, o.Questions, o.ResponsesToFindings
	for _, c := range o.RequestedChanges {
		if c.TargetID == StageResearch {
			d.back = StageResearch
			d.backReasons = append(d.backReasons, "outline planner requests a research change: "+c.Reason)
		}
	}
	reqs, err := e.requirements()
	if err != nil {
		return nil, nil, err
	}
	return d, schema.CheckOutline(o, reqs), nil
}

func (e *Engine) checkResearch(r *schema.Research) []string {
	p := e.checkResearchSources(r)
	// A requirement accepted before (the work came back from outline) cannot silently disappear or
	// become optional: that is the user's decision, not the model's.
	if prev, err := e.approvedResearch(); err == nil && prev != nil {
		now := map[string]schema.Requirement{}
		for _, q := range r.Requirements {
			now[q.ID] = q
		}
		for _, q := range prev.Requirements {
			if n, ok := now[q.ID]; !ok {
				p = append(p, fmt.Sprintf("previously accepted requirement %s was removed", q.ID))
			} else if q.Mandatory && !n.Mandatory {
				p = append(p, fmt.Sprintf("previously mandatory requirement %s became optional", q.ID))
			}
		}
	}
	return p
}

// checkResearchSources: the registry is well formed and every source is covered and cited correctly.
func (e *Engine) checkResearchSources(r *schema.Research) []string {
	p := schema.CheckRequirements(r.Requirements)
	if len(r.Requirements) == 0 {
		p = append(p, "the requirements registry is empty")
	}
	known := map[string]bool{"task": true}
	for _, w := range r.WebSources {
		known[w.URL] = true
	}
	decisions, _ := loadDecisions(e.Run.Dir)
	assumed := map[string]bool{}
	for _, d := range decisions {
		known[d.ID] = true
		assumed[d.ID] = d.Source == "assumption"
	}
	covered := map[string]bool{}
	for _, c := range r.SourceCoverage {
		covered[c.SourceID] = true
	}
	type origin struct{ id, what string }
	var mustCover []origin
	for _, r := range e.Manifest.Repos {
		mustCover = append(mustCover, origin{r.ID, r.Root})
	}
	for _, src := range e.sources() {
		mustCover = append(mustCover, origin{src.ID, src.Origin})
	}
	for _, o := range mustCover {
		known[o.id] = true
		if !covered[o.id] {
			p = append(p, fmt.Sprintf("source %s (%s) has no source_coverage row", o.id, o.what))
		}
	}
	for _, f := range r.Facts {
		if !known[f.SourceID] {
			p = append(p, fmt.Sprintf("fact %s cites unknown source %s", f.ID, f.SourceID))
		}
	}
	for _, req := range r.Requirements {
		for _, s := range req.SourceIDs {
			switch {
			case !known[s]:
				p = append(p, fmt.Sprintf("%s cites unknown source %s", req.ID, s))
			case assumed[s]:
				p = append(p, fmt.Sprintf("%s cites %s, an assumption: an assumption is not a user requirement (§7)", req.ID, s))
			}
		}
	}
	return p
}

// gate is the mechanical part of the stage gate on top of the verdict and the ledger (§6).
func (e *Engine) gate(stage string, d *stageDoc, rev *schema.Review) []string {
	if stage == StageDetail {
		return e.detailGate(d, rev)
	}
	var scope schema.CoverageScope
	if stage == StageResearch {
		scope = schema.ScopeForRequirements(d.research.Requirements, false)
		scope.RequireEvidence = false
		scope.KnownTargets = map[string]bool{}
		for _, f := range d.research.Facts {
			scope.KnownTargets[f.ID] = true
		}
		decisions, _ := loadDecisions(e.Run.Dir)
		for _, x := range decisions {
			scope.KnownTargets[x.ID] = true
		}
	} else {
		reqs, _ := e.requirements()
		scope = schema.ScopeForRequirements(reqs, true)
		scope.RequireEvidence = false
		scope.KnownTargets = map[string]bool{"final": true}
		for _, s := range d.outline.Steps {
			scope.KnownTargets[s.ID] = true
		}
	}
	notes := []string(schema.CoverageGateStrict(rev, scope))
	for _, c := range rev.Coverage {
		if c.Status == "covered" && len(c.TargetIDs) == 0 {
			notes = append(notes, c.RequirementID+" is covered without target ids")
		}
	}
	for i := range notes {
		notes[i] = "coverage: " + notes[i]
	}
	return notes
}

// relevant are the open findings a unit's gate depends on: those raised on it, those a later
// review addressed to it (target "research", "outline", or one of the batch's step ids), and, for a
// detail batch, those raised on any earlier batch that shared a step with it — so splitting or
// re-cutting a batch never leaves its unresolved findings behind. A finding aimed at one specific
// step of that earlier batch gates only that step.
func relevant(p *run.Progress, key string) []run.Finding {
	if key == StageIntegration || key == StagePlan { // a whole-plan review reconciles the whole ledger
		return openFindings(p)
	}
	targets := map[string]bool{key: true}
	mine := batchSteps(key)
	for id := range mine {
		targets[id] = true
	}
	var out []run.Finding
	for _, f := range openFindings(p) {
		parent := batchSteps(f.Stage)
		shared := false
		for id := range parent {
			shared = shared || mine[id]
		}
		// A finding addressed to one step of the old batch stays with that step only; a finding on
		// the batch as a whole (a requirement, a criterion, the batch) follows every part of it.
		if parent[f.TargetID] && !mine[f.TargetID] {
			shared = false
		}
		if f.Stage == key || targets[f.TargetID] || shared {
			out = append(out, f)
		}
	}
	return out
}

// batchSteps are the step ids of a detail unit key ("detail:S-001+S-002"); empty for other keys.
func batchSteps(key string) map[string]bool {
	out := map[string]bool{}
	if ids, ok := strings.CutPrefix(key, StageDetail+":"); ok {
		for _, id := range strings.Split(ids, "+") {
			out[id] = true
		}
	}
	return out
}

// sourceGate applies the reviewer's source assessments (§4.5, gate 6): an explicit or essential
// source that is not confirmed blocks approval, and an essential web source that Shogun could not
// archive becomes a blocking question (needs_input in --auto) whatever role the planner gave it.
func (e *Engine) sourceGate(stage string, rev *schema.Review) []string {
	archived := map[string]inputs.Source{}
	for _, s := range e.webRecords() {
		archived[s.Origin], archived[s.ID] = s, s
	}
	// Explicit sources are mandatory by host provenance (§4.5): the manifest decides, not the
	// role a model puts on the assessment.
	explicit := map[string]bool{}
	for _, r := range e.Manifest.Repos {
		explicit[r.ID] = true
	}
	for _, s := range e.Manifest.Inputs {
		explicit[s.ID] = true
	}
	var notes []string
	p := &e.State.Progress
	settled, _ := loadDecisions(e.Run.Dir)
	for _, a := range rev.SourceAssessments {
		if explicit[a.SourceID] {
			a.Role = "explicit"
		}
		if a.Role == "supporting" {
			continue
		}
		if a.Verdict != "confirmed" && a.Role == "explicit" {
			// The planner cannot make the reviewer confirm an explicit source, so what the source is
			// for is the user's call, recorded as one of two exact options for this very snapshot:
			// reference material (the task governs; the reviewer's verdict on it does not block) or
			// authoritative (the plan must follow it; the verdict stays a problem for the planner).
			// Without such a choice (no answer, an assumption, any other text) the user is asked.
			switch e.sourceRole(a.SourceID, settled) {
			case inputs.RoleReference:
				continue
			case roleAuthoritative:
				notes = append(notes, fmt.Sprintf("source %s is authoritative by the user's decision, but the reviewer finds it %s: %s", a.SourceID, a.Verdict, a.Note))
				continue
			}
			q := e.sourceQuestion(a.SourceID)
			q.Why = fmt.Sprintf("The reviewer found %s %s: %s", a.SourceID, a.Verdict, a.Note)
			askAgain(p, stage, q)
			continue
		}
		if a.Verdict != "confirmed" {
			notes = append(notes, fmt.Sprintf("source %s (%s) is %s by the reviewer: %s", a.SourceID, a.Role, a.Verdict, a.Note))
		}
		if s, ok := archived[a.SourceID]; ok && s.Status != "ok" && a.Role == "essential" {
			q := "An essential source could not be archived: " + s.Origin + " (" + s.Error + "). Provide it as a file (answers.json \"files\") or say how to proceed."
			addQuestions(p, stage, "shogun", []schema.Question{{Question: q, Why: "the reviewer classified it essential", Impact: "the research cannot be verified without it", Blocking: true}}, nil)
		}
	}
	return notes
}

// roleAuthoritative is the user's choice to keep an explicit source binding.
const roleAuthoritative = "authoritative"

// sourceQuestion asks what an explicit source is for. The text names the source and its snapshot
// digest, so a repeated verdict on the same bytes is the same question, and changed bytes (a refresh)
// are asked about again.
func (e *Engine) sourceQuestion(id string) schema.Question {
	origin, digest := id, ""
	for _, s := range e.Manifest.Inputs {
		if s.ID == id {
			origin, digest = s.Origin, s.SHA256
		}
	}
	for _, r := range e.Manifest.Repos {
		if r.ID == id {
			origin, digest = r.Root, r.Fingerprint
		}
	}
	return schema.Question{
		Question: fmt.Sprintf("The reviewer does not confirm the explicit source %s (%s, snapshot %s). What is %s for in this plan?", id, origin, short(digest), id),
		Impact:   "Unanswered, the plan cannot be approved. Answer with one option exactly (or its number); any other answer is asked again.",
		Options: []string{
			fmt.Sprintf("reference: %s is material to correct; where it differs from the task, the task governs", id),
			fmt.Sprintf("authoritative: %s is binding; revise the plan to follow it", id),
		},
		Blocking: true,
	}
}

// sourceRole is what the user made an explicit source: "reference" (by --reference, or by choosing
// that option for this snapshot), "authoritative" (by choosing that option), or "" (no valid choice).
// Only the latest user answer counts, only if it is one of the offered options; an assumption never
// does.
func (e *Engine) sourceRole(id string, settled []Decision) string {
	for _, s := range e.Manifest.Inputs {
		if s.ID == id && s.Role == inputs.RoleReference {
			return inputs.RoleReference
		}
	}
	q := e.sourceQuestion(id)
	role := ""
	for _, d := range settled {
		if d.Source == "assumption" || normQuestion(d.Question) != normQuestion(q.Question) {
			continue
		}
		switch a := normQuestion(d.Answer); a {
		case "1", normQuestion(q.Options[0]):
			role = inputs.RoleReference
		case "2", normQuestion(q.Options[1]):
			role = roleAuthoritative
		default:
			role = "" // a later unrecognised answer withdraws an earlier choice
		}
	}
	return role
}

// askAgain puts a blocking question up unless it is already pending. Unlike addQuestions it asks
// even when the question was answered before: an answer that is not one of the options resolves
// nothing.
func askAgain(p *run.Progress, stage string, q schema.Question) {
	for _, pq := range p.Pending {
		if normQuestion(pq.Question) == normQuestion(q.Question) {
			return
		}
	}
	p.NextQuestion++
	p.Pending = append(p.Pending, run.Pending{ID: fmt.Sprintf("Q-%03d", p.NextQuestion), Stage: stage, Origin: "shogun",
		Question: q.Question, Why: q.Why, Impact: q.Impact, Options: q.Options, Blocking: true, Closed: true})
}

func (e *Engine) webRecords() []inputs.Source {
	var web []inputs.Source
	if b, err := os.ReadFile(filepath.Join(e.Run.Dir, "web.json")); err == nil {
		json.Unmarshal(b, &web)
	}
	return web
}

// stalemate (§7): two reviews in a row on the same document with the same open blocker/major set,
// or a finding disputed twice without resolution. It becomes a question to the user naming both
// positions; with nothing concrete to ask the run pauses.
func (e *Engine) stalemate(stage string, payload []byte) *Outcome {
	p := &e.State.Progress
	sig := digest(payload) + openSignature(p)
	repeated := p.Stalemate[stage] == sig
	p.Stalemate[stage] = sig
	var stuck []run.Finding
	for _, f := range openFindings(p) {
		if repeated || f.Disputes >= 2 {
			stuck = append(stuck, f)
		}
	}
	if !repeated && len(stuck) == 0 {
		return nil
	}
	if len(stuck) == 0 {
		return e.stop(run.StatusPaused, "stalemate: the document and the open findings did not change")
	}
	for _, f := range stuck {
		p.NextQuestion++
		p.Pending = append(p.Pending, run.Pending{ID: fmt.Sprintf("Q-%03d", p.NextQuestion), Stage: stage, Origin: "shogun",
			Question: fmt.Sprintf("Planner and reviewer do not converge on %s: %s", f.ID, f.Problem),
			Why:      "reviewer asks: " + f.RequestedChange, Impact: "decides whether the plan must change on " + f.TargetID,
			Options: []string{"apply the reviewer's change", "keep the planner's version (the reviewer must reject the finding)"}, Blocking: true})
	}
	p.Stalemate[stage] = ""
	return nil
}

// call runs one logical model call, archives it and counts it against the budget.
func (e *Engine) call(ctx context.Context, role, stage, prompt string, kind schema.Kind, web bool) (*provider.Result, *Outcome) {
	st := e.State
	if o := e.budget(); o != nil {
		return nil, o
	}
	spec, runner := e.Cfg.Planner, e.Planner
	if role == "reviewer" {
		spec, runner = e.Cfg.Reviewer, e.Reviewer
	}
	bundle, err := schema.Bundle(kind)
	if err != nil {
		return nil, e.fail("schema", err)
	}
	roots := []string{}
	for _, r := range e.Manifest.Repos {
		roots = append(roots, r.Root)
	}
	roots = append(roots, e.Run.Dir)
	// The call may spend only what is left of the run's budget (§7): attempts and active time.
	l := st.Limits
	attempts := provider.MaxAttempts
	if l.MaxAttempts > 0 {
		attempts = min(attempts, l.MaxAttempts-st.Counters.Attempts)
	}
	deadline, budgetBound := e.Now().Add(e.Cfg.CallDeadline), false
	if l.MaxActiveSeconds > 0 {
		leftSec := l.MaxActiveSeconds - e.active()
		// Fast path: a planner call leaves a third of the run's time for the independent review.
		if stage == StagePlan && role == "planner" {
			leftSec -= l.MaxActiveSeconds / 3
			if leftSec < 60 {
				return nil, e.stop(run.StatusPaused, fmt.Sprintf("limit: %.0fs of active time left is not enough for another planner call and its review", l.MaxActiveSeconds-e.active())+e.draftNote())
			}
		}
		if left := time.Duration(leftSec * float64(time.Second)); e.Now().Add(left).Before(deadline) {
			deadline, budgetBound = e.Now().Add(left), true
		}
	}
	id := fmt.Sprintf("%04d-%s-%s", st.Counters.LogicalCalls+1, stage, role)
	// The recovery key binds a result to this request, this generation and these inputs (§9).
	reqDigest := digest([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%v\x00%s\x00gen=%d\x00inputs=%s", spec.Model, spec.Effort,
		digest([]byte(prompt)), digest(bundle), web, strings.Join(roots, "\x00"), st.Generation, e.Manifest.Fingerprint)))
	// §9 crash recovery: a finished result of this very request (a crash after the result but
	// before the checkpoint) is accepted once, from the base directory or any retry suffix; any
	// other leftover directory gets a fresh id.
	base := id
	for n := 1; exists(filepath.Join(e.Run.Dir, "calls", id)); n++ {
		if res, ok := reuseResult(filepath.Join(e.Run.Dir, "calls", id), reqDigest); ok {
			st.Counters.LogicalCalls++
			st.Counters.Attempts += res.Attempts
			st.Counters.ActiveSeconds += res.ActiveSeconds // spent by the process that crashed
			addUsages(&st.Counters, res.Usages)
			e.logf("[%s] %s call %s: finished result recovered after a crash, not called again", e.unitKey(), role, id)
			e.noteReported(role, res)
			return res, nil
		}
		id = fmt.Sprintf("%s-%d", base, n+1)
	}
	dir := filepath.Join(e.Run.Dir, "calls", id)
	if err := e.Run.WriteArtifact(filepath.Join("calls", id, "prompt.md"), []byte(prompt)); err != nil {
		return nil, e.fail("write prompt", err)
	}
	if err := writeJSON(filepath.Join(dir, "request.json"), map[string]any{"digest": reqDigest, "role": role, "unit": e.unitKey(),
		"model": spec.Model, "effort": spec.Effort, "web": web, "roots": roots}); err != nil {
		return nil, e.fail("write request", err)
	}
	req := provider.Request{Dir: dir, Model: spec.Model, Effort: string(spec.Effort), Prompt: prompt, Schema: bundle,
		Roots: roots, Web: web, Deadline: deadline, MaxAttempts: attempts}
	e.logf("[%s] %s call %s (%s)…", e.unitKey(), role, id, spec)
	start := time.Now()
	stopBeat := e.heartbeat(role, dir, attempts)
	res, err := runner.Run(ctx, req)
	stopBeat()
	spent := time.Since(start).Seconds()
	e.flushActive()
	st.Counters.LogicalCalls++
	var perr *provider.Error
	if err != nil && !errors.As(err, &perr) {
		perr = &provider.Error{Class: provider.ClassConfig, Msg: err.Error()}
	}
	if res != nil {
		st.Counters.Attempts += res.Attempts
		res.ActiveSeconds = spent // durable, so a recovered result restores the measured spend
		addUsages(&st.Counters, res.Usages)
		_ = writeJSON(filepath.Join(dir, "result.json"), res)
		e.noteReported(role, res)
		e.logSpend()
		return res, nil
	}
	st.Counters.Attempts += perr.Attempts
	addUsages(&st.Counters, perr.Usages)
	e.logSpend()
	_ = writeJSON(filepath.Join(dir, "result.json"), map[string]any{"error": perr.Class, "message": perr.Msg, "attempts": perr.Attempts})
	reason := fmt.Sprintf("%s %s call failed: %s", stage, role, perr)
	switch {
	case perr.Class == provider.ClassTimeout && budgetBound:
		return nil, e.stop(run.StatusPaused, fmt.Sprintf("limit: active time budget ran out during the %s %s call", stage, role)+e.draftNote())
	case perr.BudgetStopped:
		return nil, e.stop(run.StatusPaused, fmt.Sprintf("limit: attempt budget ran out during the %s %s call (%s)", stage, role, perr)+e.draftNote())
	}
	switch perr.Class {
	case provider.ClassRateLimit, provider.ClassTimeout:
		return nil, e.stop(run.StatusPaused, reason)
	case provider.ClassCanceled:
		return nil, e.stop(run.StatusPaused, "canceled: "+reason)
	}
	return nil, e.stop(run.StatusFailed, reason)
}

// reuseResult returns a successful result.json whose request digest matches, if there is one.
func reuseResult(dir, reqDigest string) (*provider.Result, bool) {
	var req struct {
		Digest string `json:"digest"`
	}
	if json.Unmarshal(mustRead(filepath.Join(dir, "request.json")), &req) != nil || req.Digest != reqDigest {
		return nil, false
	}
	var res provider.Result
	if json.Unmarshal(mustRead(filepath.Join(dir, "result.json")), &res) != nil || len(res.Payload) == 0 {
		return nil, false
	}
	return &res, true
}

// AddUsages books reported usages into counters (used by doctor for the preflight's spend).
func AddUsages(c *run.Counters, usages []json.RawMessage) { addUsages(c, usages) }

// addUsages books every attempt's reported usage once. An attempt without usage marks the totals
// incomplete instead of counting as zero.
func addUsages(c *run.Counters, usages []json.RawMessage) {
	if len(usages) == 0 {
		c.UsageIncomplete = true
	}
	for _, u := range usages {
		if !addUsage(c, u) {
			c.UsageIncomplete = true
		}
	}
}

// addUsage adds one attempt's usage: Claude's modelUsage (per model: uncached input, cache read,
// cache creation, output, thinking, reported cost) or Codex's turn usage (input includes cached input, which
// is split out; reasoning is kept separate). It reports whether any usage was found.
func addUsage(c *run.Counters, raw json.RawMessage) bool {
	var codex struct {
		InputTokens      *int64 `json:"input_tokens"`
		CachedInput      int64  `json:"cached_input_tokens"`
		CacheWriteTokens int64  `json:"cache_write_input_tokens"`
		OutputTokens     int64  `json:"output_tokens"`
		ReasoningTokens  int64  `json:"reasoning_output_tokens"`
	}
	if json.Unmarshal(raw, &codex) == nil && codex.InputTokens != nil {
		c.InputTokens += *codex.InputTokens - codex.CachedInput
		c.CacheReadTokens += codex.CachedInput
		c.CacheWriteTokens += codex.CacheWriteTokens
		c.OutputTokens += codex.OutputTokens
		c.ReasoningTokens += codex.ReasoningTokens
		return true
	}
	var claude map[string]struct {
		InputTokens, OutputTokens, CacheReadInputTokens, CacheCreationInputTokens, ThinkingTokens int64
		CostUSD                                                                                   float64
	}
	if json.Unmarshal(raw, &claude) != nil || len(claude) == 0 {
		return false
	}
	for _, m := range claude {
		c.InputTokens += m.InputTokens
		c.CacheReadTokens += m.CacheReadInputTokens
		c.CacheWriteTokens += m.CacheCreationInputTokens
		c.OutputTokens += m.OutputTokens
		c.ReasoningTokens += m.ThinkingTokens // part of output, like Codex's reasoning tokens
		c.CostUSD += m.CostUSD
	}
	return true
}

func (e *Engine) noteReported(role string, res *provider.Result) {
	if e.State.Progress.Reported == nil {
		e.State.Progress.Reported = map[string]string{}
	}
	e.State.Progress.Reported[role] = res.Reported.Model + ":" + res.Reported.Effort
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// budget stops before a call that would exceed the limits (§7).
func (e *Engine) budget() *Outcome {
	c, l := e.State.Counters, e.State.Limits
	switch {
	case l.MaxLogicalCalls > 0 && c.LogicalCalls >= l.MaxLogicalCalls:
		return e.stop(run.StatusPaused, fmt.Sprintf("limit: %d logical calls used (%s)", c.LogicalCalls, l.Source)+e.draftNote())
	case l.MaxAttempts > 0 && c.Attempts >= l.MaxAttempts:
		return e.stop(run.StatusPaused, fmt.Sprintf("limit: %d physical attempts used", c.Attempts)+e.draftNote())
	case l.MaxActiveSeconds > 0 && e.active() >= l.MaxActiveSeconds:
		return e.stop(run.StatusPaused, fmt.Sprintf("limit: %.0fs of active time used", e.active())+e.draftNote())
	}
	return nil
}

// opCtx bounds a host operation (downloads, repository scans) by the run's remaining active time.
func (e *Engine) opCtx(parent context.Context) (context.Context, context.CancelFunc) {
	if l := e.State.Limits.MaxActiveSeconds; l > 0 {
		return context.WithTimeout(parent, time.Duration((l-e.active())*float64(time.Second)))
	}
	return parent, func() {}
}

// timeUp stops the run when the active-time allowance is gone (before a phase starts, or when a
// host operation was cancelled by it).
func (e *Engine) timeUp(what string) *Outcome {
	if l := e.State.Limits.MaxActiveSeconds; l > 0 && e.active() >= l {
		return e.stop(run.StatusPaused, fmt.Sprintf("limit: the %.0fs active-time allowance ran out before %s; raise it with `resume --max-time`", l, what)+e.draftNote())
	}
	return nil
}

// active is the run's active time so far: engine work across all processes, user waits excluded.
func (e *Engine) active() float64 {
	if e.segStart.IsZero() {
		return e.State.Counters.ActiveSeconds
	}
	return e.State.Counters.ActiveSeconds + time.Since(e.segStart).Seconds()
}

// flushActive books the current active segment into the counters.
func (e *Engine) flushActive() {
	now := time.Now()
	if !e.segStart.IsZero() {
		e.State.Counters.ActiveSeconds += now.Sub(e.segStart).Seconds()
	}
	e.segStart = now
}

// draftNote points at the unapproved draft a limit leaves behind.
func (e *Engine) draftNote() string {
	if e.State.Cursor.Stage == StagePlan && exists(filepath.Join(e.Run.Dir, "candidate.md")) {
		return "; the last draft (NOT approved) is " + filepath.Join(e.Run.Dir, "candidate.md")
	}
	return ""
}

// logSpend prints running consumption after every call, so it is visible before it is too late.
func (e *Engine) logSpend() {
	e.logf("  spend so far: %s", SpendLine(e.State.Counters, e.State.Limits))
}

// SpendLine summarises consumption against the limits.
func SpendLine(c run.Counters, l run.Limits) string {
	usage := fmt.Sprintf("tokens: %d input, %d cache read, %d cache write, %d output, %d reasoning; Claude reported list-price equivalent $%.2f (not a subscription charge)",
		c.InputTokens, c.CacheReadTokens, c.CacheWriteTokens, c.OutputTokens, c.ReasoningTokens, c.CostUSD)
	if c.UsageIncomplete {
		usage += " — INCOMPLETE: some attempts reported no usage"
	}
	return fmt.Sprintf("%d/%s call(s), %d/%s attempt(s), %.1f/%s min active (+%.1f min waiting for you); %s",
		c.LogicalCalls, limitText(l.MaxLogicalCalls), c.Attempts, limitText(l.MaxAttempts), c.ActiveSeconds/60,
		minutesText(l.MaxActiveSeconds), c.WaitSeconds/60, usage)
}

func limitText(n int) string {
	if n <= 0 {
		return "∞"
	}
	return fmt.Sprint(n)
}

func minutesText(s float64) string {
	if s <= 0 {
		return "∞"
	}
	return fmt.Sprintf("%.0f", s/60)
}

// deriveBudget fixes C = 2R(B+3) at the first approved outline of a generation (§7). It never
// replenishes an already derived budget.
func (e *Engine) deriveBudget(steps int) {
	l := &e.State.Limits
	if l.Source == "derived" {
		return
	}
	k := max(e.Cfg.DetailBatch, 1)
	b := (steps + k - 1) / k
	c := 2 * e.Cfg.ReviewRounds * (b + 3)
	l.ExplicitAttempts = l.ExplicitAttempts || l.Source == "flag"
	l.MaxLogicalCalls, l.Source = l.BaseLogicalCalls+c, "derived"
	if !l.ExplicitAttempts {
		l.MaxAttempts = l.BaseAttempts + provider.MaxAttempts*c
	}
}

// archiveWeb fetches the planner's web sources so the reviewer checks archived bytes (§4.5). An
// unavailable essential source stops with needs_input; a supporting one is only noted.
func (e *Engine) archiveWeb(ctx context.Context, r *schema.Research) *Outcome {
	if len(r.WebSources) == 0 {
		return nil
	}
	var urls []string
	role := map[string]string{}
	for _, w := range r.WebSources {
		urls = append(urls, w.URL)
		role[w.URL] = w.Role
	}
	fetch := e.Fetch
	if fetch == nil {
		fetch = fetchWeb
	}
	octx, cancel := e.opCtx(ctx)
	srcs, _ := fetch(octx, e.Run.Dir, urls)
	cancel()
	if o := e.timeUp("the web sources were archived"); o != nil {
		return o
	}
	if err := writeJSON(filepath.Join(e.Run.Dir, "web.json"), srcs); err != nil {
		return e.fail("write web.json", err)
	}
	for _, s := range srcs {
		if s.Status != "ok" && role[s.Origin] == "essential" {
			p := &e.State.Progress
			p.NextQuestion++
			p.Pending = append(p.Pending, run.Pending{ID: fmt.Sprintf("Q-%03d", p.NextQuestion), Stage: StageResearch, Origin: "shogun",
				Question: "An essential web source is unavailable: " + s.Origin + " (" + s.Error + "). Provide it as a file (answers.json \"files\") or say how to proceed.",
				Why:      "a material decision rests on it", Impact: "research cannot be verified without it", Blocking: true})
			return e.needsInput("essential web source unavailable: " + s.Origin)
		}
		if s.Status != "ok" {
			e.logf("[research] supporting web source unavailable (not evidence): %s", s.Origin)
		}
	}
	return nil
}

func fetchWeb(ctx context.Context, runDir string, urls []string) ([]inputs.Source, error) {
	srcs, err := inputs.NewMaterializer().Materialize(ctx, filepath.Join(runDir, "web"), runDir, urls)
	for i := range srcs {
		srcs[i].ID = fmt.Sprintf("web-%d", i+1)
		if srcs[i].StoredPath != "" {
			srcs[i].StoredPath = filepath.Join("web", srcs[i].StoredPath)
		}
	}
	return srcs, err
}

// ---- prompt data ----

func (e *Engine) prompt(stage, role string, rev int) (string, error) {
	p := &e.State.Progress
	d := promptData{Stage: stage, Role: role, Revision: rev, Lang: e.Cfg.Lang, Task: e.Task,
		TaskPath: filepath.Join(e.Run.Dir, "task.md"), OpenFindings: relevant(p, e.unitKey()), GateNotes: p.GateNotes}
	for _, r := range e.Manifest.Repos {
		d.Repos = append(d.Repos, repoRef{ID: r.ID, Root: r.Root, Head: r.Head})
	}
	decisions, _ := loadDecisions(e.Run.Dir)
	for _, s := range e.sources() {
		srcRole := s.Role
		if e.sourceRole(s.ID, decisions) == inputs.RoleReference {
			srcRole = inputs.RoleReference // the user's choice for this snapshot, shown to both models
		}
		d.Inputs = append(d.Inputs, srcRef{ID: s.ID, Origin: s.Origin, Path: filepath.Join(e.Run.Dir, s.StoredPath), SHA: s.SHA256, Role: srcRole})
	}
	for _, s := range e.webRecords() {
		if s.Status == "ok" {
			d.Web = append(d.Web, srcRef{ID: s.ID, Origin: s.Origin, Path: filepath.Join(e.Run.Dir, s.StoredPath), SHA: s.SHA256})
		} else {
			d.WebFailed = append(d.WebFailed, srcRef{ID: s.ID, Origin: s.Origin, Path: s.Error})
		}
	}
	var err error
	if d.Decisions, err = loadDecisions(e.Run.Dir); err != nil {
		return "", err
	}
	path := func(st string, n int) string { return filepath.Join(e.Run.Dir, st, fmt.Sprintf("%d.json", n)) }
	if role == "planner" && rev > 1 {
		d.PrevPath = filepath.Join(e.Run.Dir, e.docRel(rev-1))
	}
	if role == "reviewer" {
		d.DocPath = filepath.Join(e.Run.Dir, e.docRel(rev))
	}
	if stage == StageOutline || stage == StageDetail {
		d.ResearchPath = path(StageResearch, p.Approved[StageResearch])
	}
	if stage == StageDetail {
		if err := e.detailPromptData(&d, role); err != nil {
			return "", err
		}
		return renderPrompt(stage, role, d)
	}
	if stage == StagePlan && role == "reviewer" {
		d.DocPath = filepath.Join(e.Run.Dir, "candidate.md")
	}
	if stage == StageIntegration || (stage == StagePlan && role == "reviewer") {
		reqs, err := e.requirements()
		if err != nil {
			return "", err
		}
		for _, r := range reqs {
			if r.Mandatory {
				var cs []string
				for _, c := range r.Criteria {
					cs = append(cs, c.ID)
				}
				d.Expected = append(d.Expected, r.ID+" ("+strings.Join(cs, ", ")+")")
			}
		}
		pd, err := e.loadPlanData()
		if err != nil {
			return "", err
		}
		d.Provers = planProvers(pd).lines()
		d.ResearchPath = ""
		return renderPrompt(stage, role, d)
	}
	if stage == StagePlan {
		return renderPrompt(stage, role, d)
	}
	if role == "reviewer" {
		var reqs []schema.Requirement
		if stage == StageResearch {
			r, err := schema.Parse[schema.Research](schema.KindResearch, mustRead(d.DocPath))
			if err != nil {
				return "", err
			}
			reqs = r.Requirements
		} else if reqs, err = e.requirements(); err != nil {
			return "", err
		}
		for _, r := range reqs {
			if stage == StageResearch || r.Mandatory {
				d.Expected = append(d.Expected, r.ID)
			}
		}
	}
	return renderPrompt(stage, role, d)
}

// UnavailableInputs lists explicit inputs that were not stored. Every explicit input is mandatory:
// such a run cannot continue (§4); P3 asks for a fresh `plan` once the material is available.
func UnavailableInputs(m *inputs.Manifest) []string {
	var out []string
	for _, s := range m.Inputs {
		if s.Status != "ok" {
			out = append(out, fmt.Sprintf("%s (%s: %s)", s.ID, s.Origin, s.Error))
		}
	}
	return out
}

// MissingInputsReason is the needs_input reason for unavailable explicit inputs.
func MissingInputsReason(missing []string) string {
	return "explicit input unavailable: " + strings.Join(missing, "; ") + "; make it available and start a new `shogun plan`"
}

// sources are the explicit inputs that were stored successfully (unavailable ones stop the run).
func (e *Engine) sources() []inputs.Source {
	var out []inputs.Source
	for _, s := range e.Manifest.Inputs {
		if s.Status == "ok" {
			out = append(out, s)
		}
	}
	return out
}

// approvedResearch is the last approved research revision. It stays in place while the work is
// back in research, so a new revision is checked against what was accepted before.
func (e *Engine) approvedResearch() (*schema.Research, error) {
	n := e.State.Progress.Approved[StageResearch]
	if n == 0 {
		return nil, nil
	}
	return schema.Parse[schema.Research](schema.KindResearch, mustRead(filepath.Join(e.Run.Dir, StageResearch, fmt.Sprintf("%d.json", n))))
}

func (e *Engine) requirements() ([]schema.Requirement, error) {
	r, err := e.approvedResearch()
	if err != nil || r == nil {
		return nil, fmt.Errorf("no approved research: %v", err)
	}
	return r.Requirements, nil
}

// ---- state transitions ----

func (e *Engine) checkpoint() *Outcome {
	e.flushActive()
	if err := e.Run.SaveState(e.State, e.Now()); err != nil {
		return &Outcome{Status: run.StatusFailed, Reason: "checkpoint: " + err.Error()}
	}
	return nil
}

func (e *Engine) stop(status run.Status, reason string) *Outcome {
	e.State.Status, e.State.Reason = status, reason
	e.logf("[%s] %s: %s", e.State.Cursor.Stage, status, reason)
	if o := e.checkpoint(); o != nil {
		return o
	}
	return &Outcome{Status: status, Reason: reason}
}

func (e *Engine) needsInput(reason string) *Outcome {
	if err := writeQuestions(e.Run, e.State.Progress.Pending); err != nil {
		return e.fail("write questions", err)
	}
	return e.stop(run.StatusNeedsInput, reason)
}

func (e *Engine) fail(what string, err error) *Outcome {
	return e.stop(run.StatusFailed, what+": "+err.Error())
}

func (e *Engine) logf(format string, a ...any) {
	if e.Log != nil {
		fmt.Fprintf(e.Log, format+"\n", a...)
	}
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func mustRead(p string) []byte { b, _ := os.ReadFile(p); return b }

func jsonUnmarshalStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return run.WriteFileAtomic(path, b, 0o600)
}
