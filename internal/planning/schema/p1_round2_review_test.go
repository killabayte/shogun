package schema

import (
	"encoding/json"
	"testing"
)

func TestP1Round2UnapprovedDependencyAdditions(t *testing.T) {
	reqs := []Requirement{{ID: "R-001", Mandatory: true, Criteria: []Criterion{{ID: "R-001.C1"}}}}
	outline := &Outline{Steps: []OutlineStep{{ID: "S-001", CriterionIDs: []string{"R-001.C1"}}, {ID: "S-002", CriterionIDs: []string{"R-001.C1"}}}}
	for _, pair := range [][2]string{{"S-001", "S-002"}, {"S-002", "S-001"}} {
		t.Run(pair[0], func(t *testing.T) {
			s := Step{ID: pair[0], Title: "step", Objective: "objective", RequirementIDs: []string{"R-001"}, CriterionIDs: []string{"R-001.C1"}, DependsOn: []string{pair[1]}, Targets: []Target{{RepoID: "repo-1", Path: "a.go", Operation: "modify"}}, Actions: []string{"do it"}, Verification: []Verification{{ID: "V-001", RepoID: "repo-1", Method: "test", Expected: "pass"}}, Risks: []Risk{}, RollbackOrWhyNotApplicable: "revert"}
			batch := StepBatch{SchemaVersion: 1, Steps: []Step{s}, Questions: []Question{}, RequestedChanges: []RequestedChange{}, ResponsesToFindings: []ResponseToFinding{}}
			raw, err := json.Marshal(batch)
			if err != nil {
				t.Fatal(err)
			}
			if err = Validate(KindStep, raw); err != nil {
				t.Fatal(err)
			}
			if p := CheckStep(&s, outline, reqs); len(p) == 0 {
				t.Fatalf("unapproved dependency %s -> %s accepted; accepting both creates a cycle", pair[0], pair[1])
			}
		})
	}
}
