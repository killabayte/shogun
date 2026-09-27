package schema

import (
	"strings"
	"testing"
)

const reqsJSON = `{"schema_version":1,"requirements":[
 {"id":"R-001","statement":"Rate limit per tenant","source_ids":["task"],"type":"functional","mandatory":true,
  "criteria":[{"id":"R-001.C1","text":"429 after N req/min"},{"id":"R-001.C2","text":"configurable N"}]},
 {"id":"R-002","statement":"No breaking API changes","source_ids":["in-1"],"type":"constraint","mandatory":true,
  "criteria":[{"id":"R-002.C1","text":"existing endpoints unchanged"}]}]}`

func TestValidateRequirementsAndRejectUnknownFields(t *testing.T) {
	if err := Validate(KindRequirements, []byte(reqsJSON)); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(reqsJSON, `"mandatory":true`, `"mandatory":true,"extra":1`, 1)
	if err := Validate(KindRequirements, []byte(bad)); err == nil {
		t.Fatal("additionalProperties must be rejected")
	}
	if err := Validate(KindRequirements, []byte(`{"schema_version":2,"requirements":[]}`)); err == nil {
		t.Fatal("wrong schema_version must be rejected")
	}
	if err := Validate(KindRequirements, []byte(`not json`)); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("invalid json: %v", err)
	}
}

func TestSemanticChecks(t *testing.T) {
	reqs, err := Parse[Requirements](KindRequirements, []byte(reqsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if p := CheckRequirements(reqs.Requirements); len(p) != 0 {
		t.Fatalf("unexpected problems: %v", p)
	}
	dup := append([]Requirement{}, reqs.Requirements...)
	dup = append(dup, Requirement{ID: "R-001", Criteria: []Criterion{{ID: "R-002.C9"}}})
	p := CheckRequirements(dup)
	if len(p) != 2 || !strings.Contains(p[0], "duplicate requirement id") || !strings.Contains(p[1], "does not belong") {
		t.Fatalf("expected dup+prefix problems, got %v", p)
	}

	outline := &Outline{SchemaVersion: 1}
	outline.Approach.Summary = "x"
	outline.Steps = []OutlineStep{
		{ID: "S-001", Title: "a", Objective: "o", Deliverable: "d", DependsOn: []string{"S-002"}, CriterionIDs: []string{"R-001.C1"}},
		{ID: "S-002", Title: "b", Objective: "o", Deliverable: "d", DependsOn: []string{"S-001"}, CriterionIDs: []string{"R-001.C2", "R-009.C1"}},
	}
	p = CheckOutline(outline, reqs.Requirements)
	joined := strings.Join(p, "\n")
	for _, want := range []string{"unknown criterion R-009.C1", "R-002.C1 is not assigned", "dependency cycle: S-001 -> S-002 -> S-001"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing problem %q in:\n%s", want, joined)
		}
	}
	outline.Steps[1].DependsOn = nil
	outline.Steps[1].CriterionIDs = []string{"R-001.C2"}
	outline.FinalVerificationCriterionIDs = []string{"R-002.C1"}
	if p := CheckOutline(outline, reqs.Requirements); len(p) != 0 {
		t.Fatalf("clean outline should pass, got %v", p)
	}

	step := &Step{ID: "S-001", CriterionIDs: []string{"R-001.C1"}, RequirementIDs: []string{"R-001"}, DependsOn: []string{"S-002"},
		Verification: []Verification{{ID: "V-001"}, {ID: "V-001"}}}
	p = CheckStep(step, outline, reqs.Requirements)
	if len(p) != 1 || !strings.Contains(p[0], "duplicate verification id") {
		t.Fatalf("expected dup verification, got %v", p)
	}
	step.CriterionIDs = nil
	if p := CheckStep(step, outline, reqs.Requirements); len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), "drops criterion R-001.C1") {
		t.Fatalf("dropped criterion must be flagged: %v", p)
	}
	// dependency set must equal the outline exactly: additions are as forbidden as removals
	exact := &Step{ID: "S-001", CriterionIDs: []string{"R-001.C1"}, RequirementIDs: []string{"R-001"}, DependsOn: []string{"S-002"}, Verification: []Verification{{ID: "V-001"}}}
	if p := CheckStep(exact, outline, reqs.Requirements); len(p) != 0 {
		t.Fatalf("exact dependencies must pass: %v", p)
	}
	outline.Steps = append(outline.Steps, OutlineStep{ID: "S-003", Title: "c", Objective: "o", Deliverable: "d"})
	added := *exact
	added.DependsOn = []string{"S-002", "S-003"}
	if p := CheckStep(&added, outline, reqs.Requirements); len(p) != 1 || !strings.Contains(p[0], "adds dependency S-003") {
		t.Fatalf("added dependency must be flagged: %v", p)
	}
	dupDep := *exact
	dupDep.DependsOn = []string{"S-002", "S-002"}
	if p := CheckStep(&dupDep, outline, reqs.Requirements); len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), "twice") {
		t.Fatalf("duplicate dependency must be flagged: %v", p)
	}
}

func TestCoverageGate(t *testing.T) {
	expected := []string{"R-001", "R-002"}
	full := func(id string) Coverage {
		return Coverage{RequirementID: id, CriterionIDs: []string{id + ".C1"}, TargetIDs: []string{"S-001"}, VerificationRefs: []string{"V-001"}, Status: "covered"}
	}
	rev := &Review{Coverage: []Coverage{full("R-001"), full("R-002")}}
	if p := CoverageGate(rev, expected); len(p) != 0 {
		t.Fatalf("full coverage should pass: %v", p)
	}
	rev.Coverage[1].Status = "partial"
	if p := CoverageGate(rev, expected); len(p) != 1 || !strings.Contains(p[0], "partial") {
		t.Fatalf("partial must block: %v", p)
	}
	rev.Coverage = rev.Coverage[:1]
	if p := CoverageGate(rev, expected); len(p) != 1 || !strings.Contains(p[0], "no coverage row") {
		t.Fatalf("missing row must block: %v", p)
	}
	rev.Coverage = []Coverage{full("R-001"), full("R-001"), full("R-777")}
	p := CoverageGate(rev, expected)
	joined := strings.Join(p, "\n")
	for _, want := range []string{"duplicate coverage row", "unknown requirement R-777", "R-002 has no coverage row"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if p := CoverageGate(&Review{}, expected); len(p) != 1 || !strings.Contains(p[0], "empty") {
		t.Fatalf("empty coverage with registry must block: %v", p)
	}
	// strict scope: all criteria of R-001 must be listed, references must be known
	reqs, _ := Parse[Requirements](KindRequirements, []byte(reqsJSON))
	scope := ScopeForRequirements(reqs.Requirements, true)
	scope.KnownTargets = map[string]bool{"S-001": true}
	scope.KnownVerifications = map[string]bool{"V-001": true}
	rev = &Review{Coverage: []Coverage{full("R-001"), full("R-002")}}
	p = CoverageGateStrict(rev, scope)
	if len(p) != 1 || !strings.Contains(p[0], "R-001.C2 is not covered") {
		t.Fatalf("missing criterion must block: %v", p)
	}
	rev.Coverage[0].CriterionIDs = []string{"R-001.C1", "R-001.C2", "R-002.C1"}
	rev.Coverage[0].TargetIDs = []string{"S-404"}
	rev.Coverage[0].VerificationRefs = []string{"V-001", "V-001"}
	p = CoverageGateStrict(rev, scope)
	joined = strings.Join(p, "\n")
	for _, want := range []string{"R-002.C1 does not belong", "unknown target S-404", "lists verification V-001 twice"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
}

// A new finding carries "id": null (strict structured output requires every key; see bundle_test).
func TestReviewSchemaAndFindingIDNullable(t *testing.T) {
	rev := `{"schema_version":1,"verdict":"revise","summary":"s",
	 "findings":[{"id":null,"severity":"major","target_id":"S-001","problem":"p","evidence":["a.go:1"],"requested_change":"fix"},
	              {"id":"F-003","severity":"minor","target_id":"S-002","problem":"p","evidence":[],"requested_change":"fix"}],
	 "dispositions":[{"finding_id":"F-001","status":"resolved","reason":"","evidence":[]}],
	 "coverage":[{"requirement_id":"R-001","criterion_ids":["R-001.C1"],"target_ids":["S-001"],"verification_refs":["V-001"],"status":"covered"}],
	 "source_assessments":[],"questions":[]}`
	r, err := Parse[Review](KindReview, []byte(rev))
	if err != nil {
		t.Fatal(err)
	}
	if !OpenBlocking(r) || r.Findings[0].ID != "" || r.Findings[1].ID != "F-003" {
		t.Fatalf("parse mismatch: %+v", r.Findings)
	}
	if err := Validate(KindReview, []byte(strings.Replace(rev, `"verdict":"revise"`, `"verdict":"maybe"`, 1))); err == nil {
		t.Fatal("bad verdict must fail")
	}
}

func TestRawSchemasAreAvailable(t *testing.T) {
	for _, k := range []Kind{KindRequirements, KindResearch, KindOutline, KindStep, KindReview, KindAnswers} {
		raw, err := Raw(k)
		if err != nil || len(raw) == 0 {
			t.Fatalf("%s: %v", k, err)
		}
	}
}

// A step must reference at least one accepted criterion; a constraint-only step (e.g. running
// compatibility checks) is legitimate. Whether a step is needed at all is judged by the reviewer.
func TestOutlineStepMustReferenceACriterion(t *testing.T) {
	reqs, err := Parse[Requirements](KindRequirements, []byte(reqsJSON))
	if err != nil {
		t.Fatal(err)
	}
	outline := &Outline{SchemaVersion: 1}
	outline.Steps = []OutlineStep{
		{ID: "S-001", CriterionIDs: []string{"R-001.C1", "R-001.C2"}},
		{ID: "S-002", CriterionIDs: nil},
		{ID: "S-003", CriterionIDs: []string{"R-002.C1"}},
	}
	joined := strings.Join(CheckOutline(outline, reqs.Requirements), "\n")
	if joined != "S-002 references no criterion" {
		t.Fatalf("want only the empty step rejected, got:\n%s", joined)
	}
}
