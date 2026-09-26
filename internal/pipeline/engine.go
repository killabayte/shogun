// Package pipeline runs the planning stages (§5): each unit is planner → Shogun's mechanical
// checks → independent review, repeated until the stage gate passes or a limit, stalemate,
// question or error stops the run. P3 covers research and outline; detailing arrives in P4.
package pipeline

import (
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

// Stages in order. "detail" is where P3 stops.
const (
	StageResearch = "research"
	StageOutline  = "outline"
	StageDetail   = "detail"
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
	for _, m := range []*map[string]int{&p.Revisions, &p.Approved, &p.Rounds} {
		if *m == nil {
			*m = map[string]int{}
		}
	}
	if p.Stalemate == nil {
		p.Stalemate = map[string]string{}
	}
	if e.State.Cursor.Stage == "" || e.State.Cursor.Stage == "intake" {
		e.State.Cursor = run.Cursor{Stage: StageResearch}
	}
	if missing := UnavailableInputs(e.Manifest); len(missing) > 0 {
		return *e.stop(run.StatusNeedsInput, MissingInputsReason(missing))
	}
	e.State.Status, e.State.Reason = run.StatusRunning, ""
	for {
		stage := e.State.Cursor.Stage
		if stage != StageResearch && stage != StageOutline {
			return *e.stop(run.StatusPaused, "not_implemented: the detail stage arrives in P4 (outline approved)")
		}
		if o := e.unit(ctx, stage); o != nil {
			return *o
		}
	}
}

// unit runs review rounds of one stage until it is approved (cursor advances, nil) or stops.
func (e *Engine) unit(ctx context.Context, stage string) *Outcome {
	p := &e.State.Progress
	stageStart := stage
	for e.State.Cursor.Stage == stageStart {
		if len(p.Pending) > 0 {
			if o := e.resolveQuestions(ctx); o != nil {
				return o
			}
		}
		if p.Rounds[stage] >= e.Cfg.ReviewRounds {
			return e.stop(run.StatusPaused, fmt.Sprintf("limit: %s used all %d review rounds", stage, e.Cfg.ReviewRounds))
		}
		// Planner.
		rev := p.Revisions[stage] + 1
		prompt, err := e.prompt(stage, "planner", rev)
		if err != nil {
			return e.fail("prompt", err)
		}
		kind := schema.KindResearch
		if stage == StageOutline {
			kind = schema.KindOutline
		}
		res, o := e.call(ctx, "planner", stage, prompt, kind, stage == StageResearch)
		if o != nil {
			return o
		}
		if err := e.Run.WriteArtifact(fmt.Sprintf("%s/%d.json", stage, rev), res.Payload); err != nil {
			return e.fail("write revision", err)
		}
		p.Revisions[stage] = rev
		doc, problems, err := e.check(stage, res.Payload)
		if err != nil {
			return e.fail("parse "+stage, err)
		}
		recordDisputes(p, doc.responses)
		settled, err := loadDecisions(e.Run.Dir)
		if err != nil {
			return e.fail("decisions", err)
		}
		addQuestions(p, stage, "planner", doc.questions, settled)
		if len(problems) > 0 { // Shogun's contract checks fail: back to the planner, no review.
			p.GateNotes = problems
			p.Rounds[stage]++
			e.logf("[%s] r%d: %d contract problem(s), back to the planner", stage, rev, len(problems))
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
		if doc.wantsResearch { // the planner itself says the approved research must change
			return e.backToResearch(doc.researchReasons)
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
		rres, o := e.call(ctx, "reviewer", stage, prompt, schema.KindReview, false)
		if o != nil {
			return o
		}
		review, err := schema.Parse[schema.Review](schema.KindReview, rres.Payload)
		if err != nil {
			return e.fail("parse review", err)
		}
		where := fmt.Sprintf("%s r%d", stage, rev)
		if err := e.Run.WriteArtifact(fmt.Sprintf("reviews/%s-%d.json", stage, rev), rres.Payload); err != nil {
			return e.fail("write review", err)
		}
		notes := applyReview(p, stage, where, review, relevant(p, stage))
		p.Rounds[stage]++
		e.State.Counters.ReviewRounds++
		addQuestions(p, stage, "reviewer", review.Questions, settled)
		notes = append(notes, e.gate(stage, doc, review)...)
		notes = append(notes, e.sourceGate(stage, review)...)
		e.logf("[%s] r%d: reviewer %s, %d open finding(s), %d gate note(s)", stage, rev, review.Verdict, len(openFindings(p)), len(notes))

		if stage == StageOutline && targetsResearch(p) {
			return e.backToResearch(notes)
		}
		if review.Verdict == "approve" && len(notes) == 0 && len(relevant(p, stage)) == 0 && len(p.Pending) == 0 {
			p.Approved[stage], p.GateNotes = rev, nil
			if stage == StageResearch {
				e.State.Cursor = run.Cursor{Stage: StageOutline}
				e.State.Hashes["requirements"] = digest(res.Payload)
			} else {
				e.State.Cursor = run.Cursor{Stage: StageDetail}
				e.deriveBudget(len(doc.outline.Steps))
			}
			e.logf("[%s] approved at r%d", stage, rev)
			return e.checkpoint()
		}
		p.GateNotes = notes
		if o := e.stalemate(stage, res.Payload); o != nil {
			return o
		}
		if o := e.checkpoint(); o != nil {
			return o
		}
	}
	return nil
}

// stageDoc is the parsed planner document of either stage.
type stageDoc struct {
	research        *schema.Research
	outline         *schema.Outline
	questions       []schema.Question
	responses       []schema.ResponseToFinding
	wantsResearch   bool
	researchReasons []string
}

// check parses the planner document and applies Shogun's mechanical contract checks.
func (e *Engine) check(stage string, payload []byte) (*stageDoc, []string, error) {
	d := &stageDoc{}
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
			d.wantsResearch = true
			d.researchReasons = append(d.researchReasons, "outline planner requests a research change: "+c.Reason)
		}
	}
	reqs, err := e.requirements()
	if err != nil {
		return nil, nil, err
	}
	return d, schema.CheckOutline(o, reqs), nil
}

func (e *Engine) checkResearch(r *schema.Research) []string {
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

// gate is the mechanical part of the stage gate on top of the verdict and the ledger (§6).
func (e *Engine) gate(stage string, d *stageDoc, rev *schema.Review) []string {
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

// backToResearch returns the work to research. The previous approval stays as the baseline that
// the new revision is checked against; the outline is redone after the new approval.
func (e *Engine) backToResearch(notes []string) *Outcome {
	e.logf("[outline] the approved research must change: back to research")
	e.State.Progress.GateNotes = notes
	e.State.Cursor = run.Cursor{Stage: StageResearch}
	return e.checkpoint()
}

// relevant are the open findings a stage's gate depends on: its own, plus those an outline review
// addressed to research.
func relevant(p *run.Progress, stage string) []run.Finding {
	var out []run.Finding
	for _, f := range openFindings(p) {
		if f.Stage == stage || (stage == StageResearch && f.TargetID == StageResearch) {
			out = append(out, f)
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
	for _, a := range rev.SourceAssessments {
		if explicit[a.SourceID] {
			a.Role = "explicit"
		}
		if a.Role == "supporting" {
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

func (e *Engine) webRecords() []inputs.Source {
	var web []inputs.Source
	if b, err := os.ReadFile(filepath.Join(e.Run.Dir, "web.json")); err == nil {
		json.Unmarshal(b, &web)
	}
	return web
}

func targetsResearch(p *run.Progress) bool {
	for _, f := range p.Ledger {
		if f.Status == "open" && f.Stage == StageOutline && f.TargetID == StageResearch {
			return true
		}
	}
	return false
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
	id := fmt.Sprintf("%04d-%s-%s", st.Counters.LogicalCalls+1, stage, role)
	dir := filepath.Join(e.Run.Dir, "calls", id)
	if err := e.Run.WriteArtifact(filepath.Join("calls", id, "prompt.md"), []byte(prompt)); err != nil {
		return nil, e.fail("write prompt", err)
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
		if left := time.Duration((l.MaxActiveSeconds - st.Counters.ActiveSeconds) * float64(time.Second)); e.Now().Add(left).Before(deadline) {
			deadline, budgetBound = e.Now().Add(left), true
		}
	}
	req := provider.Request{Dir: dir, Model: spec.Model, Effort: string(spec.Effort), Prompt: prompt, Schema: bundle,
		Roots: roots, Web: web, Deadline: deadline, MaxAttempts: attempts}
	e.logf("[%s] %s call %s (%s)…", stage, role, id, spec)
	start := time.Now()
	res, err := runner.Run(ctx, req)
	st.Counters.LogicalCalls++
	st.Counters.ActiveSeconds += time.Since(start).Seconds()
	var perr *provider.Error
	if err != nil && !errors.As(err, &perr) {
		perr = &provider.Error{Class: provider.ClassConfig, Msg: err.Error()}
	}
	if res != nil {
		st.Counters.Attempts += res.Attempts
		_ = writeJSON(filepath.Join(dir, "result.json"), res)
		return res, nil
	}
	st.Counters.Attempts += perr.Attempts
	_ = writeJSON(filepath.Join(dir, "result.json"), map[string]any{"error": perr.Class, "message": perr.Msg, "attempts": perr.Attempts})
	reason := fmt.Sprintf("%s %s call failed: %s", stage, role, perr)
	switch {
	case perr.Class == provider.ClassTimeout && budgetBound:
		return nil, e.stop(run.StatusPaused, fmt.Sprintf("limit: active time budget ran out during the %s %s call", stage, role))
	case perr.BudgetStopped:
		return nil, e.stop(run.StatusPaused, fmt.Sprintf("limit: attempt budget ran out during the %s %s call (%s)", stage, role, perr))
	}
	switch perr.Class {
	case provider.ClassRateLimit, provider.ClassTimeout:
		return nil, e.stop(run.StatusPaused, reason)
	case provider.ClassCanceled:
		return nil, e.stop(run.StatusPaused, "canceled: "+reason)
	}
	return nil, e.stop(run.StatusFailed, reason)
}

// budget stops before a call that would exceed the limits (§7).
func (e *Engine) budget() *Outcome {
	c, l := e.State.Counters, e.State.Limits
	switch {
	case l.MaxLogicalCalls > 0 && c.LogicalCalls >= l.MaxLogicalCalls:
		return e.stop(run.StatusPaused, fmt.Sprintf("limit: %d logical calls used (%s)", c.LogicalCalls, l.Source))
	case l.MaxAttempts > 0 && c.Attempts >= l.MaxAttempts:
		return e.stop(run.StatusPaused, fmt.Sprintf("limit: %d attempts used", c.Attempts))
	case l.MaxActiveSeconds > 0 && c.ActiveSeconds >= l.MaxActiveSeconds:
		return e.stop(run.StatusPaused, fmt.Sprintf("limit: %.0fs active time used", c.ActiveSeconds))
	}
	return nil
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
	flag := l.Source == "flag"
	l.MaxLogicalCalls, l.Source = c, "derived"
	if !flag {
		l.MaxAttempts = provider.MaxAttempts * c
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
	srcs, _ := fetch(ctx, e.Run.Dir, urls)
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
		TaskPath: filepath.Join(e.Run.Dir, "task.md"), OpenFindings: relevant(p, stage), GateNotes: p.GateNotes}
	for _, r := range e.Manifest.Repos {
		d.Repos = append(d.Repos, repoRef{ID: r.ID, Root: r.Root, Head: r.Head})
	}
	for _, s := range e.sources() {
		d.Inputs = append(d.Inputs, srcRef{ID: s.ID, Origin: s.Origin, Path: filepath.Join(e.Run.Dir, s.StoredPath), SHA: s.SHA256})
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
		d.PrevPath = path(stage, rev-1)
	}
	if role == "reviewer" {
		d.DocPath = path(stage, rev)
	}
	if stage == StageOutline {
		d.ResearchPath = path(StageResearch, p.Approved[StageResearch])
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

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return run.WriteFileAtomic(path, b, 0o600)
}
