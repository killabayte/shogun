package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/run"
)

// A detail unit is one batch of up to detail_batch steps (§5), keyed "detail:S-001+S-002". The key
// holds the revisions, rounds, stalemate signature and findings of that batch.

func (e *Engine) unitKey() string {
	if e.State.Cursor.Stage == StageDetail {
		return StageDetail + ":" + e.State.Cursor.Step
	}
	return e.State.Cursor.Stage
}

// unitLabel is the key in a form usable in file names.
func (e *Engine) unitLabel() string {
	return strings.NewReplacer(":", "-", "+", "_").Replace(e.unitKey())
}

// docRel is the run-relative path of the planner document for revision rev of the current unit.
func (e *Engine) docRel(rev int) string {
	if e.State.Cursor.Stage == StageDetail {
		return fmt.Sprintf("detail/%s/%d.json", strings.ReplaceAll(e.State.Cursor.Step, "+", "_"), rev)
	}
	return fmt.Sprintf("%s/%d.json", e.State.Cursor.Stage, rev)
}

func (e *Engine) batchIDs() []string { return strings.Split(e.State.Cursor.Step, "+") }

func (e *Engine) approvedOutline() (*schema.Outline, error) {
	n := e.State.Progress.Approved[StageOutline]
	if n == 0 {
		return nil, fmt.Errorf("no approved outline")
	}
	return schema.Parse[schema.Outline](schema.KindOutline, mustRead(filepath.Join(e.Run.Dir, StageOutline, fmt.Sprintf("%d.json", n))))
}

// order is the outline's steps in dependency order, stable by their position in the outline.
func order(o *schema.Outline) []schema.OutlineStep {
	done := map[string]bool{}
	var out []schema.OutlineStep
	for len(out) < len(o.Steps) {
		progressed := false
		for _, s := range o.Steps {
			if done[s.ID] {
				continue
			}
			ready := true
			for _, d := range s.DependsOn {
				ready = ready && done[d]
			}
			if ready {
				done[s.ID], progressed = true, true
				out = append(out, s)
			}
		}
		if !progressed { // a cycle; CheckOutline rejected it already, keep the rest in list order
			for _, s := range o.Steps {
				if !done[s.ID] {
					done[s.ID] = true
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// nextBatch points the cursor at the next up to k steps that are not accepted, in dependency order,
// or at integration when every step is accepted.
func (e *Engine) nextBatch() *Outcome {
	o, err := e.approvedOutline()
	if err != nil {
		return e.fail("detail", err)
	}
	k := e.State.Progress.BatchSize
	if k <= 0 {
		k = max(e.Cfg.DetailBatch, 1)
	}
	var batch []string
	for _, s := range order(o) {
		if e.State.Progress.Accepted[s.ID] == 0 && len(batch) < k {
			batch = append(batch, s.ID)
		}
	}
	if len(batch) == 0 {
		e.State.Cursor = run.Cursor{Stage: StageIntegration}
		e.logf("[detail] every step is accepted")
		return e.checkpoint()
	}
	e.State.Cursor = run.Cursor{Stage: StageDetail, Step: strings.Join(batch, "+")}
	return e.checkpoint()
}

// checkDetail parses a step batch and applies Shogun's contract: exactly the batch's steps, each
// consistent with the approved outline (criteria, dependencies, no cycles, known repos).
func (e *Engine) checkDetail(payload []byte) (*stageDoc, []string, error) {
	b, err := schema.Parse[schema.StepBatch](schema.KindStep, payload)
	if err != nil {
		return nil, nil, err
	}
	d := &stageDoc{steps: b, questions: b.Questions, responses: b.ResponsesToFindings}
	o, err := e.approvedOutline()
	if err != nil {
		return nil, nil, err
	}
	reqs, err := e.requirements()
	if err != nil {
		return nil, nil, err
	}
	want := map[string]bool{}
	for _, id := range e.batchIDs() {
		want[id] = true
	}
	repos := map[string]bool{}
	for _, r := range e.Manifest.Repos {
		repos[r.ID] = true
	}
	var p []string
	got := map[string]bool{}
	for i := range b.Steps {
		s := &b.Steps[i]
		switch {
		case got[s.ID]:
			p = append(p, "step "+s.ID+" appears twice")
		case !want[s.ID]:
			p = append(p, fmt.Sprintf("step %s is not in this batch (%s); accepted steps change only through requested_changes", s.ID, e.State.Cursor.Step))
		}
		got[s.ID] = true
		p = append(p, schema.CheckStep(s, o, reqs)...)
		for _, t := range s.Targets {
			if !repos[t.RepoID] {
				p = append(p, fmt.Sprintf("%s targets unknown repository %s", s.ID, t.RepoID))
			}
		}
	}
	for _, id := range e.batchIDs() {
		if !got[id] {
			p = append(p, "the batch is missing step "+id)
		}
	}
	for _, c := range b.RequestedChanges {
		if c.TargetID != StageResearch && c.TargetID != StageOutline && e.State.Progress.Accepted[c.TargetID] == 0 {
			continue
		}
		d.back = e.earliest(d.back, c.TargetID)
		d.backReasons = append(d.backReasons, fmt.Sprintf("detail planner requests a change to %s: %s", c.TargetID, c.Reason))
	}
	return d, p, nil
}

// earliest keeps the target that reaches furthest back: research, then outline, then the step
// that comes first in dependency order (the order nextBatch and suffix invalidation use).
func (e *Engine) earliest(a, b string) string {
	rank := func(t string) int {
		switch t {
		case "":
			return 3
		case StageResearch:
			return 0
		case StageOutline:
			return 1
		}
		return 2
	}
	switch ra, rb := rank(a), rank(b); {
	case rb < ra:
		return b
	case rb == 2 && ra == 2 && e.before(b, a):
		return b
	}
	return a
}

// detailGate: coverage of exactly the criteria the outline assigned to the batch, with step targets
// and verification references of these steps; every step of the batch must be covered.
func (e *Engine) detailGate(d *stageDoc, rev *schema.Review) []string {
	o, _ := e.approvedOutline()
	crit := map[string][]string{}
	for _, s := range o.Steps {
		if strings.Contains("+"+e.State.Cursor.Step+"+", "+"+s.ID+"+") {
			for _, c := range s.CriterionIDs {
				req, _, _ := strings.Cut(c, ".")
				if !contains(crit[req], c) {
					crit[req] = append(crit[req], c)
				}
			}
		}
	}
	scope := schema.CoverageScope{ExpectedCriteria: crit, RequireEvidence: true,
		KnownTargets: map[string]bool{}, KnownVerifications: map[string]bool{}}
	for _, s := range d.steps.Steps {
		scope.KnownTargets[s.ID] = true
		for _, v := range s.Verification {
			scope.KnownVerifications[s.ID+"/"+v.ID] = true
		}
	}
	notes := []string(schema.CoverageGateStrict(rev, scope))
	// Associations (§5: "explicit results/coverage for each step"): every criterion the outline gave
	// a step must be covered in its requirement's row, with that step as a target and a verification
	// of that step; a row may cite only steps that carry some of its criteria.
	carries := map[string]map[string]bool{} // requirement -> steps of this batch that carry its criteria
	for _, s := range o.Steps {
		if !contains(e.batchIDs(), s.ID) {
			continue
		}
		for _, c := range s.CriterionIDs {
			r, _, _ := strings.Cut(c, ".")
			if carries[r] == nil {
				carries[r] = map[string]bool{}
			}
			carries[r][s.ID] = true
		}
	}
	for _, c := range rev.Coverage {
		for _, t := range c.TargetIDs {
			if !carries[c.RequirementID][t] {
				notes = append(notes, fmt.Sprintf("%s cites step %s, which carries none of its criteria", c.RequirementID, t))
			}
		}
		for _, v := range c.VerificationRefs {
			if step, _, _ := strings.Cut(v, "/"); !carries[c.RequirementID][step] {
				notes = append(notes, fmt.Sprintf("%s cites verification %s of a step that carries none of its criteria", c.RequirementID, v))
			}
		}
		for step := range carries[c.RequirementID] {
			if !contains(c.TargetIDs, step) {
				notes = append(notes, fmt.Sprintf("%s: step %s carries its criteria but is not a target of the row", c.RequirementID, step))
			}
			proved := false
			for _, v := range c.VerificationRefs {
				proved = proved || strings.HasPrefix(v, step+"/")
			}
			if !proved {
				notes = append(notes, fmt.Sprintf("%s: step %s has no verification cited for its criteria", c.RequirementID, step))
			}
		}
	}
	covered := map[string]bool{}
	for _, c := range rev.Coverage {
		for _, t := range c.TargetIDs {
			covered[t] = true
		}
	}
	for _, id := range e.batchIDs() {
		if !covered[id] {
			notes = append(notes, "step "+id+" does not appear in any coverage row")
		}
	}
	for i := range notes {
		notes[i] = "coverage: " + notes[i]
	}
	return notes
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// backTarget is where an open finding raised on this unit sends the work: research, the outline,
// or an already accepted step (which reopens it and everything after it).
func (e *Engine) backTarget(key string) string {
	target := ""
	for _, f := range openFindings(&e.State.Progress) {
		if f.Stage != key || f.TargetID == key {
			continue
		}
		switch {
		case f.TargetID == StageResearch && key != StageResearch,
			f.TargetID == StageOutline && strings.HasPrefix(key, StageDetail+":"),
			e.State.Progress.Accepted[f.TargetID] > 0 && !contains(e.batchIDs(), f.TargetID):
			target = e.earliest(target, f.TargetID)
		}
	}
	return target
}

// before reports whether step a comes before step b in dependency order.
func (e *Engine) before(a, b string) bool {
	o, err := e.approvedOutline()
	if err != nil {
		return false
	}
	pos := map[string]int{}
	for i, s := range order(o) {
		pos[s.ID] = i
	}
	return pos[a] < pos[b]
}

// backTo sends the work back (§5 immutability). Research or outline: every detail approval is reset
// (the registry or skeleton changes); the previous research approval stays as the baseline. A step:
// that step and the whole suffix after it lose their approval.
func (e *Engine) backTo(target string, notes []string) *Outcome {
	p := &e.State.Progress
	p.GateNotes = notes
	switch target {
	case StageResearch, StageOutline:
		e.logf("[%s] approved %s must change: back to %s", e.unitKey(), target, target)
		p.Accepted = map[string]int{}
		e.State.Cursor = run.Cursor{Stage: target}
	default:
		o, err := e.approvedOutline()
		if err != nil {
			return e.fail("detail", err)
		}
		reopen := false
		for _, s := range order(o) {
			reopen = reopen || s.ID == target
			if reopen {
				delete(p.Accepted, s.ID)
			}
		}
		e.logf("[%s] accepted step %s must change: it and every later step are reopened", e.unitKey(), target)
		e.State.Cursor = run.Cursor{Stage: StageDetail}
	}
	return e.checkpoint()
}

// nextStepRevision numbers accepted step files so an earlier acceptance is never overwritten.
func (e *Engine) nextStepRevision(id string) int {
	entries, _ := os.ReadDir(filepath.Join(e.Run.Dir, "steps", id))
	return len(entries) + 1
}

// fitsContext applies the context heuristic (§5: bytes/4 ≈ tokens against max_context_tokens) to a
// call's prompt plus the files it must read. An oversized detail batch is halved (shrunk=true, the
// cursor is reset); a single step or another stage pauses with context_limit — never an approve.
func (e *Engine) fitsContext(stage, prompt string, rev int, reviewer bool) (*Outcome, bool) {
	limit := e.Cfg.MaxContextTokens
	if limit <= 0 {
		return nil, false
	}
	size := len(prompt)
	for _, f := range e.mustRead(stage, rev, reviewer) {
		if st, err := os.Stat(f); err == nil {
			size += int(st.Size())
		}
	}
	if size/4 <= limit {
		return nil, false
	}
	if n := len(e.batchIDs()); stage == StageDetail && n > 1 {
		e.State.Progress.BatchSize = max(1, n/2)
		e.State.Cursor = run.Cursor{Stage: StageDetail}
		e.logf("[%s] ≈%d tokens exceed max_context_tokens %d: batch shrinks to %d", e.unitKey(), size/4, limit, e.State.Progress.BatchSize)
		return e.checkpoint(), true
	}
	return e.stop(run.StatusPaused, fmt.Sprintf("context_limit: the %s call needs ≈%d tokens, max_context_tokens is %d", e.unitKey(), size/4, limit)), false
}

// mustRead are the files a call is told to read in full.
func (e *Engine) mustRead(stage string, rev int, reviewer bool) []string {
	p := &e.State.Progress
	var files []string
	if n := p.Approved[StageResearch]; n > 0 && stage != StageResearch {
		files = append(files, filepath.Join(e.Run.Dir, StageResearch, fmt.Sprintf("%d.json", n)))
	}
	if n := p.Approved[StageOutline]; n > 0 && stage == StageDetail {
		files = append(files, filepath.Join(e.Run.Dir, StageOutline, fmt.Sprintf("%d.json", n)))
		for _, id := range e.dependencies() {
			files = append(files, filepath.Join(e.Run.Dir, "steps", id, fmt.Sprintf("%d.json", p.Accepted[id])))
		}
	}
	if reviewer {
		files = append(files, filepath.Join(e.Run.Dir, e.docRel(rev)))
	} else if rev > 1 {
		files = append(files, filepath.Join(e.Run.Dir, e.docRel(rev-1))) // the revision being revised
	}
	for _, s := range e.sources() {
		files = append(files, filepath.Join(e.Run.Dir, s.StoredPath))
	}
	for _, s := range e.webRecords() {
		if s.Status == "ok" {
			files = append(files, filepath.Join(e.Run.Dir, s.StoredPath))
		}
	}
	return files
}

// dependencies are the accepted steps the batch depends on, directly or transitively.
func (e *Engine) dependencies() []string {
	o, err := e.approvedOutline()
	if err != nil {
		return nil
	}
	byID := map[string]schema.OutlineStep{}
	for _, s := range o.Steps {
		byID[s.ID] = s
	}
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		for _, d := range byID[id].DependsOn {
			if !seen[d] {
				seen[d] = true
				walk(d)
			}
		}
	}
	for _, id := range e.batchIDs() {
		walk(id)
	}
	var out []string
	for id := range seen {
		if e.State.Progress.Accepted[id] > 0 {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

type stepRef struct {
	ID, Title, Objective, Deliverable string
	DependsOn, CriterionIDs           []string
	Path                              string // accepted steps: the file to read
}

// detailPromptData adds the outline, the batch, accepted steps and the expected coverage.
func (e *Engine) detailPromptData(d *promptData, role string) error {
	p := &e.State.Progress
	o, err := e.approvedOutline()
	if err != nil {
		return err
	}
	d.OutlinePath = filepath.Join(e.Run.Dir, StageOutline, fmt.Sprintf("%d.json", p.Approved[StageOutline]))
	deps := map[string]bool{}
	for _, id := range e.dependencies() {
		deps[id] = true
	}
	for _, s := range order(o) {
		ref := stepRef{ID: s.ID, Title: s.Title, Objective: s.Objective, Deliverable: s.Deliverable, DependsOn: s.DependsOn, CriterionIDs: s.CriterionIDs}
		switch {
		case contains(e.batchIDs(), s.ID):
			d.Batch = append(d.Batch, ref)
		case p.Accepted[s.ID] > 0:
			ref.Path = filepath.Join(e.Run.Dir, "steps", s.ID, fmt.Sprintf("%d.json", p.Accepted[s.ID]))
			if deps[s.ID] {
				d.Dependencies = append(d.Dependencies, ref)
			} else {
				d.Accepted = append(d.Accepted, ref)
			}
		}
	}
	if role == "reviewer" {
		crit := map[string][]string{}
		var reqs []string
		for _, s := range d.Batch {
			for _, c := range s.CriterionIDs {
				r, _, _ := strings.Cut(c, ".")
				if _, ok := crit[r]; !ok {
					reqs = append(reqs, r)
				}
				if !contains(crit[r], c) {
					crit[r] = append(crit[r], c)
				}
			}
		}
		sort.Strings(reqs)
		for _, r := range reqs {
			d.Expected = append(d.Expected, r+" ("+strings.Join(crit[r], ", ")+")")
		}
	}
	return nil
}
