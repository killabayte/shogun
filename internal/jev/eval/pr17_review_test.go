package eval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/killabayte/shogun/internal/jev"
)

func TestPR17ReviewOutgoingContainsChoiceCriteria(t *testing.T) {
	var out strings.Builder
	if _, err := Outgoing(&out, Cases, nil); err != nil {
		t.Fatal(err)
	}
	_, items, err := Load(Cases[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID != "R-001" {
			continue
		}
		opts := it.Questions["origin"].Criteria.(map[string]string)
		key := "task_necessary_consequence"
		if !strings.Contains(out.String(), key) || !strings.Contains(out.String(), opts[key]) {
			t.Fatal("the outgoing audit omits option names/descriptions that the request sends")
		}
	}
}

func TestPR17ReviewOutgoingAndAskUseTheSamePolicy(t *testing.T) {
	deny, err := jev.CompileDeny([]string{`"task":"Add a`})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	refused, err := Outgoing(&out, Cases[:1], deny)
	if err != nil {
		t.Fatal(err)
	}
	c := pr15PerfectClient(t) // in-memory transport only
	c.Deny = deny
	_, items, err := Load(Cases[0])
	if err != nil {
		t.Fatal(err)
	}
	actual := 0
	for _, it := range items {
		_, err := c.Ask(context.Background(), it.State, it.Questions)
		var e *jev.Error
		if errors.As(err, &e) && e.Class == jev.ClassSensitive {
			actual++
		}
	}
	if refused != actual {
		t.Fatalf("dry-run verdict differs from real Ask: dump refuses %d, Ask refuses %d", refused, actual)
	}
}
