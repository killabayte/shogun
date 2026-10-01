package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/config"
	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/library"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/golden.md")

var s1 = step{"S-001", []string{"R-001.C1"}}

// published runs research → outline → S-001 → integration → publish and returns the fixture.
func published(t *testing.T) *fixture {
	t.Helper()
	pl, rv := details([]reply{fixed(research(r1)), fixed(outline(s1))},
		[]reply{fixed(review("approve", researchOK, nil)), fixed(review("approve", outlineOK, nil))}, s1)
	rv = append(rv, fixed(finalReview("approve", nil, s1)))
	f := newFixture(t, pl, rv)
	if o := f.execute(t); o.Status != run.StatusApproved {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
	return f
}

func edit(t *testing.T, path, old, new string) {
	t.Helper()
	b, _ := os.ReadFile(path)
	if !bytes.Contains(b, []byte(old)) {
		t.Fatalf("%s does not contain %q", path, old)
	}
	os.WriteFile(path, bytes.Replace(b, []byte(old), []byte(new), 1), 0o644)
}

// §13 P5: body or immutable-metadata edits invalidate; status/tags/updated/log do not; markers,
// duplicate keys and run-id substitution are detected.
func TestPublishedPlanIntegrity(t *testing.T) {
	for _, tc := range []struct {
		name, old, new string
		want           library.Integrity
	}{
		{"status", "status: planned", "status: in_progress", library.Valid},
		{"tags", "    - plan\n", "    - plan\n    - done-soon\n", library.Valid},
		{"updated", "updated: ", "updated: 2099-01-01\nx_ignored_before: ", library.Changed}, // a new immutable key is a change
		{"execution log", "| S-001 | todo | — | — |", "| S-001 | done | 2026-09-27 me | PR #1 |", library.Valid},
		{"body", "## Approach", "## Approach (edited)", library.Changed},
		{"run id", "run_id: 20260926-000000-test-abcd", "run_id: 20260926-000000-other-ffff", library.Changed},
		{"planner", "planner: claude/fable:xhigh", "planner: claude/opus:xhigh", library.Changed},
		{"duplicate key", "status: planned", "status: planned\nstatus: done", library.InvalidFormat},
		{"duplicate marker", library.MarkerEnd, library.MarkerEnd + "\n" + library.MarkerEnd, library.InvalidFormat},
		{"reordered markers", library.MarkerBegin, library.MarkerEnd + "\n" + library.MarkerBegin, library.InvalidFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := published(t)
			path := f.e.State.Publish.Path
			edit(t, path, tc.old, tc.new)
			if got, note := library.Verify(path); got != tc.want {
				t.Fatalf("got %s (%s), want %s", got, note, tc.want)
			}
		})
	}
	// an updated date alone (the mutable key, same type) stays valid
	f := published(t)
	b, _ := os.ReadFile(f.e.State.Publish.Path)
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "updated: ") {
			lines[i] = "updated: \"2099-01-01\""
		}
	}
	os.WriteFile(f.e.State.Publish.Path, []byte(strings.Join(lines, "\n")), 0o644)
	if got, note := library.Verify(f.e.State.Publish.Path); got != library.Valid {
		t.Fatalf("updated edit: %s %s", got, note)
	}
}

// §13 P5: plan + receipt move without the run dir; a missing receipt is unverifiable.
func TestMovedPlanVerifiesWithoutRunDir(t *testing.T) {
	f := published(t)
	dst := filepath.Join(t.TempDir(), "library", "demo")
	os.MkdirAll(dst, 0o755)
	for _, p := range []string{f.e.State.Publish.Path, library.ReceiptPath(f.e.State.Publish.Path)} {
		b, _ := os.ReadFile(p)
		os.WriteFile(filepath.Join(dst, filepath.Base(p)), b, 0o644)
	}
	os.RemoveAll(f.e.Run.Dir)
	moved := filepath.Join(dst, filepath.Base(f.e.State.Publish.Path))
	if got, note := library.Verify(moved); got != library.Valid {
		t.Fatalf("moved: %s %s", got, note)
	}
	os.Remove(library.ReceiptPath(moved))
	if got, _ := library.Verify(moved); got != library.Unverifiable {
		t.Fatalf("missing receipt: %s", got)
	}
	// The receipt carries no absolute local paths.
	b, _ := os.ReadFile(filepath.Join(f.e.State.Publish.Path[:len(f.e.State.Publish.Path)-3] + ".approval.json"))
	if bytes.Contains(b, []byte(os.TempDir())) || bytes.Contains(b, []byte("/Users/")) {
		t.Fatalf("receipt leaks a local path: %s", b)
	}
}

// §13 P5: a crash between receipt and plan is repaired by running publish again; a foreign file at
// the output path is never clobbered.
func TestPublishRecoversAndNeverClobbers(t *testing.T) {
	f := published(t)
	out := f.e.State.Publish.Path
	os.Remove(out) // as if the process died after the receipt
	f.e.State.Publish.Done, f.e.State.Status = false, run.StatusRunning
	f.e.State.Cursor = run.Cursor{Stage: StagePublish}
	if o := f.execute(t); o.Status != run.StatusApproved {
		t.Fatalf("recover: %+v", o)
	}
	if got, _ := library.Verify(out); got != library.Valid {
		t.Fatalf("recovered plan: %s", got)
	}
	os.WriteFile(out, []byte("someone else's notes\n"), 0o644)
	f.e.State.Publish.Done, f.e.State.Status = false, run.StatusRunning
	if o := f.execute(t); o.Status != run.StatusFailed || !strings.Contains(o.Reason, "refusing to overwrite") {
		t.Fatalf("foreign file: %+v", o)
	}
	if b, _ := os.ReadFile(out); string(b) != "someone else's notes\n" {
		t.Fatal("foreign file was clobbered")
	}
}

// §9 result boundary: a finished call result that was not checkpointed is accepted once, without
// calling the model again.
func TestCrashAfterResultReusesIt(t *testing.T) {
	f := newFixture(t, []reply{fixed(research(r1))}, []reply{fixed(review("revise", researchOK, []map[string]any{finding("R-001", "minor", "x")}))})
	saved := *f.e.State
	saved.Progress = run.Progress{}
	f.e.Cfg.ReviewRounds = 1
	f.execute(t) // research planner + reviewer; stops on the round limit
	if calls(f.planner) != 1 {
		t.Fatalf("planner calls %d", calls(f.planner))
	}
	// Roll the state back to before the first call, as if the process died right after the
	// planner's result.json was written.
	g := newFixture(t, nil, nil)
	g.e.Run = f.e.Run
	st := saved
	g.e.State = &st
	g.e.Cfg.ReviewRounds = 1
	g.e.Manifest = f.e.Manifest
	g.execute(t)
	if calls(g.planner) != 0 || calls(g.reviewer) != 0 || !strings.Contains(g.log.String(), "recovered after a crash") {
		t.Fatalf("results were not reused: planner %d reviewer %d\n%s", calls(g.planner), calls(g.reviewer), g.log.String())
	}
}

// §13 P5: a reviewer refusal never gives an approved run.
func TestFinalReviewerRefusalIsNotApproval(t *testing.T) {
	pl, rv := details([]reply{fixed(research(r1)), fixed(outline(s1))},
		[]reply{fixed(review("approve", researchOK, nil)), fixed(review("approve", outlineOK, nil))}, s1)
	rv = append(rv, func(provider.Request) (any, error) {
		return nil, &provider.Error{Class: provider.ClassRefusal, Msg: "refused", Attempts: 1}
	})
	f := newFixture(t, pl, rv)
	if o := f.execute(t); o.Status != run.StatusFailed || f.e.State.Publish.Done {
		t.Fatalf("%+v", o)
	}
	if _, err := os.Stat(f.e.State.Publish.Path); err == nil {
		t.Fatal("a plan was published after a refusal")
	}
}

// The final review can send the work back to a step; the candidate is rebuilt and re-reviewed.
func TestFinalReviewReturnsToStep(t *testing.T) {
	pl, rv := details([]reply{fixed(research(r1)), fixed(outline(s1))},
		[]reply{fixed(review("approve", researchOK, nil)), fixed(review("approve", outlineOK, nil))}, s1)
	rv = append(rv, fixed(finalReview("revise", []map[string]any{finding("S-001", "major", "rollback misses the new file")}, s1)))
	pl = append(pl, fixed(withDeps(stepBatch(s1))))
	redo := stepReview("approve", nil, s1)
	redo["dispositions"] = []any{disposition("F-001", "resolved")}
	rv = append(rv, fixed(redo), fixed(finalReview("approve", nil, s1)))
	f := newFixture(t, pl, rv)
	if o := f.execute(t); o.Status != run.StatusApproved || f.e.State.Progress.Accepted["S-001"] != 2 {
		t.Fatalf("%+v %v\n%s", o, f.e.State.Progress.Accepted, f.log.String())
	}
}

// §13 P5: input drift — a repeated edit of an already dirty tracked file, or a new untracked file —
// stops publication until `resume --refresh`.
func TestDriftBlocksPublication(t *testing.T) {
	for _, change := range []string{"dirty-again", "untracked"} {
		t.Run(change, func(t *testing.T) {
			repo := t.TempDir()
			gitRun := func(args ...string) {
				c := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@e", "-c", "user.name=t"}, args...)...)
				if out, err := c.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %s", args, out)
				}
			}
			os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644)
			gitRun("init", "-q")
			gitRun("add", "-A")
			gitRun("commit", "-qm", "i")
			os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main // dirty\n"), 0o644) // dirty at snapshot time
			pl, rv := details([]reply{fixed(research(r1)), fixed(outline(s1))},
				[]reply{fixed(review("approve", researchOK, nil)), fixed(review("approve", outlineOK, nil))}, s1)
			rv = append(rv, func(provider.Request) (any, error) {
				if change == "dirty-again" {
					os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main // dirty, edited again\n"), 0o644)
				} else {
					os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("new\n"), 0o644)
				}
				return finalReview("approve", nil, s1), nil
			})
			f := newFixture(t, pl, rv)
			m, err := inputs.RepoManifest(context.Background(), "repo-1", repo)
			if err != nil {
				t.Fatal(err)
			}
			f.e.Manifest = &inputs.Manifest{Repos: []inputs.Repo{m}}
			o := f.execute(t)
			if o.Status != run.StatusPaused || !strings.Contains(o.Reason, "drift") || !strings.Contains(o.Reason, "--refresh") {
				t.Fatalf("%+v", o)
			}
			if _, err := os.Stat(f.e.State.Publish.Path); err == nil {
				t.Fatal("published despite drift")
			}
		})
	}
}

// --refresh starts a new generation: archived artifacts, new snapshots, research again, spend kept.
func TestRefreshStartsNewGeneration(t *testing.T) {
	f := published(t)
	f.e.Manifest.Version = inputs.ManifestVersion
	f.e.Manifest.Save(filepath.Join(f.e.Run.Dir, "manifest.json"))
	st := f.e.State
	before := st.Counters
	if err := Refresh(context.Background(), f.e.Run, st, f.e.Cfg); err != nil {
		t.Fatal(err)
	}
	if st.Generation != 2 || st.Cursor.Stage != StageResearch || st.Counters != before || len(st.Progress.Approved) != 0 || st.Publish.Done {
		t.Fatalf("state after refresh: gen %d cursor %+v counters %+v", st.Generation, st.Cursor, st.Counters)
	}
	if st.Limits.MaxLogicalCalls != before.LogicalCalls+4*f.e.Cfg.ReviewRounds || st.Limits.BaseLogicalCalls != before.LogicalCalls {
		t.Fatalf("limits %+v", st.Limits)
	}
	for _, p := range []string{"gen-1/research/1.json", "gen-1/approval.json", "gen-1/steps/S-001/1.json"} {
		if _, err := os.Stat(filepath.Join(f.e.Run.Dir, p)); err != nil {
			t.Errorf("not archived: %s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(f.e.Run.Dir, "research")); err == nil {
		t.Error("old research is still active")
	}
}

// §5 context heuristic at the final gate: an oversized candidate pauses, never approves.
func TestIntegrationContextLimit(t *testing.T) {
	pl, rv := details([]reply{fixed(research(r1)), fixed(outline(s1))},
		[]reply{fixed(review("approve", researchOK, nil)), fixed(review("approve", outlineOK, nil))}, s1)
	last := rv[len(rv)-1]
	f := newFixture(t, pl, nil)
	f.reviewer.replies = append(rv[:len(rv)-1], func(r provider.Request) (any, error) {
		f.e.Cfg.MaxContextTokens = 10 // from here on every call is far above the threshold
		return last(r)
	})
	o := f.execute(t)
	if o.Status != run.StatusPaused || !strings.Contains(o.Reason, "context_limit") || f.e.State.Cursor.Stage != StageIntegration {
		t.Fatalf("%+v", o)
	}
}

// §13 P5: the golden document — deterministic rendering with the Step-ID execution log and the
// explanation of what verify does and does not check.
func TestGoldenDocument(t *testing.T) {
	f := newFixture(t, nil, nil)
	e := f.e
	e.State.CreatedAt = "2026-09-26T10:00:00Z"
	e.State.RunID = "20260926-100000-version-flag-a1b2"
	e.Task = "Add a --version flag that prints the version and exits"
	e.Manifest = &inputs.Manifest{Repos: []inputs.Repo{{ID: "repo-1", Root: "/work/demo", IsGit: true, Head: "9096172da7ad0000", Fingerprint: "f00d"}}}
	e.Cfg = config.Default()
	b, _ := json.Marshal(research(r1, r2))
	e.Run.WriteArtifact("research/1.json", b)
	b, _ = json.Marshal(outline(s1, step{"S-002", []string{"R-002.C1"}}))
	e.Run.WriteArtifact("outline/1.json", b)
	for _, s := range []step{s1, {"S-002", []string{"R-002.C1"}}} {
		b, _ := json.Marshal(stepBatch(s)["steps"].([]any)[0])
		e.Run.WriteArtifact("steps/"+s.id+"/1.json", b)
	}
	e.State.Progress = run.Progress{Approved: map[string]int{StageResearch: 1, StageOutline: 1}, Accepted: map[string]int{"S-001": 1, "S-002": 1},
		Rounds: map[string]int{"research": 2, "outline": 1, "detail:S-001": 1, "detail:S-002": 1}}
	saveDecisions(e.Run, []Decision{{ID: "Q-001", Source: "user", Question: "Version source?", Answer: "a constant"}})
	d, err := e.loadPlanData()
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.render(d)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := e.render(d)
	if !bytes.Equal(got, again) {
		t.Fatal("rendering is not deterministic")
	}
	if _, err := library.Parse(got); err != nil {
		t.Fatalf("golden does not parse: %v", err)
	}
	golden := filepath.Join("testdata", "golden.md")
	if *updateGolden {
		os.MkdirAll("testdata", 0o755)
		os.WriteFile(golden, got, 0o644)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("rendered plan differs from %s (run with -update after an intended change):\n%s", golden, got)
	}
	for _, must := range []string{"## Execution log", "| S-001 | todo | — | — |", "| S-002 | todo | — | — |", "does not check that the work was done", library.MarkerBegin, library.MarkerEnd} {
		if !bytes.Contains(got, []byte(must)) {
			t.Errorf("golden lacks %q", must)
		}
	}
	_ = time.Now
}

// approvedRun is a fixture that runs up to publication when executed.
func approvedRun(t *testing.T) *fixture {
	t.Helper()
	pl, rv := details([]reply{fixed(research(r1)), fixed(outline(s1))},
		[]reply{fixed(review("approve", researchOK, nil)), fixed(review("approve", outlineOK, nil))}, s1)
	rv = append(rv, fixed(finalReview("approve", nil, s1)))
	return newFixture(t, pl, rv)
}

func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// S0: publication installs the frozen run manifest as a private sidecar beside the receipt and the
// plan, byte for byte, and the triplet verifies strictly.
func TestPublishWritesManifestSidecar(t *testing.T) {
	f := published(t)
	out := f.e.State.Publish.Path
	mp := library.ManifestPath(out)
	want, _ := os.ReadFile(filepath.Join(f.e.Run.Dir, "manifest.json"))
	got, err := os.ReadFile(mp)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("sidecar is not the frozen run manifest: %v", err)
	}
	for _, c := range []struct {
		path string
		mode os.FileMode
	}{{mp, 0o600}, {library.ReceiptPath(out), 0o644}, {out, 0o644}} {
		if p := perm(t, c.path); p != c.mode {
			t.Errorf("%s: mode %v, want %v", filepath.Base(c.path), p, c.mode)
		}
	}
	if res, note := library.VerifyWithManifest(out); res != library.Valid {
		t.Fatalf("triplet: %s %s", res, note)
	}
	if !strings.Contains(f.log.String(), "manifest "+filepath.Base(mp)) {
		t.Fatalf("log does not name the sidecar:\n%s", f.log.String())
	}
}

// S0: a run manifest that is not the one the receipt binds — a different digest, an inconsistent
// fingerprint field or repository, an unsupported version — blocks publication before any file
// is installed.
func TestPublishManifestMismatchBlocksBeforeInstall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(m *inputs.Manifest)
		want   string
	}{
		{"digest differs", func(m *inputs.Manifest) {
			m.Repos[0].Head = strings.Repeat("0", 40)
			m.Repos[0].Fingerprint = m.Repos[0].ComputeFingerprint()
			m.ComputeFingerprint()
		}, "does not match the approval receipt"},
		{"fingerprint field inconsistent", func(m *inputs.Manifest) { m.Fingerprint = strings.Repeat("0", 64) }, "does not match the approval receipt"},
		{"repository inconsistent", func(m *inputs.Manifest) { m.Repos[0].Fingerprint = strings.Repeat("0", 64) }, "inconsistent with its recorded digests"},
		{"unsupported version", func(m *inputs.Manifest) { m.Version = 99 }, "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := approvedRun(t)
			manPath := filepath.Join(f.e.Run.Dir, "manifest.json")
			m, err := inputs.Load(manPath)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(m)
			b, _ := json.MarshalIndent(m, "", " ")
			os.WriteFile(manPath, b, 0o600) // e.Manifest in memory, and so the receipt, keep the approved digest
			o := f.execute(t)
			if o.Status != run.StatusFailed || !strings.Contains(o.Reason, "manifest") || !strings.Contains(o.Reason, tc.want) {
				t.Fatalf("%+v\n%s", o, f.log.String())
			}
			out := f.e.State.Publish.Path
			for _, p := range []string{library.ManifestPath(out), library.ReceiptPath(out), out} {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("%s was installed despite the mismatch", filepath.Base(p))
				}
			}
		})
	}
}

// S0: a crash after the sidecar alone, or after sidecar and receipt, is repaired by publishing
// again without model calls; an accepted sidecar that had been widened is narrowed back to
// private; a foreign file or a symlink at the sidecar path is never clobbered.
func TestPublishRecoversSidecarGapsAndNeverClobbersSidecar(t *testing.T) {
	f := published(t)
	out := f.e.State.Publish.Path
	mp, rp := library.ManifestPath(out), library.ReceiptPath(out)
	planner, reviewer := calls(f.planner), calls(f.reviewer)
	republish := func() Outcome {
		f.e.State.Publish.Done, f.e.State.Status = false, run.StatusRunning
		f.e.State.Cursor = run.Cursor{Stage: StagePublish}
		return f.execute(t)
	}
	// gap after the sidecar only
	os.Remove(rp)
	os.Remove(out)
	os.Chmod(mp, 0o644)
	if o := republish(); o.Status != run.StatusApproved {
		t.Fatalf("sidecar-only gap: %+v\n%s", o, f.log.String())
	}
	if p := perm(t, mp); p != 0o600 {
		t.Fatalf("recovered sidecar mode %v, want private", p)
	}
	// gap after sidecar and receipt
	os.Remove(out)
	if o := republish(); o.Status != run.StatusApproved {
		t.Fatalf("plan gap: %+v", o)
	}
	if res, note := library.VerifyWithManifest(out); res != library.Valid {
		t.Fatalf("recovered triplet: %s %s", res, note)
	}
	if calls(f.planner) != planner || calls(f.reviewer) != reviewer {
		t.Fatal("recovery called a model")
	}
	// a foreign file at the sidecar path
	for _, p := range []string{mp, rp, out} {
		os.Remove(p)
	}
	os.WriteFile(mp, []byte("{}\n"), 0o600)
	if o := republish(); o.Status != run.StatusFailed || !strings.Contains(o.Reason, "refusing to overwrite") {
		t.Fatalf("foreign sidecar: %+v", o)
	}
	if b, _ := os.ReadFile(mp); string(b) != "{}\n" {
		t.Fatal("foreign sidecar was clobbered")
	}
	if _, err := os.Stat(rp); err == nil {
		t.Fatal("the receipt was installed after a sidecar conflict")
	}
	// a symlink at the sidecar path
	os.Remove(mp)
	target := filepath.Join(filepath.Dir(mp), "elsewhere.json")
	if err := os.Symlink(target, mp); err != nil {
		t.Fatal(err)
	}
	if o := republish(); o.Status != run.StatusFailed || !strings.Contains(o.Reason, "refusing to overwrite") {
		t.Fatalf("symlink sidecar: %+v", o)
	}
	if got, err := os.Readlink(mp); err != nil || got != target {
		t.Fatalf("symlink replaced: %q %v", got, err)
	}
}

// S0: a run whose manifest predates the sidecar does not exclude it from repository fingerprints.
// A sidecar left inside a studied repository by an interrupted publication is tolerated by the
// publish-time drift check only; the installed sidecar remains the frozen run bytes.
func TestPublishToleratesOwnSidecarForOlderManifests(t *testing.T) {
	repo := t.TempDir()
	gitRun := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@e", "-c", "user.name=t"}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644)
	gitRun("init", "-q")
	gitRun("add", "-A")
	gitRun("commit", "-qm", "i")
	f := approvedRun(t)
	out := filepath.Join(repo, "docs", "plans", "old.md")
	f.e.State.Publish.Path = out
	exclude := []string{out, library.ReceiptPath(out)} // the pre-S0 shape
	m, err := inputs.RepoManifest(context.Background(), "repo-1", repo, exclude...)
	if err != nil {
		t.Fatal(err)
	}
	man := &inputs.Manifest{Version: inputs.ManifestVersion, Workspace: repo, Repos: []inputs.Repo{m}, Exclude: exclude}
	man.ComputeFingerprint()
	manPath := filepath.Join(f.e.Run.Dir, "manifest.json")
	if err := man.Save(manPath); err != nil {
		t.Fatal(err)
	}
	f.e.Manifest = man
	frozen, _ := os.ReadFile(manPath)
	os.MkdirAll(filepath.Dir(out), 0o755)
	os.WriteFile(library.ManifestPath(out), frozen, 0o600) // left by an interrupted publication
	if _, err := inputs.CheckDrift(context.Background(), man); !errors.Is(err, inputs.ErrDrift) {
		t.Fatalf("precondition: without the tolerance the sidecar reads as drift, got %v", err)
	}
	if o := f.execute(t); o.Status != run.StatusApproved {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
	got, _ := os.ReadFile(library.ManifestPath(out))
	if !bytes.Equal(got, frozen) || bytes.Contains(got, []byte(library.ManifestPath(out))) {
		t.Fatal("the installed sidecar is not the frozen run manifest")
	}
	if res, note := library.VerifyWithManifest(out); res != library.Valid {
		t.Fatalf("%s %s", res, note)
	}
}

// S0: the triplet moves without the run dir and verifies strictly; the pair alone stays valid for
// plain verify but is unverifiable when the manifest is required; a missing, truncated,
// unsupported, mismatched or inconsistent sidecar is reported.
func TestMovedTripletVerifiesWithManifest(t *testing.T) {
	f := published(t)
	out := f.e.State.Publish.Path
	dst := filepath.Join(t.TempDir(), "library", "demo")
	os.MkdirAll(dst, 0o755)
	for _, p := range []string{out, library.ReceiptPath(out), library.ManifestPath(out)} {
		b, _ := os.ReadFile(p)
		os.WriteFile(filepath.Join(dst, filepath.Base(p)), b, 0o600)
	}
	os.RemoveAll(f.e.Run.Dir)
	moved := filepath.Join(dst, filepath.Base(out))
	if res, note := library.VerifyWithManifest(moved); res != library.Valid {
		t.Fatalf("moved triplet: %s %s", res, note)
	}
	mp := library.ManifestPath(moved)
	good, _ := os.ReadFile(mp)
	edited := func(mutate func(m *inputs.Manifest)) []byte {
		m, err := inputs.Decode(good)
		if err != nil {
			t.Fatal(err)
		}
		mutate(m)
		b, _ := json.MarshalIndent(m, "", " ")
		return b
	}
	for _, tc := range []struct {
		name string
		data []byte
		want library.Integrity
		note string
	}{
		{"truncated", good[:len(good)/2], library.Unverifiable, "corrupt"},
		{"unsupported version", edited(func(m *inputs.Manifest) { m.Version = 99 }), library.Unverifiable, "unsupported"},
		{"digest mismatch", edited(func(m *inputs.Manifest) {
			m.Repos[0].Head = strings.Repeat("0", 40)
			m.Repos[0].Fingerprint = m.Repos[0].ComputeFingerprint()
			m.ComputeFingerprint()
		}), library.Changed, "does not match"},
		{"fingerprint field inconsistent", edited(func(m *inputs.Manifest) { m.Fingerprint = strings.Repeat("0", 64) }), library.Changed, "inconsistent"},
		{"repository inconsistent", edited(func(m *inputs.Manifest) { m.Repos[0].Fingerprint = strings.Repeat("0", 64) }), library.Changed, "inconsistent"},
	} {
		os.WriteFile(mp, tc.data, 0o600)
		if res, note := library.VerifyWithManifest(moved); res != tc.want || !strings.Contains(note, tc.note) {
			t.Errorf("%s: got %s (%s), want %s containing %q", tc.name, res, note, tc.want, tc.note)
		}
		if res, _ := library.Verify(moved); res != library.Valid {
			t.Errorf("%s: plain verify must not depend on the sidecar, got %s", tc.name, res)
		}
	}
	os.Remove(mp)
	if res, note := library.VerifyWithManifest(moved); res != library.Unverifiable || !strings.Contains(note, "missing") {
		t.Fatalf("missing sidecar: %s %s", res, note)
	}
	if res, _ := library.Verify(moved); res != library.Valid {
		t.Fatalf("pair without sidecar: %s", res)
	}
}
