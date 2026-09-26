package schema

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The bundle is standalone (no cross-file refs) and accepts/rejects the same documents as Validate.
func TestBundleIsSelfContainedAndEquivalent(t *testing.T) {
	outline := `{"schema_version":1,"approach":{"summary":"s","alternatives":[]},"steps":[{"id":"S-001","title":"t","objective":"o","deliverable":"d","depends_on":[],"criterion_ids":["R-001.C1"]}],"final_verification_criterion_ids":[],"questions":[],"requested_changes":[],"responses_to_findings":[]}`
	docs := map[Kind][]string{
		KindRequirements: {reqsJSON, strings.Replace(reqsJSON, `"R-001"`, `"X-1"`, 1)},
		KindOutline:      {outline, strings.Replace(outline, `"S-001"`, `"S-1"`, 1), strings.Replace(outline, `"criterion_ids":["R-001.C1"]`, `"criterion_ids":[]`, 1)},
		KindReview:       {`{"schema_version":1,"verdict":"approve","summary":"ok","findings":[],"dispositions":[],"coverage":[],"source_assessments":[],"questions":[]}`, `{"schema_version":1,"verdict":"maybe"}`},
	}
	for _, k := range []Kind{KindRequirements, KindResearch, KindOutline, KindStep, KindReview, KindAnswers} {
		raw, err := Bundle(k)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if bytes.Contains(raw, []byte(".schema.json")) {
			t.Fatalf("%s: bundle still references another file: %s", k, raw)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("bundle.json", doc); err != nil {
			t.Fatal(err)
		}
		s, err := c.Compile("bundle.json")
		if err != nil {
			t.Fatalf("%s: bundle does not compile standalone: %v", k, err)
		}
		for _, d := range docs[k] {
			inst, _ := jsonschema.UnmarshalJSON(strings.NewReader(d))
			if (s.Validate(inst) == nil) != (Validate(k, []byte(d)) == nil) {
				t.Errorf("%s: bundle and Validate disagree on %s", k, d)
			}
		}
	}
}

// Codex --output-schema is OpenAI strict structured output (verified live 2026-09-26: a schema with
// an optional key is rejected with invalid_json_schema before the model runs). Every object in a
// model-produced schema must list all its properties as required and forbid additional ones.
func TestModelSchemasAreStrict(t *testing.T) {
	for _, k := range ModelKinds {
		raw, err := Bundle(k)
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		var walk func(n any, path string)
		walk = func(n any, path string) {
			switch v := n.(type) {
			case map[string]any:
				_, hasConst := v["const"]
				_, hasEnum := v["enum"]
				if _, typed := v["type"]; (hasConst || hasEnum) && !typed {
					t.Errorf("%s %s: const/enum needs an explicit type", k, path)
				}
				if props, ok := v["properties"].(map[string]any); ok {
					req := map[string]bool{}
					list, _ := v["required"].([]any)
					for _, r := range list {
						req[r.(string)] = true
					}
					for p := range props {
						if !req[p] {
							t.Errorf("%s %s: property %q is not required", k, path, p)
						}
					}
					if v["additionalProperties"] != false {
						t.Errorf("%s %s: additionalProperties must be false", k, path)
					}
				}
				for key, c := range v {
					walk(c, path+"/"+key)
				}
			case []any:
				for _, c := range v {
					walk(c, path+"[]")
				}
			}
		}
		walk(doc, "")
	}
}
