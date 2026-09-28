package pipeline

import (
	"encoding/json"
	"testing"

	"github.com/killabayte/shogun/internal/inputs"
	"github.com/killabayte/shogun/internal/run"
)

// fastWithContradictedInput: the plan studies explicit input in-1; the approving reviewer marks it
// contradicted.
func fastWithContradictedInput(t *testing.T) *fixture {
	d := planDoc([]req{r1}, s1)
	d["source_coverage"] = append(d["source_coverage"].([]any), map[string]any{"source_id": "in-1", "studied": "draft.md", "relevance": "relevant", "notes": "earlier draft"})
	rev := finalReview("approve", nil, s1)
	rev["source_assessments"] = []any{map[string]any{"source_id": "in-1", "role": "explicit", "verdict": "contradicted", "note": "the draft drops fields the task keeps"}}
	f := newFast(t, []reply{fixed(d)}, []reply{fixed(rev)})
	if err := f.e.Run.WriteArtifact("inputs/draft.md", []byte("draft\n")); err != nil {
		t.Fatal(err)
	}
	f.e.Manifest.Inputs = []inputs.Source{{ID: "in-1", Origin: "draft.md", StoredPath: "inputs/draft.md", Status: "ok"}}
	return f
}

// An explicit source the reviewer does not confirm is the user's call: one blocking question right
// after the review, no second planner call.
func TestContradictedExplicitSourceAsksTheUser(t *testing.T) {
	f := fastWithContradictedInput(t)
	o := f.execute(t)
	p := f.e.State.Progress.Pending
	if o.Status != run.StatusNeedsInput || f.e.State.Counters.LogicalCalls != 2 || len(p) != 1 || p[0].Origin != "shogun" || !p[0].Blocking {
		t.Fatalf("%+v calls %d pending %+v\n%s", o, f.e.State.Counters.LogicalCalls, p, f.log.String())
	}
}

// An assumption is not the user's basis: the question stays open.
func TestContradictedExplicitSourceIsNotResolvedByAnAssumption(t *testing.T) {
	f := fastWithContradictedInput(t)
	q := f.e.sourceQuestion("in-1")
	b, _ := json.Marshal([]Decision{{ID: "Q-001", Stage: StagePlan, Origin: "shogun", Question: q.Question, Answer: "follow the task", Source: "assumption"}})
	if err := f.e.Run.WriteArtifact("decisions.json", b); err != nil {
		t.Fatal(err)
	}
	if o := f.execute(t); o.Status != run.StatusNeedsInput {
		t.Fatalf("an assumption resolved the contradiction: %+v", o)
	}
}

// The user's recorded answer resolves it: the approval stands without asking again.
func TestContradictedExplicitSourceResolvedByTheUser(t *testing.T) {
	f := fastWithContradictedInput(t)
	q := f.e.sourceQuestion("in-1")
	b, _ := json.Marshal([]Decision{{ID: "Q-001", Stage: StagePlan, Origin: "shogun", Question: q.Question, Answer: "follow the task", Source: "user"}})
	if err := f.e.Run.WriteArtifact("decisions.json", b); err != nil {
		t.Fatal(err)
	}
	if o := f.execute(t); o.Status != run.StatusApproved || f.e.State.Counters.LogicalCalls != 2 {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
}
