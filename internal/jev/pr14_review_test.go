package jev

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type pr14Transport func(*http.Request) (*http.Response, error)

func (f pr14Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// pr14Key is assembled at run time so that no literal in the repository looks like a key.
var pr14Key = "apik" + "-review-only-" + strings.Repeat("f", 12)

func pr14Client(status int, body string) *Client {
	c := New(pr14Key)
	c.HTTP = &http.Client{Transport: pr14Transport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	return c
}

func TestPR14ReviewRedactsHTTPErrorBodies(t *testing.T) {
	key := pr14Key
	for _, body := range []string{
		`{"error":{"type":"authentication_error","message":"invalid key ` + key + `"}}`,
		`proxy rejected Bearer ` + key,
	} {
		c := pr14Client(401, body)
		_, err := c.Ask(context.Background(), "s", map[string]Question{"q": Noul("Is this s?")})
		if err == nil {
			t.Fatal("expected auth error")
		}
		if strings.Contains(err.Error(), key) {
			t.Error("HTTP error exposed the fake API key")
		}
	}
}

func TestPR14ReviewRejectsMalformedResults(t *testing.T) {
	choice := map[string]Question{"q": Choice("Choose", map[string]string{"yes": "yes", "no": "no"})}
	noul := map[string]Question{"q": Noul("Is this s?")}
	score := map[string]Question{"q": Score("Rate", []string{"bad", "good"})}
	for _, tc := range []struct {
		name, body string
		questions  map[string]Question
	}{
		{"probabilities_out_of_range", `{"model":"jev-1.13.0","answers":{"q":{"type":"choice","choice":"yes","probabilities":{"yes":2,"no":-1},"confidence":1}},"usage":{"input_tokens":10,"output_tokens":0}}`, choice},
		{"missing_selected_probability", `{"model":"jev-1.13.0","answers":{"q":{"type":"choice","choice":"yes","probabilities":{"no":1},"confidence":1}},"usage":{"input_tokens":10,"output_tokens":0}}`, choice},
		{"unknown_probability_option", `{"model":"jev-1.13.0","answers":{"q":{"type":"choice","choice":"yes","probabilities":{"yes":0.9,"alien":0.1},"confidence":1}},"usage":{"input_tokens":10,"output_tokens":0}}`, choice},
		{"distribution_not_normalized", `{"model":"jev-1.13.0","answers":{"q":{"type":"choice","choice":"yes","probabilities":{"yes":0.9,"no":0.9},"confidence":1}},"usage":{"input_tokens":10,"output_tokens":0}}`, choice},
		{"score_unknown_level", `{"model":"jev-1.13.0","answers":{"q":{"type":"score","score":0.5,"probabilities":{"9":1},"confidence":1,"legend":{"9":"other"}}},"usage":{"input_tokens":10,"output_tokens":0}}`, score},
		{"different_pinned_model", `{"model":"different-model","answers":{"q":{"type":"noul","noul":0.9}},"usage":{"input_tokens":10,"output_tokens":0}}`, noul},
		{"missing_usage", `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.9}}}`, noul},
		{"negative_usage", `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.9}},"usage":{"input_tokens":-10,"output_tokens":0}}`, noul},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := pr14Client(200, tc.body)
			if res, err := c.Ask(context.Background(), "s", tc.questions); err == nil {
				t.Fatalf("malformed response accepted: model=%s usage=%+v answer=%+v", res.Model, res.Usage, res.Answers["q"])
			}
		})
	}
}

type pr14BrokenBody struct{ body []byte }

func (b *pr14BrokenBody) Read(p []byte) (int, error) {
	n := copy(p, b.body)
	b.body = b.body[n:]
	return n, io.ErrUnexpectedEOF
}
func (*pr14BrokenBody) Close() error { return nil }

func TestPR14ReviewRejectsBodyReadFailure(t *testing.T) {
	c := New("fake-key")
	c.HTTP = &http.Client{Transport: pr14Transport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Request: r, Body: &pr14BrokenBody{[]byte(`{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.9}},"usage":{"input_tokens":10,"output_tokens":0}}`)}}, nil
	})}
	if _, err := c.Ask(context.Background(), "s", map[string]Question{"q": Noul("Is this s?")}); err == nil {
		t.Fatal("a response with an incomplete HTTP body was accepted as success")
	}
}

func TestPR14ReviewQuestionTextCountsTowardLimits(t *testing.T) {
	questions := map[string]Question{"q": Noul(strings.Repeat("x", (MaxStateTokens+1)*charsPerToken))}
	calls := 0
	c := New("fake-key")
	c.HTTP = &http.Client{Transport: pr14Transport(func(r *http.Request) (*http.Response, error) {
		calls++
		b, _ := json.Marshal(map[string]any{"model": DefaultModel, "answers": map[string]any{"q": map[string]any{"type": "noul", "noul": 0.9}}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 0}})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	})}
	_, err := c.Ask(context.Background(), "s", questions)
	if err == nil || calls != 0 {
		t.Fatalf("oversized question bypassed local request limit: requests=%d error=%v", calls, err)
	}
}
