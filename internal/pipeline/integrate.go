package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/library"
	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/run"
)

// StagePublish follows an approved integration review.
const StagePublish = "publish"

// integrate is the final gate (§5.4): Shogun assembles the candidate from typed data and the
// reviewer checks it end to end. There is no planner here; findings send the work back to the
// research, the outline or a step, after which the candidate is rebuilt and reviewed again.
func (e *Engine) integrate(ctx context.Context) *Outcome {
	p := &e.State.Progress
	key := StageIntegration
	for e.State.Cursor.Stage == StageIntegration {
		if len(p.Pending) > 0 {
			if o := e.resolveQuestions(ctx); o != nil {
				return o
			}
		}
		if p.Rounds[key] >= e.Cfg.ReviewRounds {
			return e.stop(run.StatusPaused, fmt.Sprintf("limit: %s used all %d review rounds", key, e.Cfg.ReviewRounds))
		}
		d, err := e.loadPlanData()
		if err != nil {
			return e.fail("integration", err)
		}
		candidate, err := e.render(d)
		if err != nil {
			return e.fail("render", err)
		}
		rev := p.Revisions[key] + 1
		if err := e.Run.WriteArtifact(e.docRel(rev), candidate); err != nil {
			return e.fail("write candidate", err)
		}
		if err := e.Run.WriteArtifact("candidate.md", candidate); err != nil {
			return e.fail("write candidate", err)
		}
		p.Revisions[key] = rev
		prompt, err := e.prompt(StageIntegration, "reviewer", rev)
		if err != nil {
			return e.fail("prompt", err)
		}
		if o, _ := e.fitsContext(StageIntegration, prompt, rev, true); o != nil {
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
		reviewRel := fmt.Sprintf("reviews/%s-%d.json", e.unitLabel(), rev)
		if err := e.Run.WriteArtifact(reviewRel, rres.Payload); err != nil {
			return e.fail("write review", err)
		}
		// Ledger reconciliation: the final review sees and must settle every open finding.
		notes := applyReview(p, key, fmt.Sprintf("%s r%d", key, rev), review, blocking(relevant(p, key)))
		p.Rounds[key]++
		e.State.Counters.ReviewRounds++
		settled, _ := loadDecisions(e.Run.Dir)
		addQuestions(p, key, "reviewer", review.Questions, settled)
		notes = append(notes, e.integrationGate(d, review)...)
		notes = append(notes, e.sourceGate(key, review)...)
		e.logf("[%s] r%d: reviewer %s, %d blocking finding(s), %d gate note(s)", key, rev, review.Verdict, len(blocking(relevant(p, key))), len(notes))
		if target := e.backTarget(key); target != "" {
			return e.backTo(target, notes)
		}
		if review.Verdict == "approve" && len(notes) == 0 && len(blocking(relevant(p, key))) == 0 && len(p.Pending) == 0 {
			p.GateNotes = nil
			if err := e.recordApproval(candidate, reviewRel); err != nil {
				return e.fail("approval", err)
			}
			e.logf("[%s] approved at r%d", key, rev)
			e.State.Cursor = run.Cursor{Stage: StagePublish}
			return e.checkpoint()
		}
		p.GateNotes = notes
		// The candidate's review history changes every round; what must not stand still is the
		// approved content itself.
		if o := e.stalemate(key, []byte(fmt.Sprint(p.Approved, p.Accepted))); o != nil {
			return o
		}
		if o := e.checkpoint(); o != nil {
			return o
		}
	}
	return nil
}

// integrationGate: the whole registry is covered with evidence (§6 gate 3/4). Every mandatory
// requirement has a row with all its criteria, targets that are accepted steps (or "final" for
// end-to-end checks) and verification references of those steps.
func (e *Engine) integrationGate(d *planData, rev *schema.Review) []string {
	scope := schema.ScopeForRequirements(d.research.Requirements, true)
	scope.KnownTargets = map[string]bool{}
	scope.KnownVerifications = map[string]bool{}
	carries := map[string]map[string]bool{} // requirement -> steps (and "final") that carry its criteria
	carry := func(c, who string) {
		r, _, _ := strings.Cut(c, ".")
		if carries[r] == nil {
			carries[r] = map[string]bool{}
		}
		carries[r][who] = true
	}
	inStep := map[string]bool{}
	for _, s := range d.steps {
		scope.KnownTargets[s.ID] = true
		for _, v := range s.Verification {
			scope.KnownVerifications[s.ID+"/"+v.ID] = true
		}
		for _, c := range s.CriterionIDs {
			carry(c, s.ID)
			inStep[c] = true
		}
	}
	// "final" is evidence only where the plan has an end-to-end check, and only for its criteria.
	// It must be cited only for a criterion no step carries; for the others it may be cited.
	allowed := map[string]map[string]bool{}
	for r, who := range carries {
		allowed[r] = map[string]bool{}
		for w := range who {
			allowed[r][w] = true
		}
	}
	for _, c := range d.outline.FinalVerificationCriterionIDs {
		scope.KnownTargets["final"], scope.KnownVerifications["final"] = true, true
		r, _, _ := strings.Cut(c, ".")
		if allowed[r] == nil {
			allowed[r] = map[string]bool{}
		}
		allowed[r]["final"] = true
		if !inStep[c] {
			carry(c, "final")
		}
	}
	notes := []string(schema.CoverageGateStrict(rev, scope))
	notes = append(notes, associations(rev, carries, allowed)...)
	for i := range notes {
		notes[i] = "coverage: " + notes[i]
	}
	return notes
}

// associations checks that each coverage row cites exactly the evidence its criteria are assigned
// to: every carrier (a step, or "final" for the end-to-end check) of a requirement is a target with a
// verification of its own ("S-NNN/V-NNN", or "final"), and nothing else is cited.
func associations(rev *schema.Review, carries, allowed map[string]map[string]bool) []string {
	var notes []string
	owner := func(ref string) string { s, _, _ := strings.Cut(ref, "/"); return s }
	for _, c := range rev.Coverage {
		for _, t := range c.TargetIDs {
			if !allowed[c.RequirementID][t] {
				notes = append(notes, fmt.Sprintf("%s cites %s, which carries none of its criteria", c.RequirementID, t))
			}
		}
		for _, v := range c.VerificationRefs {
			if !allowed[c.RequirementID][owner(v)] {
				notes = append(notes, fmt.Sprintf("%s cites verification %s of something that carries none of its criteria", c.RequirementID, v))
			}
		}
		for who := range carries[c.RequirementID] {
			if !contains(c.TargetIDs, who) {
				notes = append(notes, fmt.Sprintf("%s: %s carries its criteria but is not a target of the row", c.RequirementID, who))
			}
			proved := false
			for _, v := range c.VerificationRefs {
				proved = proved || owner(v) == who
			}
			if !proved {
				notes = append(notes, fmt.Sprintf("%s: %s has no verification cited for its criteria", c.RequirementID, who))
			}
		}
	}
	return notes
}

// checkSnapshots re-hashes every stored snapshot (explicit inputs, answer files, archived web
// sources) against its recorded digest (§4): the run dir is outside repository fingerprints, so a
// changed or missing snapshot is caught here, before work is resumed or published.
func (e *Engine) checkSnapshots() error {
	var bad []string
	for _, s := range append(e.sources(), e.webRecords()...) {
		if s.Status != "ok" || s.SHA256 == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(e.Run.Dir, s.StoredPath))
		if err != nil || digest(b) != s.SHA256 {
			bad = append(bad, s.ID+" ("+s.Origin+")")
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("input snapshot changed or missing: %s; the approvals rest on the recorded bytes, start a new `shogun plan`", strings.Join(bad, ", "))
	}
	return nil
}

// recordApproval binds the approval to the exact candidate bytes (§6 gate 7, §9): the body hash and
// the immutable metadata as parsed back from the candidate, plus the manifest digest, the
// registry revision, the review and the models.
func (e *Engine) recordApproval(candidate []byte, reviewRel string) error {
	doc, err := library.Parse(candidate)
	if err != nil {
		return fmt.Errorf("the rendered candidate does not parse as a plan: %w", err)
	}
	rc := library.Receipt{SchemaVersion: 1, PlanID: e.State.RunID, Revision: e.State.Generation, BodySHA256: doc.BodySHA256,
		ImmutableMetadata: library.ImmutableMetadata(doc.Frontmatter), ManifestDigest: e.Manifest.Fingerprint,
		RequirementsRevision: fmt.Sprint(e.State.Progress.Approved[StageResearch]), ReviewID: reviewRel,
		CLIVersions: e.CLIVersions,
		Requested:   map[string]string{"planner": e.Cfg.Planner.String(), "reviewer": e.Cfg.Reviewer.String()},
		Reported:    e.State.Progress.Reported, ApprovedAt: e.Now().UTC().Format("2006-01-02T15:04:05Z")}
	b, err := json.MarshalIndent(rc, "", " ")
	if err != nil {
		return err
	}
	e.State.Hashes["candidate"] = digest(candidate)
	return e.Run.WriteArtifact("approval.json", append(b, '\n'))
}

// publish writes exactly the reviewed bytes (§9): the receipt first, then the plan, each atomically
// and never over a different file; the run is approved only after both verify. It is idempotent: a
// crash between the two files is repaired by running it again, without model calls.
func (e *Engine) publish(ctx context.Context) *Outcome {
	if o := e.timeUp("publication"); o != nil {
		return o
	}
	if err := e.checkSnapshots(); err != nil {
		return e.fail("publish", err)
	}
	octx, cancel := e.opCtx(ctx)
	drift, err := inputs.CheckDrift(octx, e.Manifest)
	cancel()
	if o := e.timeUp("the drift check finished"); o != nil {
		return o
	}
	if err != nil {
		if errors.Is(err, inputs.ErrDrift) {
			return e.stop(run.StatusPaused, "drift: "+strings.Join(drift, ", ")+" changed since the snapshot; run `shogun resume --refresh`")
		}
		return e.fail("drift check", err)
	}
	candidate, err := os.ReadFile(filepath.Join(e.Run.Dir, "candidate.md"))
	if err != nil {
		return e.fail("publish", err)
	}
	receipt, err := os.ReadFile(filepath.Join(e.Run.Dir, "approval.json"))
	if err != nil {
		return e.fail("publish", err)
	}
	if digest(candidate) != e.State.Hashes["candidate"] {
		return e.fail("publish", errors.New("candidate.md differs from the approved candidate"))
	}
	out := e.State.Publish.Path
	if out == "" {
		return e.fail("publish", errors.New("no output path recorded for this run"))
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return e.fail("publish", err)
	}
	for _, f := range []struct {
		path string
		data []byte
	}{{library.ReceiptPath(out), receipt}, {out, candidate}} {
		if err := writeOnce(f.path, f.data); err != nil {
			return e.fail("publish", err)
		}
	}
	if got, note := library.Verify(out); got != library.Valid {
		return e.fail("publish", fmt.Errorf("published plan does not verify: %s (%s)", got, note))
	}
	if err := e.Run.WriteArtifact("PLAN.md", candidate); err != nil {
		return e.fail("publish", err)
	}
	e.State.Publish.Done = true
	e.State.Cursor = run.Cursor{Stage: StagePublish}
	e.logf("[publish] %s (receipt %s)", out, filepath.Base(library.ReceiptPath(out)))
	return e.stop(run.StatusApproved, "published "+out)
}

// writeOnce installs data at path exclusively: the complete content is written to a temporary file
// in the same directory and hard-linked into place, which fails if any entry — a file, another
// publisher's result, a symlink, even a dangling one — already exists. An existing regular file with
// exactly data is accepted (idempotent recovery); anything else is an error (§9: never clobber).
func writeOnce(path string, data []byte) error {
	if err := existingMatches(path, data); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	os.Chmod(tmp.Name(), 0o644)
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if err := existingMatches(path, data); errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("refusing to overwrite %s: another entry appeared there", path)
			} else {
				return err
			}
		}
		return err
	}
	return nil
}

// existingMatches is nil if path is a regular file holding exactly data, fs.ErrNotExist if there is
// no entry at all, and a refusal otherwise.
func existingMatches(path string, data []byte) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("refusing to overwrite %s: it exists and is not a regular file", path)
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(old, data) {
		return fmt.Errorf("refusing to overwrite %s: it exists and differs from the approved content", path)
	}
	return nil
}
