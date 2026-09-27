package library

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestP1Round4RejectInvalidTypedScalars(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
	}{
		{"float", "title: Rate limiting", "title: !!float Rate limiting"},
		{"int", "title: Rate limiting", "title: !!int Rate limiting"},
		{"timestamp", "title: Rate limiting", "title: !!timestamp Rate limiting"},
		{"null", "extra: null", "extra: !!null not-null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseline := strings.Replace(samplePlan, "revision: 1\n", "revision: 1\nextra: null\n", 1)
			doc, err := Parse([]byte(baseline))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "plan.md")
			if err := os.WriteFile(path, []byte(baseline), 0600); err != nil {
				t.Fatal(err)
			}
			writeReceipt(t, path, doc)
			if got, note := Verify(path); got != Valid {
				t.Fatalf("baseline: %s (%s)", got, note)
			}
			// A control using the underlying YAML decoder proves the explicit tag/value
			// combination is invalid, rather than just a different spelling of a value.
			var control map[string]any
			if err := yaml.Unmarshal([]byte(tc.after+"\n"), &control); err == nil {
				t.Fatal("test input must be rejected by the standard typed YAML decoder")
			}
			changed := strings.Replace(baseline, tc.before, tc.after, 1)
			if _, err := Parse([]byte(changed)); !errors.Is(err, ErrInvalidFormat) {
				t.Errorf("Parse accepted invalid %s tag/value; got %v, want ErrInvalidFormat", tc.name, err)
			}
			if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
				t.Fatal(err)
			}
			if got, note := Verify(path); got != InvalidFormat {
				t.Errorf("%s: got %s, want invalid_format (%s)", tc.after, got, note)
			}
		})
	}
}
