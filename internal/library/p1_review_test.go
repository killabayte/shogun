package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestP1ReviewImmutableIntegerChange(t *testing.T) {
	src := strings.Replace(samplePlan, "revision: 1\n", "revision: 1\nsource_epoch_ns: 9007199254740992\n", 1)
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
	os.WriteFile(p, []byte(strings.Replace(src, "9007199254740992", "9007199254740993", 1)), 0600)
	if got, note := Verify(p); got == Valid {
		t.Fatalf("immutable metadata value changed but verify says valid: %s", note)
	}
}
func TestP1ReviewRevisionTypeChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plan.md")
	os.WriteFile(p, []byte(samplePlan), 0600)
	doc, _ := Parse([]byte(samplePlan))
	writeReceipt(t, p, doc)
	os.WriteFile(p, []byte(strings.Replace(samplePlan, "revision: 1\n", "revision: 1.0\n", 1)), 0600)
	if got, note := Verify(p); got == Valid {
		t.Fatalf("invalid revision type accepted: %s", note)
	}
}
