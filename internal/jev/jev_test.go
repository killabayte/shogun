package jev

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The body of the first live call (design §0.1), used as the fixture of a well-formed answer.
const liveBody = `{"model":"jev-1.13.0","answers":{
  "single_change":{"type":"noul","noul":0.95},
  "size":{"type":"choice","choice":"small","confidence":0.84,"probabilities":{"small":0.89,"medium":0.1,"large":0.0,"unclear":0.01}},
  "clarity":{"type":"score","score":2.97,"confidence":0.97,"legend":{"0":"no outcome stated","1":"outcome implied","2":"outcome stated","3":"outcome and constraints stated"},"probabilities":{"0":0.0,"1":0.0,"2":0.03,"3":0.97}}},
 "usage":{"input_tokens":453,"output_tokens":78}}`

func liveQuestions() map[string]Question {
	return map[string]Question{
		"single_change": Noul("Does `task` describe one bounded change rather than several independent ones?"),
		"size":          Choice("How large is `task`?", map[string]string{"small": "one file", "medium": "a few files", "large": "several repositories", "unclear": "cannot tell"}),
		"clarity":       Score("How clearly does `task` state what must be true when it is done?", []string{"no outcome stated", "outcome implied", "outcome stated", "outcome and constraints stated"}),
	}
}

// testKey is assembled at run time so that no literal in the repository looks like a key.
var testKey = "apik" + "-test-key-" + strings.Repeat("0", 10)

type recorded struct {
	auth, contentType string
	body              map[string]any
}

func server(t *testing.T, status int, body string, rec *recorded) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec != nil {
			rec.auth, rec.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
			json.NewDecoder(r.Body).Decode(&rec.body)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Key: testKey, Timeout: 2 * time.Second}
}

func classIs(t *testing.T, err error, want Class) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Class != want {
		t.Fatalf("error %v, want class %s", err, want)
	}
	return e
}

// A well-formed answer is parsed, typed, and the request carried the key, the model and the state.
func TestAskParsesTheLiveShape(t *testing.T) {
	var rec recorded
	c := server(t, 200, liveBody, &rec)
	res, err := c.Ask(context.Background(), map[string]string{"task": "Add a --json flag"}, liveQuestions())
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "jev-1.13.0" || res.Usage.InputTokens != 453 || res.Latency <= 0 {
		t.Fatalf("result %+v", res)
	}
	if p := res.Answers["single_change"].P("true"); p != 0.95 {
		t.Fatalf("noul %v", p)
	}
	if a := res.Answers["size"]; a.Choice != "small" || a.P("small") != 0.89 || a.P("medium") != 0.1 || *a.Confidence != 0.84 {
		t.Fatalf("choice %+v", a)
	}
	if a := res.Answers["clarity"]; *a.Score != 2.97 || a.P("3") != 0.97 || a.Legend["3"] == "" {
		t.Fatalf("score %+v", a)
	}
	if rec.auth != "Bearer "+testKey || rec.contentType != "application/json" || rec.body["model"] != DefaultModel {
		t.Fatalf("request %+v", rec)
	}
	if st, _ := rec.body["state"].(map[string]any); st["task"] != "Add a --json flag" {
		t.Fatalf("state %v", rec.body["state"])
	}
}

// Every documented status maps to a class; the API's message is kept, the key never appears.
func TestAskClassifiesStatuses(t *testing.T) {
	for status, want := range map[int]Class{401: ClassAuth, 422: ClassInvalid, 429: ClassRate, 529: ClassOverloaded, 500: ClassTransport} {
		c := server(t, status, `{"error":{"type":"some_error","message":"the message"}}`, nil)
		_, err := c.Ask(context.Background(), "s", map[string]Question{"q": Noul("Is `state` s?")})
		e := classIs(t, err, want)
		if e.Status != status || !strings.Contains(e.Msg, "the message") || strings.Contains(err.Error(), c.Key) {
			t.Fatalf("%d: %v", status, err)
		}
	}
}

// No key: refused before any request.
func TestAskWithoutKeyIsAuth(t *testing.T) {
	c := server(t, 200, liveBody, nil)
	c.Key = ""
	_, err := c.Ask(context.Background(), "s", map[string]Question{"q": Noul("x")})
	classIs(t, err, ClassAuth)
}

// A slow server is a timeout, not a hang.
func TestAskTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(liveBody))
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Key: "k", Timeout: 50 * time.Millisecond}
	_, err := c.Ask(context.Background(), "s", map[string]Question{"q": Noul("x")})
	classIs(t, err, ClassTimeout)
}

// Documented limits are enforced locally, before any request leaves.
func TestAskRefusesOverLimit(t *testing.T) {
	c := server(t, 200, liveBody, nil)
	big := strings.Repeat("x", MaxStateTokens*charsPerToken+8)
	cases := map[string]struct {
		state any
		qs    map[string]Question
	}{
		"state too large":   {big, map[string]Question{"q": Noul("x")}},
		"no questions":      {"s", map[string]Question{}},
		"too many":          {"s", nine()},
		"bad name":          {"s", map[string]Question{"has space": Noul("x")}},
		"empty instruction": {"s", map[string]Question{"q": Noul("  ")}},
		"one option":        {"s", map[string]Question{"q": Choice("x", map[string]string{"a": "a"})}},
		"eleven levels":     {"s", map[string]Question{"q": Score("x", make([]string, 11))}},
		"unknown type":      {"s", map[string]Question{"q": {Type: "essay", Instructions: "x"}}},
	}
	for name, tc := range cases {
		_, err := c.Ask(context.Background(), tc.state, tc.qs)
		if classIs(t, err, ClassLimit); err == nil {
			t.Fatal(name)
		}
	}
}

func nine() map[string]Question {
	m := map[string]Question{}
	for i := 0; i < MaxQuestions+1; i++ {
		m["q"+string(rune('a'+i))] = Noul("x")
	}
	return m
}

// An answer that does not match the questions is a shape failure, never silently used.
func TestAskValidatesTheShape(t *testing.T) {
	q := map[string]Question{"size": Choice("x", map[string]string{"small": "s", "large": "l"}), "ok": Noul("y")}
	for name, body := range map[string]string{
		"not json":        `<html>`,
		"no model":        `{"answers":{"size":{"type":"choice","choice":"small","probabilities":{"small":1}},"ok":{"type":"noul","noul":0.5}}}`,
		"missing answer":  `{"model":"m","answers":{"ok":{"type":"noul","noul":0.5}}}`,
		"wrong type":      `{"model":"m","answers":{"size":{"type":"noul","noul":0.5},"ok":{"type":"noul","noul":0.5}}}`,
		"unknown option":  `{"model":"m","answers":{"size":{"type":"choice","choice":"medium","probabilities":{"medium":1}},"ok":{"type":"noul","noul":0.5}}}`,
		"noul out of 0-1": `{"model":"m","answers":{"size":{"type":"choice","choice":"small","probabilities":{"small":1}},"ok":{"type":"noul","noul":1.5}}}`,
	} {
		c := server(t, 200, body, nil)
		_, err := c.Ask(context.Background(), "s", q)
		if classIs(t, err, ClassShape); err == nil {
			t.Fatal(name)
		}
	}
}

func TestRedactShowsPrefixAndLengthOnly(t *testing.T) {
	if got := Redact("apik-secret-secret"); got != "apik… (18 chars)" || strings.Contains(got, "secret") {
		t.Fatal(got)
	}
	if Redact("") != "empty" {
		t.Fatal("empty")
	}
}
