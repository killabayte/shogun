package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/library"
	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/provider"
	"github.com/killabayte/shogun/internal/run"
)

func TestP5ReviewFinalCoverageCannotCiteAbsentFinalCheck(t *testing.T) {
	pl, rv := details([]reply{fixed(research(r1)), fixed(outline(s1))},
		[]reply{fixed(review("approve", researchOK, nil)), fixed(review("approve", outlineOK, nil))}, s1)
	d := finalReview("approve", nil, s1)
	c := d["coverage"].([]any)[0].(map[string]any)
	c["target_ids"], c["verification_refs"] = []string{"final"}, []string{"final"}
	f := newFixture(t, pl, append(rv, fixed(d)))
	f.e.Cfg.ReviewRounds = 1
	o := f.execute(t)
	if o.Status == run.StatusApproved {
		t.Fatalf("published using a nonexistent final check: %+v", o)
	}
}

func TestP5ReviewFinalCoverageCannotCrossWireRequirements(t *testing.T) {
	s2 := step{"S-002", r2.crit}
	final := finalReview("approve", nil, s1, s2)
	rows := final["coverage"].([]any)
	rows[0].(map[string]any)["target_ids"], rows[0].(map[string]any)["verification_refs"] = []string{"S-002"}, []string{"S-002/V-001"}
	rows[1].(map[string]any)["target_ids"], rows[1].(map[string]any)["verification_refs"] = []string{"S-001"}, []string{"S-001/V-001"}
	f := newFixture(t,
		[]reply{fixed(research(r1, r2)), fixed(outline(s1, s2)), fixed(stepBatch(s1)), fixed(stepBatch(s2))},
		[]reply{fixed(review("approve", []row{
			{"R-001", "covered", r1.crit, []string{"FACT-001"}}, {"R-002", "covered", r2.crit, []string{"FACT-001"}},
		}, nil)), fixed(review("approve", []row{
			{"R-001", "covered", r1.crit, []string{"S-001"}}, {"R-002", "covered", r2.crit, []string{"S-002"}},
		}, nil)), fixed(stepReview("approve", nil, s1)), fixed(stepReview("approve", nil, s2)), fixed(final)})
	f.e.Cfg.ReviewRounds = 1
	if o := f.execute(t); o.Status == run.StatusApproved {
		t.Fatalf("final gate published cross-wired requirement evidence: %+v", o)
	}
}

func TestP5ReviewChangedInputSnapshotBlocksPublication(t *testing.T) {
	researchDoc := research(r1)
	researchDoc["source_coverage"] = append(researchDoc["source_coverage"].([]any), map[string]any{
		"source_id": "in-1", "studied": "policy.txt", "relevance": "relevant", "notes": "mandatory policy"})
	pl, rv := details([]reply{fixed(researchDoc), fixed(outline(s1))},
		[]reply{fixed(review("approve", researchOK, nil)), fixed(review("approve", outlineOK, nil))}, s1)
	f := newFixture(t, pl, rv)
	const rel = "inputs/policy.txt"
	before := []byte("Keep the public API.\n")
	if err := f.e.Run.WriteArtifact(rel, before); err != nil {
		t.Fatal(err)
	}
	f.e.Manifest.Inputs = []inputs.Source{{ID: "in-1", Origin: "policy.txt", StoredPath: rel, Status: "ok", SHA256: digest(before)}}
	f.e.Manifest.ComputeFingerprint()
	f.reviewer.replies = append(rv, func(provider.Request) (any, error) {
		if err := f.e.Run.WriteArtifact(rel, []byte("Remove the public API.\n")); err != nil {
			t.Fatal(err)
		}
		return finalReview("approve", nil, s1), nil
	})
	o := f.execute(t)
	if o.Status == run.StatusApproved {
		valid, _ := library.Verify(f.e.State.Publish.Path)
		t.Fatalf("changed mandatory snapshot published with old manifest digest: %+v verify=%s", o, valid)
	}
}

func TestP5ReviewRefreshKeepsExplicitAttemptCap(t *testing.T) {
	f := newFixture(t, nil, nil)
	f.e.Manifest.Version = inputs.ManifestVersion
	if err := f.e.Manifest.Save(filepath.Join(f.e.Run.Dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	f.e.State.Limits.MaxAttempts, f.e.State.Limits.Source = 7, "flag"
	f.e.deriveBudget(1) // normal first outline approval
	f.e.State.Counters.Attempts = 7
	if err := Refresh(context.Background(), f.e.Run, f.e.State, f.e.Cfg); err != nil {
		t.Fatal(err)
	}
	if got := f.e.State.Limits.MaxAttempts; got != 7 {
		t.Fatalf("refresh raised explicit --max-calls without permission: got %d, want 7", got)
	}
}

func TestP5ReviewRecoveryKeepsKnownActiveTime(t *testing.T) {
	f := newFixture(t, []reply{func(provider.Request) (any, error) {
		time.Sleep(25 * time.Millisecond)
		return research(r1), nil
	}}, nil)
	b, _ := json.Marshal(f.e.State)
	if _, o := f.e.call(context.Background(), "planner", StageResearch, "same request", schema.KindResearch, true); o != nil {
		t.Fatal(o)
	}
	spent := f.e.State.Counters.ActiveSeconds
	g := newFixture(t, nil, nil)
	g.e.Run, g.e.Manifest = f.e.Run, f.e.Manifest
	if err := json.Unmarshal(b, g.e.State); err != nil {
		t.Fatal(err)
	}
	if _, o := g.e.call(context.Background(), "planner", StageResearch, "same request", schema.KindResearch, true); o != nil {
		t.Fatal(o)
	}
	if g.e.State.Counters.ActiveSeconds < spent {
		t.Fatalf("recovered result forgot measured active time: spent=%f recovered=%f", spent, g.e.State.Counters.ActiveSeconds)
	}
}

func TestP5ReviewRecoveryFindsFinishedSuffixedCall(t *testing.T) {
	f := newFixture(t, []reply{fixed(research(r1))}, nil)
	b, _ := json.Marshal(f.e.State)
	// The first call was interrupted; the next attempt gets a new call directory.
	if err := os.MkdirAll(filepath.Join(f.e.Run.Dir, "calls", "0001-research-planner"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, o := f.e.call(context.Background(), "planner", StageResearch, "same request", schema.KindResearch, true); o != nil {
		t.Fatal(o)
	}
	if _, err := os.Stat(filepath.Join(f.e.Run.Dir, "calls", "0001-research-planner-2", "result.json")); err != nil {
		t.Fatal(err)
	}
	g := newFixture(t, []reply{fixed(research(r1))}, nil)
	g.e.Run, g.e.Manifest = f.e.Run, f.e.Manifest
	if err := json.Unmarshal(b, g.e.State); err != nil {
		t.Fatal(err)
	}
	if _, o := g.e.call(context.Background(), "planner", StageResearch, "same request", schema.KindResearch, true); o != nil {
		t.Fatal(o)
	}
	if calls(g.planner) != 0 {
		t.Fatalf("finished -2 result was ignored after second crash: extra model calls=%d", calls(g.planner))
	}
}

func TestP5ReviewRefreshDoesNotReusePreviousGenerationResult(t *testing.T) {
	f := newFixture(t, []reply{fixed(research(r1))}, nil)
	f.e.State.Cursor = run.Cursor{Stage: StageResearch}
	f.e.Manifest.Version = inputs.ManifestVersion
	f.e.Manifest.ComputeFingerprint()
	if err := f.e.Manifest.Save(filepath.Join(f.e.Run.Dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(f.e.State)
	prompt, err := f.e.prompt(StageResearch, "planner", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, o := f.e.call(context.Background(), "planner", StageResearch, prompt, schema.KindResearch, true); o != nil {
		t.Fatal(o)
	}
	stop := func(provider.Request) (any, error) {
		return nil, &provider.Error{Class: provider.ClassRateLimit, Msg: "controlled stop", Attempts: 1}
	}
	g := newFixture(t, []reply{stop}, []reply{stop})
	g.e.Run = f.e.Run
	if err := json.Unmarshal(b, g.e.State); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.e.Manifest.Repos[0].Root, "changed-source.txt"), []byte("new input after crash\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Refresh(context.Background(), g.e.Run, g.e.State, g.e.Cfg); err != nil {
		t.Fatal(err)
	}
	man, err := inputs.Load(filepath.Join(g.e.Run.Dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	g.e.Manifest = man
	if man.Fingerprint == f.e.Manifest.Fingerprint || g.e.State.Generation != 2 {
		t.Fatal("control refresh did not change the generation and inputs")
	}
	o := g.execute(t)
	if calls(g.planner) != 1 || calls(g.reviewer) != 0 {
		t.Fatalf("new generation reused old research after input drift: planner_calls=%d reviewer_calls=%d outcome=%+v\n%s", calls(g.planner), calls(g.reviewer), o, g.log.String())
	}
}

func TestP5ReviewWriteOnceDoesNotReplaceDanglingSymlink(t *testing.T) {
	d := t.TempDir()
	out, target := filepath.Join(d, "plan.md"), filepath.Join(d, "future-user-file.md")
	if err := os.Symlink(target, out); err != nil {
		t.Fatal(err)
	}
	err := writeOnce(out, []byte("new plan"))
	got, linkErr := os.Readlink(out)
	if err == nil || linkErr != nil || got != target {
		t.Fatalf("foreign symlink replaced: write error=%v readlink=%q error=%v", err, got, linkErr)
	}
}

func TestP5ReviewConcurrentPublicationDoesNotClobber(t *testing.T) {
	// Independent runs have different run locks but may choose the same --out.
	for round := 0; round < 4; round++ {
		out := filepath.Join(t.TempDir(), "plan.md")
		start := make(chan struct{})
		results := make(chan error, 4)
		var wg sync.WaitGroup
		for writer := 0; writer < 4; writer++ {
			payload := []byte(fmt.Sprintf("writer %d\n", writer) + strings.Repeat("x", 2<<20))
			wg.Add(1)
			go func() { defer wg.Done(); <-start; results <- writeOnce(out, payload) }()
		}
		close(start)
		wg.Wait()
		close(results)
		succeeded := 0
		for err := range results {
			if err == nil {
				succeeded++
			}
		}
		if succeeded != 1 {
			t.Fatalf("%d different publishers succeeded at the same path; expected exactly one", succeeded)
		}
	}
}
