package schema

import (
	"encoding/json"
	"testing"
)

func TestP1ReviewCoveredRowWithoutEvidence(t *testing.T) {
	data := []byte(`{"schema_version":1,"verdict":"approve","summary":"ok","findings":[],"dispositions":[],"coverage":[{"requirement_id":"R-001","criterion_ids":[],"target_ids":[],"verification_refs":[],"status":"covered"}],"source_assessments":[],"questions":[]}`)
	r, err := Parse[Review](KindReview, data)
	if err != nil {
		return
	}
	if p := CoverageGate(r, []string{"R-001"}); len(p) == 0 {
		t.Fatal("empty criteria/targets/verifications accepted as full coverage")
	}
}
func TestP1ReviewStepDependencies(t *testing.T) {
	req := []Requirement{{ID: "R-001", Mandatory: true, Criteria: []Criterion{{ID: "R-001.C1"}}}}
	o := &Outline{Steps: []OutlineStep{{ID: "S-001", CriterionIDs: []string{"R-001.C1"}, DependsOn: []string{"S-002"}}, {ID: "S-002"}}}
	for name, deps := range map[string][]string{"dropped": {}, "unknown": {"S-999"}, "self": {"S-001"}} {
		t.Run(name, func(t *testing.T) {
			s := &Step{ID: "S-001", Title: "Step", Objective: "Objective", RequirementIDs: []string{"R-001"}, CriterionIDs: []string{"R-001.C1"}, DependsOn: deps,
				Targets: []Target{{RepoID: "repo-1", Path: "a.go", Operation: "modify"}}, Actions: []string{"Change a.go"},
				Verification: []Verification{{ID: "V-001", RepoID: "repo-1", Method: "test", Expected: "tests pass"}}, Risks: []Risk{}, RollbackOrWhyNotApplicable: "revert change"}
			batch := StepBatch{SchemaVersion: 1, Steps: []Step{*s}, Questions: []Question{}, RequestedChanges: []RequestedChange{}, ResponsesToFindings: []ResponseToFinding{}}
			data, err := json.Marshal(batch)
			if err != nil {
				t.Fatal(err)
			}
			if err = Validate(KindStep, data); err != nil {
				t.Fatalf("repro must be schema-valid: %v", err)
			}
			if p := CheckStep(s, o, req); len(p) == 0 {
				t.Fatalf("invalid detail dependencies accepted: %v", deps)
			}
		})
	}
}
