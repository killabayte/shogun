package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestP1Round2TaggedMapIsNotScalar(t *testing.T) {
	src := strings.Replace(samplePlan, "revision: 1\n", "revision: 1\nretry_count: 1\n", 1)
	p := filepath.Join(t.TempDir(), "plan.md")
	os.WriteFile(p, []byte(src), 0600)
	doc, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	writeReceipt(t, p, doc)
	if got, _ := Verify(p); got != Valid {
		t.Fatal("baseline", got)
	}
	changed := strings.Replace(src, "retry_count: 1\n", "retry_count: {__int__: \"1\"}\n", 1)
	os.WriteFile(p, []byte(changed), 0600)
	if got, note := Verify(p); got == Valid {
		t.Fatalf("scalar changed to map but verify=valid: %s", note)
	}
}
func TestP1Round2OverflowingYAMLInteger(t *testing.T) {
	src := strings.Replace(samplePlan, "revision: 1\n", "revision: 1\nexternal_id: 18446744073709551616\n", 1)
	p := filepath.Join(t.TempDir(), "plan.md")
	doc, err := Parse([]byte(src))
	if err != nil {
		return
	}
	os.WriteFile(p, []byte(src), 0600)
	writeReceipt(t, p, doc)
	if got, _ := Verify(p); got != Valid {
		t.Fatal("baseline", got)
	}
	os.WriteFile(p, []byte(strings.Replace(src, "18446744073709551616", "18446744073709551617", 1)), 0600)
	if got, note := Verify(p); got == Valid {
		t.Fatalf("overflowing YAML integer change ignored: %s", note)
	}
}
