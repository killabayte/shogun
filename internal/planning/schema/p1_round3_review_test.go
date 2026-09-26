package schema

import (
	"encoding/json"
	"testing"
)

func TestP1Round3NecessaryConstraintStep(t *testing.T) {
	const registry = `{"schema_version":1,"requirements":[
	 {"id":"R-001","statement":"Add CSV export","source_ids":["task"],"type":"functional","mandatory":true,
	  "criteria":[{"id":"R-001.C1","text":"CSV export returns the requested rows"}]},
	 {"id":"R-002","statement":"Preserve the existing public API","source_ids":["task"],"type":"constraint","mandatory":true,
	  "criteria":[{"id":"R-002.C1","text":"Existing public API contract tests remain green"}]}]}`
	const outlineJSON = `{"schema_version":1,"approach":{"summary":"Add export and verify compatibility","alternatives":[]},
	 "steps":[
	  {"id":"S-001","title":"CSV export","objective":"Export requested rows","deliverable":"Working CSV export",
	   "depends_on":[],"criterion_ids":["R-001.C1"]},
	  {"id":"S-002","title":"Compatibility verification","objective":"Check the required API constraint","deliverable":"Existing API contract test results",
	   "depends_on":["S-001"],"criterion_ids":["R-002.C1"]}],
	 "final_verification_criterion_ids":[],"questions":[],"requested_changes":[],"responses_to_findings":[]}`
	reqs, err := Parse[Requirements](KindRequirements, []byte(registry))
	if err != nil {
		t.Fatal(err)
	}
	if p := CheckRequirements(reqs.Requirements); len(p) != 0 {
		t.Fatal(p)
	}
	base, err := Parse[Outline](KindOutline, []byte(outlineJSON))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("mandatory_constraint_check", func(t *testing.T) {
		if p := CheckOutline(base, reqs.Requirements); len(p) != 0 {
			t.Fatalf("required compatibility verification must reach reviewer without a fabricated functional reference: %v", p)
		}
	})
	t.Run("functional_support_step", func(t *testing.T) {
		copy := *base
		copy.Steps = append([]OutlineStep(nil), base.Steps...)
		copy.Steps = append(copy.Steps, OutlineStep{ID: "S-003", Title: "Fixture setup", Objective: "Prepare input for export verification", Deliverable: "CSV fixtures", DependsOn: []string{}, CriterionIDs: []string{"R-001.C1"}})
		// Keep the constraint in final verification so this control isolates the setup step.
		copy.Steps = append(copy.Steps[:1], copy.Steps[2:]...)
		copy.FinalVerificationCriterionIDs = []string{"R-002.C1"}
		raw, err := json.Marshal(copy)
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(KindOutline, raw); err != nil {
			t.Fatal(err)
		}
		if p := CheckOutline(&copy, reqs.Requirements); len(p) != 0 {
			t.Fatalf("necessary setup must pass: %v", p)
		}
	})
}
