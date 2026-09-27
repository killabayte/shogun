package pipeline

import (
	"encoding/json"
	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/run"
	"testing"
)

func TestGateReviewFinalOnlyCriterionStillRequiresFinalEvidence(t *testing.T) {
	requirement := req{"R-001", "functional", true, []string{"R-001.C1", "R-001.C2"}}
	doc := planDoc([]req{requirement}, s1)
	doc["final_verification_criterion_ids"] = []string{"R-001.C2"}
	f := newFast(t, nil, nil)
	b, _ := json.Marshal(doc)
	plan, problems, err := f.e.checkPlan(b)
	if err != nil || len(problems) > 0 {
		t.Fatalf("bad fixture: %v %v", err, problems)
	}
	d := &planData{research: planResearch(plan), outline: planOutline(plan), steps: plan.Steps}
	revDoc := finalReview("approve", nil, s1)
	row := revDoc["coverage"].([]any)[0].(map[string]any)
	row["criterion_ids"] = requirement.crit
	parse := func() *schema.Review {
		b, _ := json.Marshal(revDoc)
		r, err := schema.Parse[schema.Review](schema.KindReview, b)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if got := f.e.integrationGate(d, parse()); len(got) == 0 {
		t.Fatal("accepted a final-only criterion with step-only evidence")
	}
	row["target_ids"] = []string{"S-001", "final"}
	row["verification_refs"] = []string{"S-001/V-001", "final"}
	if got := f.e.integrationGate(d, parse()); len(got) != 0 {
		t.Fatalf("rejected complete evidence: %v", got)
	}
}

func TestGateReviewBogusDispositionCannotCloseRealMajor(t *testing.T) {
	p := run.Progress{NextFinding: 1, Ledger: []run.Finding{{ID: "F-001", Stage: StagePlan, TargetID: "S-001", Severity: "major", Status: "open", Problem: "required criterion missing"}}}
	doc := finalReview("approve", nil, s1)
	doc["dispositions"] = []any{disposition("F-999", "resolved")}
	b, _ := json.Marshal(doc)
	rev, err := schema.Parse[schema.Review](schema.KindReview, b)
	if err != nil {
		t.Fatal(err)
	}
	notes := applyReview(&p, StagePlan, "round 2", rev, blocking(relevant(&p, StagePlan)))
	if len(notes) == 0 || len(blocking(relevant(&p, StagePlan))) != 1 || p.Ledger[0].Status != "open" {
		t.Fatalf("bogus disposition removed real blocker: notes=%v ledger=%+v", notes, p.Ledger)
	}
}
