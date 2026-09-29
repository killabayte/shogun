// Package jev is a minimal client for TypeSafe AI's System One API ("Jev"): a state and a map of
// typed questions in, calibrated probabilities out, no generated text. Shogun uses it as an
// advisory classifier (docs/plans/shogun-jev.md). The client enforces the documented limits before
// sending, validates the response shape, never retries and never logs the key.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the official endpoint (docs.typesafe.ai/api).
	DefaultBaseURL = "https://api.typesafe.ai/v1/systemone"
	// DefaultModel is pinned: aliases such as jev-latest move on their own.
	DefaultModel = "jev-1.13.0"
	// DefaultKeyEnv is the provider's own convention for the API key variable.
	DefaultKeyEnv = "TYPESAFE_API_KEY"
	// MaxStateTokens is the documented state budget of Jev 1.13 (32k of a 64k request).
	MaxStateTokens = 32_000
	// MaxQuestions per request is Shogun's own rule for atomic, per-item requests.
	MaxQuestions = 8
	// MaxRequestTokens is the documented request budget of Jev 1.13 (state plus all questions).
	MaxRequestTokens = 64_000
	// DefaultTimeout bounds one request (design §4); Jev answers in well under a second.
	DefaultTimeout = 5 * time.Second
	// Aliases resolve to a versioned id on the server; a pinned id must come back unchanged.
	aliasLatest, aliasPreview = "jev-latest", "jev-preview"
	// probabilityTolerance is the accepted deviation of a distribution's sum from 1 (rounding).
	probabilityTolerance = 0.02

	charsPerToken = 4
)

// Question is one typed question. Criteria is a map option→description for a choice, an ordered
// slice of level descriptions for a score, nil for a noul.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Noul asks a yes/no question and gets P(true).
func Noul(instructions string) Question { return Question{Type: "noul", Instructions: instructions} }

// Choice asks for one of the options; every option maps to a short description.
func Choice(instructions string, options map[string]string) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: options}
}

// Score asks for a position on an ordered rubric, levels described from low to high.
func Score(instructions string, levels []string) Question {
	return Question{Type: "score", Instructions: instructions, Criteria: levels}
}

// Answer is one question's result as the API returns it.
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
}

// P is the probability of an outcome: "true"/"false" for a noul, an option for a choice, a level
// index ("0", "1", …) for a score. Unknown outcomes are 0.
func (a Answer) P(outcome string) float64 {
	if a.Type == "noul" && a.Noul != nil {
		switch outcome {
		case "true":
			return *a.Noul
		case "false":
			return 1 - *a.Noul
		}
		return 0
	}
	return a.Probabilities[outcome]
}

// Usage is what the API charges for (input only; output is free).
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Result is one successful request.
type Result struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	Latency time.Duration     `json:"latency"`
}

// Class sorts failures for the caller: none of them is retried by the client.
type Class string

const (
	ClassAuth       Class = "auth"       // no key or 401
	ClassInvalid    Class = "invalid"    // 422: the API rejected the request
	ClassRate       Class = "rate"       // 429
	ClassOverloaded Class = "overloaded" // 529
	ClassTimeout    Class = "timeout"    // the request did not finish within Timeout
	ClassShape      Class = "shape"      // the response is not what the contract promises
	ClassTransport  Class = "transport"  // connection failures and other statuses
	ClassLimit      Class = "limit"      // Shogun refused to send: a documented limit would be exceeded
)

// Error is a classified failure. Its text never contains the key.
type Error struct {
	Class  Class
	Status int
	Msg    string
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("jev %s (HTTP %d): %s", e.Class, e.Status, e.Msg)
	}
	return fmt.Sprintf("jev %s: %s", e.Class, e.Msg)
}

// Client talks to one endpoint with one key and one pinned model.
type Client struct {
	BaseURL string
	Model   string
	Key     string
	Timeout time.Duration
	HTTP    *http.Client
}

// New returns a client for the official endpoint with the pinned model.
func New(key string) *Client {
	return &Client{BaseURL: DefaultBaseURL, Model: DefaultModel, Key: key, Timeout: DefaultTimeout}
}

var reName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Ask sends one request. Limits are checked first (ClassLimit), then the response is validated
// against the questions (ClassShape): every question answered, with its type, its values in range.
func (c *Client) Ask(ctx context.Context, state any, questions map[string]Question) (*Result, error) {
	if c.Key == "" {
		return nil, &Error{Class: ClassAuth, Msg: "no API key"}
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return nil, &Error{Class: ClassLimit, Msg: "state is not serializable: " + err.Error()}
	}
	if err := checkLimits(stateJSON, questions); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(struct {
		Model     string              `json:"model"`
		State     json.RawMessage     `json:"state"`
		Questions map[string]Question `json:"questions"`
	}{c.model(), stateJSON, questions})
	res, err := c.do(ctx, body, questions)
	if err != nil {
		// Every message that could carry external text passes through scrub: an API error
		// envelope, a body excerpt, a shape message quoting a returned field.
		var e *Error
		if errors.As(err, &e) {
			e.Msg = scrub(e.Msg, c.Key)
		}
		return nil, err
	}
	return res, nil
}

const maxResponseBytes = 1 << 20

func (c *Client) do(ctx context.Context, body []byte, questions map[string]Question) (*Result, error) {

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, c.baseURL(), bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Class: ClassTransport, Msg: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	start := time.Now()
	resp, err := httpc.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, &Error{Class: ClassTimeout, Msg: fmt.Sprintf("no answer within %s", timeout)}
		}
		return nil, &Error{Class: ClassTransport, Msg: scrub(err.Error(), c.Key)}
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	latency := time.Since(start)
	if rerr != nil {
		// A truncated body is not a completed request, whatever its prefix parses as.
		if errors.Is(rerr, context.DeadlineExceeded) || isTimeout(rerr) {
			return nil, &Error{Class: ClassTimeout, Msg: fmt.Sprintf("the response did not arrive within %s", timeout)}
		}
		return nil, &Error{Class: ClassTransport, Status: resp.StatusCode, Msg: "reading the response body: " + rerr.Error()}
	}
	if len(raw) > maxResponseBytes {
		return nil, &Error{Class: ClassShape, Status: resp.StatusCode, Msg: "response larger than 1 MiB"}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Class: classOf(resp.StatusCode), Status: resp.StatusCode, Msg: apiMessage(raw)}
	}
	var wire struct {
		Model   string            `json:"model"`
		Answers map[string]Answer `json:"answers"`
		Usage   *Usage            `json:"usage"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, &Error{Class: ClassShape, Msg: "response is not JSON: " + err.Error()}
	}
	if wire.Usage == nil {
		return nil, &Error{Class: ClassShape, Msg: "response carries no usage"}
	}
	res := &Result{Model: wire.Model, Answers: wire.Answers, Usage: *wire.Usage, Latency: latency}
	if err := validate(res, c.model(), questions); err != nil {
		return nil, err
	}
	return res, nil
}

func (c *Client) model() string {
	if c.Model == "" {
		return DefaultModel
	}
	return c.Model
}

func (c *Client) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

// EstimateTokens is the client's state-size estimate (4 characters per token).
func EstimateTokens(stateJSON []byte) int {
	return (len(stateJSON) + charsPerToken - 1) / charsPerToken
}

// checkLimits is a heuristic (4 characters per token), not the provider's tokenizer: the state
// plus the longest question must fit the 32k-token budget, the whole request the 64k one.
func checkLimits(stateJSON []byte, questions map[string]Question) error {
	if len(questions) == 0 || len(questions) > MaxQuestions {
		return &Error{Class: ClassLimit, Msg: fmt.Sprintf("%d question(s); 1 to %d per request", len(questions), MaxQuestions)}
	}
	longest, all := 0, 0
	for _, q := range questions {
		b, _ := json.Marshal(q)
		all += len(b)
		if len(b) > longest {
			longest = len(b)
		}
	}
	if n := EstimateTokens(stateJSON) + EstimateTokens(make([]byte, longest)); n > MaxStateTokens {
		return &Error{Class: ClassLimit, Msg: fmt.Sprintf("state plus the longest question is about %d tokens, above the %d-token budget", n, MaxStateTokens)}
	}
	if n := EstimateTokens(stateJSON) + EstimateTokens(make([]byte, all)); n > MaxRequestTokens {
		return &Error{Class: ClassLimit, Msg: fmt.Sprintf("the request is about %d tokens, above the %d-token budget", n, MaxRequestTokens)}
	}
	for name, q := range questions {
		if !reName.MatchString(name) {
			return &Error{Class: ClassLimit, Msg: fmt.Sprintf("question name %q is not an identifier of at most 64 characters", name)}
		}
		if strings.TrimSpace(q.Instructions) == "" {
			return &Error{Class: ClassLimit, Msg: fmt.Sprintf("question %s has no instructions", name)}
		}
		switch q.Type {
		case "noul":
		case "choice":
			opts, ok := q.Criteria.(map[string]string)
			if !ok || len(opts) < 2 || len(opts) > 255 {
				return &Error{Class: ClassLimit, Msg: fmt.Sprintf("question %s: a choice needs 2 to 255 options", name)}
			}
		case "score":
			levels, ok := q.Criteria.([]string)
			if !ok || len(levels) < 2 || len(levels) > 10 {
				return &Error{Class: ClassLimit, Msg: fmt.Sprintf("question %s: a score needs 2 to 10 levels", name)}
			}
		default:
			return &Error{Class: ClassLimit, Msg: fmt.Sprintf("question %s: unknown type %q", name, q.Type)}
		}
	}
	return nil
}

// validate holds the response to the contract: the requested model (an alias may resolve to any
// versioned id; a pinned id must come back unchanged), non-negative usage, and for every question an
// answer of its type whose values are probabilities — a choice's distribution over exactly the
// offered options, a score's over exactly its levels, each summing to 1 within the tolerance, with
// a confidence in [0,1].
func validate(res *Result, requested string, questions map[string]Question) error {
	switch {
	case res.Model == "":
		return &Error{Class: ClassShape, Msg: "response names no model"}
	case requested != aliasLatest && requested != aliasPreview && res.Model != requested:
		return &Error{Class: ClassShape, Msg: fmt.Sprintf("response from model %q, requested %q", res.Model, requested)}
	case !strings.HasPrefix(res.Model, "jev-"):
		return &Error{Class: ClassShape, Msg: fmt.Sprintf("response from an unexpected model %q", res.Model)}
	}
	if res.Usage.InputTokens < 0 || res.Usage.OutputTokens < 0 {
		return &Error{Class: ClassShape, Msg: "negative usage"}
	}
	for name, q := range questions {
		a, ok := res.Answers[name]
		if !ok {
			return &Error{Class: ClassShape, Msg: "no answer to " + name}
		}
		if a.Type != q.Type {
			return &Error{Class: ClassShape, Msg: fmt.Sprintf("%s answered as %s, asked as %s", name, a.Type, q.Type)}
		}
		switch q.Type {
		case "noul":
			if a.Noul == nil || !unit(*a.Noul) {
				return &Error{Class: ClassShape, Msg: name + ": noul missing or outside [0,1]"}
			}
		case "choice":
			opts := q.Criteria.(map[string]string)
			keys := map[string]bool{}
			for o := range opts {
				keys[o] = true
			}
			if !keys[a.Choice] {
				return &Error{Class: ClassShape, Msg: fmt.Sprintf("%s: the chosen option is not one offered", name)}
			}
			if err := distribution(name, a.Probabilities, keys, a.Choice); err != nil {
				return err
			}
			if a.Confidence == nil || !unit(*a.Confidence) {
				return &Error{Class: ClassShape, Msg: name + ": confidence missing or outside [0,1]"}
			}
		case "score":
			levels := q.Criteria.([]string)
			keys := map[string]bool{}
			for i := range levels {
				keys[fmt.Sprint(i)] = true
			}
			if a.Score == nil || *a.Score < 0 || *a.Score > float64(len(levels)-1) {
				return &Error{Class: ClassShape, Msg: name + ": score missing or outside its levels"}
			}
			if err := distribution(name, a.Probabilities, keys, ""); err != nil {
				return err
			}
			if a.Confidence == nil || !unit(*a.Confidence) {
				return &Error{Class: ClassShape, Msg: name + ": confidence missing or outside [0,1]"}
			}
		}
	}
	return nil
}

func unit(p float64) bool { return p >= 0 && p <= 1 && p == p }

// distribution checks a probability map: keys are exactly the allowed ones (a missing key counts
// as absent probability for a selected option), every value in [0,1], the sum 1 within tolerance.
func distribution(name string, probs map[string]float64, allowed map[string]bool, selected string) *Error {
	if len(probs) == 0 {
		return &Error{Class: ClassShape, Msg: name + ": no probabilities"}
	}
	sum := 0.0
	for k, p := range probs {
		if !allowed[k] {
			return &Error{Class: ClassShape, Msg: name + ": a probability for an option that was not offered"}
		}
		if !unit(p) {
			return &Error{Class: ClassShape, Msg: name + ": a probability outside [0,1]"}
		}
		sum += p
	}
	if selected != "" {
		if _, ok := probs[selected]; !ok {
			return &Error{Class: ClassShape, Msg: name + ": no probability for the chosen option"}
		}
	}
	if sum < 1-probabilityTolerance || sum > 1+probabilityTolerance {
		return &Error{Class: ClassShape, Msg: fmt.Sprintf("%s: probabilities sum to %.2f", name, sum)}
	}
	return nil
}

func classOf(status int) Class {
	switch status {
	case http.StatusUnauthorized:
		return ClassAuth
	case http.StatusUnprocessableEntity:
		return ClassInvalid
	case http.StatusTooManyRequests:
		return ClassRate
	case 529:
		return ClassOverloaded
	}
	return ClassTransport
}

// apiMessage extracts {"error":{"type","message"}} when present, else a short excerpt of the body.
func apiMessage(raw []byte) string {
	var e struct {
		Error struct {
			Type, Message string
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		if e.Error.Type != "" {
			return e.Error.Type + ": " + e.Error.Message
		}
		return e.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "empty response body"
	}
	return s
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// scrub removes the key from any message built from external text (error envelopes, body
// excerpts, transport errors, returned fields), should a server or proxy ever echo it.
func scrub(msg, key string) string {
	if key == "" {
		return msg
	}
	return strings.ReplaceAll(msg, key, "<key>")
}

// Redact describes a key without revealing it: its prefix and length.
func Redact(key string) string {
	if key == "" {
		return "empty"
	}
	n := 4
	if len(key) < n {
		n = len(key)
	}
	return fmt.Sprintf("%s… (%d chars)", key[:n], len(key))
}
