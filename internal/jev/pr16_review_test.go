package jev

import (
	"context"
	"strings"
	"testing"
)

// Scrubbing after truncation misses a key that crosses the excerpt boundary.
func TestPR16ReviewRedactsKeyBeforeTruncatingBody(t *testing.T) {
	c := pr14Client(401, "")
	c = pr14Client(401, strings.Repeat("x", 180)+c.Key)
	_, err := c.Ask(context.Background(), "s", map[string]Question{"q": Noul("Is this s?")})
	if err == nil {
		t.Fatal("expected an auth error")
	}
	if strings.Contains(err.Error(), c.Key[:16]) {
		t.Fatal("a truncated HTTP body exposed more than the public prefix of the fake key")
	}
}

// Presence of the usage object is not presence of its mandatory input counter.
// A missing/null counter must not become a known zero-cost request.
func TestPR16ReviewRejectsMissingInputUsage(t *testing.T) {
	for _, usage := range []string{`{}`, `{"output_tokens":3}`, `{"input_tokens":null,"output_tokens":3}`} {
		body := `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.9}},"usage":` + usage + `}`
		c := pr14Client(200, body)
		res, err := c.Ask(context.Background(), "s", map[string]Question{"q": Noul("Is this s?")})
		if err == nil {
			t.Errorf("usage %s became a successful known-zero request: %+v", usage, res.Usage)
		}
	}
}

// All offered options/levels and required answer fields must be returned.
func TestPR16ReviewRejectsIncompleteAnswerFields(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		question     Question
	}{
		{"choice", `{"type":"choice","choice":"yes","probabilities":{"yes":1},"confidence":1}`, Choice("Choose", map[string]string{"yes": "yes", "no": "no"})},
		{"score", `{"type":"score","score":0,"probabilities":{"0":1},"confidence":1,"legend":{"0":"bad","1":"good"}}`, Score("Rate", []string{"bad", "good"})},
		{"score_without_legend", `{"type":"score","score":0,"probabilities":{"0":1,"1":0},"confidence":1}`, Score("Rate", []string{"bad", "good"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := pr14Client(200, `{"model":"jev-1.13.0","answers":{"q":`+tc.answer+`},"usage":{"input_tokens":10,"output_tokens":1}}`)
			if _, err := c.Ask(context.Background(), "s", map[string]Question{"q": tc.question}); err == nil {
				t.Fatal("an answer with missing required probability/legend fields was accepted")
			}
		})
	}
}
