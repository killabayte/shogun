package pipeline

import (
	"encoding/json"
	"strings"
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
	b, _ := json.Marshal([]Decision{{ID: "Q-001", Stage: StagePlan, Origin: "shogun", Question: q.Question, Answer: q.Options[0], Source: "assumption"}})
	if err := f.e.Run.WriteArtifact("decisions.json", b); err != nil {
		t.Fatal(err)
	}
	if o := f.execute(t); o.Status != run.StatusNeedsInput {
		t.Fatalf("an assumption resolved the contradiction: %+v", o)
	}
}

// The user's choice "reference" for this snapshot resolves it: the approval stands, not asked again.
func TestContradictedExplicitSourceResolvedByTheUser(t *testing.T) {
	f := fastWithContradictedInput(t)
	q := f.e.sourceQuestion("in-1")
	b, _ := json.Marshal([]Decision{{ID: "Q-001", Stage: StagePlan, Origin: "shogun", Question: q.Question, Answer: q.Options[0], Source: "user"}})
	if err := f.e.Run.WriteArtifact("decisions.json", b); err != nil {
		t.Fatal(err)
	}
	if o := f.execute(t); o.Status != run.StatusApproved || f.e.State.Counters.LogicalCalls != 2 {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
}

func decide(t *testing.T, f *fixture, answer string) {
	t.Helper()
	q := f.e.sourceQuestion("in-1")
	b, _ := json.Marshal([]Decision{{ID: "Q-001", Stage: StagePlan, Origin: "shogun", Question: q.Question, Answer: answer, Source: "answers-file"}})
	if err := f.e.Run.WriteArtifact("decisions.json", b); err != nil {
		t.Fatal(err)
	}
}

// "authoritative" keeps the source binding: the verdict is a problem for the planner, never a waiver.
func TestContradictedExplicitSourceKeptAuthoritativeBlocks(t *testing.T) {
	f := fastWithContradictedInput(t)
	decide(t, f, "2")
	f.planner.replies = append(f.planner.replies, f.planner.replies[0])
	f.reviewer.replies = append(f.reviewer.replies, f.reviewer.replies[0])
	o := f.execute(t)
	notes := strings.Join(f.e.State.Progress.GateNotes, "; ")
	if o.Status == run.StatusApproved || len(f.e.State.Progress.Pending) != 0 || !strings.Contains(notes, "authoritative by the user's decision") {
		t.Fatalf("%+v pending %+v notes %s", o, f.e.State.Progress.Pending, notes)
	}
}

// An answer that is not one of the options resolves nothing: the question is asked again.
func TestContradictedExplicitSourceFreeTextIsAskedAgain(t *testing.T) {
	f := fastWithContradictedInput(t)
	decide(t, f, "follow the task for the damaged-run shape only")
	if o := f.execute(t); o.Status != run.StatusNeedsInput || len(f.e.State.Progress.Pending) != 1 {
		t.Fatalf("%+v pending %+v", o, f.e.State.Progress.Pending)
	}
}

// A choice holds for the snapshot it was made on: changed bytes are asked about again.
func TestContradictedExplicitSourceChoiceIsPerSnapshot(t *testing.T) {
	f := fastWithContradictedInput(t)
	decide(t, f, "1")
	changed := []byte("draft, edited\n")
	if err := f.e.Run.WriteArtifact("inputs/draft.md", changed); err != nil {
		t.Fatal(err)
	}
	f.e.Manifest.Inputs[0].SHA256 = digest(changed)
	if o := f.execute(t); o.Status != run.StatusNeedsInput {
		t.Fatalf("a choice about other bytes resolved the new snapshot: %+v", o)
	}
}

// --reference says it up front: no question, the approval stands, and both models are told.
func TestReferenceInputDoesNotAsk(t *testing.T) {
	f := fastWithContradictedInput(t)
	f.e.Manifest.Inputs[0].Role = inputs.RoleReference
	if o := f.execute(t); o.Status != run.StatusApproved || f.e.State.Counters.LogicalCalls != 2 {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
	if !strings.Contains(f.planner.prompts[0], "reference: material to correct; where it differs from the task, the task governs") {
		t.Fatal("the planner was not told that in-1 is reference material")
	}
}
