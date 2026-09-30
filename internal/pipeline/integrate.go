package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	for _, s := range d.steps {
		scope.KnownTargets[s.ID] = true
		for _, v := range s.Verification {
			scope.KnownVerifications[s.ID+"/"+v.ID] = true
		}
	}
	if len(d.outline.FinalVerificationCriterionIDs) > 0 {
		scope.KnownTargets["final"], scope.KnownVerifications["final"] = true, true
	}
	notes := []string(schema.CoverageGateStrict(rev, scope))
	notes = append(notes, associations(rev, planProvers(d))...)
	for i := range notes {
		notes[i] = "coverage: " + notes[i]
	}
	return notes
}

// planProvers maps every criterion of the plan to the steps that carry it and, for a criterion of
// the end-to-end check, to "final". The gate and the reviewer's prompt use the same map.
func planProvers(d *planData) criterionProvers {
	p := criterionProvers{}
	for _, s := range d.steps {
		for _, c := range s.CriterionIDs {
			p.add(c, s.ID)
		}
	}
	for _, c := range d.outline.FinalVerificationCriterionIDs {
		p.add(c, "final") // evidence only where the plan has an end-to-end check, and only for its criteria
	}
	return p
}

// lines renders the map for a prompt: "R-001.C1 → S-001, S-002", sorted by criterion.
func (p criterionProvers) lines() []string {
	var out []string
	for _, c := range slices.Sorted(maps.Keys(p)) {
		out = append(out, c+" → "+strings.Join(slices.Sorted(maps.Keys(p[c])), ", "))
	}
	return out
}

// criterionProvers maps a criterion to what may prove it: the steps that carry it and "final" for a
// criterion of the end-to-end check.
type criterionProvers map[string]map[string]bool

func (p criterionProvers) add(criterion, who string) {
	if p[criterion] == nil {
		p[criterion] = map[string]bool{}
	}
	p[criterion][who] = true
}

// carries reports whether who proves any criterion of the requirement.
func (p criterionProvers) carries(requirement, who string) bool {
	for c, ws := range p {
		if r, _, _ := strings.Cut(c, "."); r == requirement && ws[who] {
			return true
		}
	}
	return false
}

// associations checks that each coverage row cites evidence for every one of its criteria: each
// criterion is proved by at least one cited verification of a step that carries it (or "final" for a
// criterion of the end-to-end check). A requirement carried by several steps needs no citation from
// every one of them, only per-criterion proof. Every cited verification must belong to something
// that carries the row's criteria and to one of the row's targets. An extra target with no cited
// verification adds nothing to the proof and does not block; it never makes a step a carrier.
func associations(rev *schema.Review, provers criterionProvers) []string {
	var notes []string
	owner := func(ref string) string { s, _, _ := strings.Cut(ref, "/"); return s }
	for _, c := range rev.Coverage {
		for _, v := range c.VerificationRefs {
			switch o := owner(v); {
			case !provers.carries(c.RequirementID, o):
				notes = append(notes, fmt.Sprintf("%s cites verification %s of something that carries none of its criteria", c.RequirementID, v))
			case !contains(c.TargetIDs, o):
				notes = append(notes, fmt.Sprintf("%s cites verification %s, but %s is not a target of the row", c.RequirementID, v, o))
			}
		}
		for _, crit := range c.CriterionIDs {
			proved := false
			for _, v := range c.VerificationRefs {
				proved = proved || provers[crit][owner(v)]
			}
			if !proved {
				notes = append(notes, fmt.Sprintf("%s: no cited verification proves %s (it can be proved by %s)", c.RequirementID, crit, strings.Join(slices.Sorted(maps.Keys(provers[crit])), ", ")))
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

// publish writes exactly the reviewed bytes (§9): the frozen manifest of the approved generation
// as <stem>.manifest.json (S0), then the receipt, then the plan, each atomically and never over a
// different file; the run is approved only after the triplet verifies. It is idempotent: a crash
// between any two of the files is repaired by running it again, without model calls.
func (e *Engine) publish(ctx context.Context) *Outcome {
	if o := e.timeUp("publication"); o != nil {
		return o
	}
	if err := e.checkSnapshots(); err != nil {
		return e.fail("publish", err)
	}
	out := e.State.Publish.Path
	if out == "" {
		return e.fail("publish", errors.New("no output path recorded for this run"))
	}
	// Runs whose manifest predates the sidecar do not exclude it from repository fingerprints; a
	// sidecar left by an interrupted publication must not read as drift. The tolerance is for this
	// check only: the sidecar installed below stays the frozen run bytes.
	driftManifest := *e.Manifest
	driftManifest.Exclude = withExclusion(e.Manifest.Exclude, library.ManifestPath(out))
	octx, cancel := e.opCtx(ctx)
	drift, err := inputs.CheckDrift(octx, &driftManifest)
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
	manifest, err := os.ReadFile(filepath.Join(e.Run.Dir, "manifest.json"))
	if err != nil {
		return e.fail("publish", err)
	}
	if err := manifestMatchesReceipt(manifest, receipt); err != nil {
		return e.fail("publish", err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return e.fail("publish", err)
	}
	for _, f := range []struct {
		path string
		data []byte
		mode fs.FileMode
	}{{library.ManifestPath(out), manifest, 0o600}, {library.ReceiptPath(out), receipt, 0o644}, {out, candidate, 0o644}} {
		if err := installOnce(f.path, f.data, f.mode); err != nil {
			return e.fail("publish", err)
		}
	}
	if got, note := library.VerifyWithManifest(out); got != library.Valid {
		return e.fail("publish", fmt.Errorf("published plan does not verify: %s (%s)", got, note))
	}
	if err := e.Run.WriteArtifact("PLAN.md", candidate); err != nil {
		return e.fail("publish", err)
	}
	e.State.Publish.Done = true
	e.State.Cursor = run.Cursor{Stage: StagePublish}
	e.logf("[publish] %s (receipt %s, manifest %s)", out, filepath.Base(library.ReceiptPath(out)), filepath.Base(library.ManifestPath(out)))
	return e.stop(run.StatusApproved, "published "+out)
}

// manifestMatchesReceipt checks, before anything is installed, that the run's frozen manifest is
// the one the receipt binds: a supported version, repository fingerprints consistent with their
// recorded digests, and a recomputed digest equal to the receipt's manifest_digest and to the
// stored fingerprint field. The manifest is never rebuilt from today's repositories here.
func manifestMatchesReceipt(manifest, receipt []byte) error {
	var rc library.Receipt
	if err := json.Unmarshal(receipt, &rc); err != nil {
		return fmt.Errorf("approval.json does not parse: %w", err)
	}
	m, err := inputs.Decode(manifest)
	if err != nil {
		return fmt.Errorf("manifest.json: %w", err)
	}
	for _, r := range m.Repos {
		if r.ComputeFingerprint() != r.Fingerprint {
			return fmt.Errorf("manifest.json: repository %s fingerprint is inconsistent with its recorded digests", r.ID)
		}
	}
	stored := m.Fingerprint
	if got := m.ComputeFingerprint(); got != rc.ManifestDigest || got != stored {
		return fmt.Errorf("manifest.json does not match the approval receipt (manifest digest %.12s, receipt %.12s); the approval rests on the snapshot it was taken on", got, rc.ManifestDigest)
	}
	return nil
}

// withExclusion returns exclude plus path, without duplicates and without touching the original.
func withExclusion(exclude []string, path string) []string {
	if slices.Contains(exclude, path) {
		return exclude
	}
	return append(append([]string{}, exclude...), path)
}

// writeOnce installs data at path with the public output mode; see installOnce.
func writeOnce(path string, data []byte) error { return installOnce(path, data, 0o644) }

// installOnce installs data at path exclusively: the complete content is written to a temporary
// file in the same directory and hard-linked into place, which fails if any entry — a file, another
// publisher's result, a symlink, even a dangling one — already exists. An existing regular file with
// exactly data is accepted (idempotent recovery); anything else is an error (§9: never clobber). A
// private mode is re-applied to an accepted existing file, so a recovered sidecar never stays wider.
func installOnce(path string, data []byte, mode fs.FileMode) error {
	if err := existingMatches(path, data); !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return err
		}
		return tighten(path, mode)
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
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if err := existingMatches(path, data); errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("refusing to overwrite %s: another entry appeared there", path)
			} else if err != nil {
				return err
			}
			return tighten(path, mode)
		}
		return err
	}
	return nil
}

// tighten narrows the permission bits of an accepted existing file to a private mode; it never
// widens a file and leaves public outputs as they are.
func tighten(path string, mode fs.FileMode) error {
	if mode&0o077 != 0 {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&^mode == 0 {
		return nil
	}
	return os.Chmod(path, mode)
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
