package library

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestP1Round3FloatMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		want                Integrity
	}{
		{"equivalent_fraction", "0.5", ".5", Valid},
		{"changed_fraction", ".5", "5.0", Changed},
		{"changed_negative_fraction", "-.5", "-5.0", Changed},
		{"changed_positive_fraction", "+.5", "+5.0", Changed},
		{"int_to_explicit_float", "1", "!!float 1", Changed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := func(value string) string {
				return strings.Replace(samplePlan, "revision: 1\n", fmt.Sprintf("revision: 1\nratio: %s\n", value), 1)
			}
			path := filepath.Join(t.TempDir(), "plan.md")
			before := plan(tc.before)
			doc, err := Parse([]byte(before))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(before), 0600); err != nil {
				t.Fatal(err)
			}
			writeReceipt(t, path, doc)
			if got, note := Verify(path); got != Valid {
				t.Fatalf("baseline: %s (%s)", got, note)
			}
			after := plan(tc.after)
			if _, err := Parse([]byte(after)); err != nil {
				t.Fatalf("valid YAML was rejected: %v", err)
			}
			if err := os.WriteFile(path, []byte(after), 0600); err != nil {
				t.Fatal(err)
			}
			if got, note := Verify(path); got != tc.want {
				t.Fatalf("ratio %s -> %s: got %s, want %s (%s)", tc.before, tc.after, got, tc.want, note)
			}
		})
	}
}
