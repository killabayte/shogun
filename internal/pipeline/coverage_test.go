package pipeline

import (
	"strings"
	"testing"

	"github.com/killabayte/shogun/internal/planning/schema"
	"github.com/killabayte/shogun/internal/run"
)

func covRow(req string, crit, targets, refs []string) *schema.Review {
	return &schema.Review{Coverage: []schema.Coverage{{RequirementID: req, CriterionIDs: crit, TargetIDs: targets, VerificationRefs: refs, Status: "covered"}}}
}

func provided(pairs ...string) criterionProvers {
	p := criterionProvers{}
	for i := 0; i < len(pairs); i += 2 {
		p.add(pairs[i], pairs[i+1])
	}
	return p
}

// Live 2026-09-27 (stats --json self-run): S-001 implements and S-002 tests the same four criteria;
// the approving reviewer cited only S-002's tests. Each criterion is proved, so the gate passes.
func TestCoverageProofIsPerCriterionNotPerStep(t *testing.T) {
	crit := []string{"R-001.C1", "R-001.C2", "R-001.C3", "R-001.C4"}
	p := criterionProvers{}
	for _, c := range crit {
		p.add(c, "S-001")
		p.add(c, "S-002")
	}
	if notes := associations(covRow("R-001", crit, []string{"S-001", "S-002"}, []string{"S-002/V-003", "S-002/V-004"}), p); len(notes) != 0 {
		t.Fatalf("a proved requirement was blocked: %v", notes)
	}
}

// Proof of one step does not cover a criterion only another step carries.
func TestCoverageUnprovedCriterionBlocks(t *testing.T) {
	p := provided("R-001.C1", "S-001", "R-001.C2", "S-002")
	notes := associations(covRow("R-001", []string{"R-001.C1", "R-001.C2"}, []string{"S-001", "S-002"}, []string{"S-002/V-001"}), p)
	if len(notes) != 1 || !strings.Contains(notes[0], "no cited verification proves R-001.C1 (it can be proved by S-001)") {
		t.Fatalf("notes %v", notes)
	}
}

// A verification of a step that carries none of the requirement's criteria, or of a step that is not
// a target of the row, is not evidence.
func TestCoverageForeignVerificationBlocks(t *testing.T) {
	p := provided("R-001.C1", "S-001", "R-002.C1", "S-002")
	notes := associations(covRow("R-001", []string{"R-001.C1"}, []string{"S-001"}, []string{"S-002/V-001"}), p)
	if len(notes) != 2 || !strings.Contains(notes[0], "of something that carries none of its criteria") || !strings.Contains(notes[1], "no cited verification proves R-001.C1") {
		t.Fatalf("foreign verification: %v", notes)
	}
	p = provided("R-001.C1", "S-001", "R-001.C1", "S-002")
	notes = associations(covRow("R-001", []string{"R-001.C1"}, []string{"S-001"}, []string{"S-002/V-001"}), p)
	if len(notes) != 1 || !strings.Contains(notes[0], "S-002 is not a target of the row") {
		t.Fatalf("verification outside the targets: %v", notes)
	}
}

// A criterion that only the end-to-end check carries must cite "final".
func TestCoverageFinalOnlyCriterionNeedsFinal(t *testing.T) {
	p := provided("R-001.C1", "S-001", "R-001.C2", "final")
	crit := []string{"R-001.C1", "R-001.C2"}
	notes := associations(covRow("R-001", crit, []string{"S-001"}, []string{"S-001/V-001"}), p)
	if len(notes) != 1 || !strings.Contains(notes[0], "no cited verification proves R-001.C2 (it can be proved by final)") {
		t.Fatalf("notes %v", notes)
	}
	if notes := associations(covRow("R-001", crit, []string{"S-001", "final"}, []string{"S-001/V-001", "final"}), p); len(notes) != 0 {
		t.Fatalf("final proof rejected: %v", notes)
	}
}

// End to end: two steps carry the criterion, the approving review cites only the second — approved
// in one round.
func TestFastPathApprovesWhenOneCarrierProvesTheCriterion(t *testing.T) {
	s2 := step{"S-002", []string{"R-001.C1"}}
	rev := finalReview("approve", nil, s1)
	row := rev["coverage"].([]any)[0].(map[string]any)
	row["target_ids"], row["verification_refs"] = []string{"S-001", "S-002"}, []string{"S-002/V-001"}
	f := newFast(t, []reply{fixed(planDoc([]req{r1}, s1, s2))}, []reply{fixed(rev)})
	if o := f.execute(t); o.Status != run.StatusApproved || f.e.State.Counters.LogicalCalls != 2 {
		t.Fatalf("%+v\n%s", o, f.log.String())
	}
}
