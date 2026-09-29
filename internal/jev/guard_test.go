package jev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every built-in pattern fires on its kind of secret or identifier, and the request is refused
// before it leaves: the fake server sees nothing, and the error names patterns, never the text.
func TestGuardRefusesSensitiveStates(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Key: "k"}
	// Secret-shaped samples are assembled at run time from harmless parts, so that no literal in
	// this repository matches a secret scanner (GitHub flagged an earlier literal as a real key).
	x := func(n int) string { return strings.Repeat("x", n) }
	cases := map[string]string{
		"aws_access_key":        "use " + "AKIA" + strings.Repeat("Q", 16) + " for the bucket",
		"prefixed_token":        "the key is " + "apik" + "-" + x(20),
		"google_api_key":        "AI" + "za" + x(32),
		"jwt":                   "header " + "ey" + "J" + x(12) + "." + x(12) + ".sig",
		"private_key":           "-----BEGIN " + "RSA PRIVATE KEY" + "-----",
		"bearer_token":          "Authorization: Bearer " + x(16),
		"credential_assignment": "set password = " + x(8) + " in the env",
		"token_assignment":      "TOKEN: " + x(16),
		"credential_url":        "mongodb://app:" + x(6) + "@db.internal/x",
		"email":                 "ask someone@example.com",
		"ipv4":                  "the NLB at 10.20.30.40",
		"aws_account_id":        "registry 123456789012",
		"home_path":             "/Users/somebody/workspace/x",
	}
	for want, text := range cases {
		_, err := c.Ask(context.Background(), map[string]string{"task": text}, map[string]Question{"q": Noul("x")})
		e := classIs(t, err, ClassSensitive)
		if !strings.Contains(e.Msg, want+"×") {
			t.Errorf("%s: %s", want, e.Msg)
		}
		for _, leak := range []string{"AKIA", "apik-", "xxxxxx", "example.com", "10.20.30.40", "123456789012", "somebody"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: the error repeats the text: %s", want, err)
			}
		}
	}
	if calls != 0 {
		t.Fatalf("%d request(s) left despite the guard", calls)
	}
}

// Ordinary plan text passes; "input_tokens: 100" is not a token assignment.
func TestGuardPassesPlanText(t *testing.T) {
	state := map[string]any{"task": "Add a --json flag to `shogun stats`; input_tokens: 100, cost_usd 0.5; see cmd/shogun/stats.go:65-92 and .shogun/runs",
		"criteria": []string{"R-001.C1: stdout parses as one JSON object", "tokens_lower_bound true when damaged > 0"}}
	b, _ := json.Marshal(state)
	if ms := Scan(string(b), nil); len(ms) != 0 {
		t.Fatalf("false alarms: %s", Describe(ms))
	}
}

// Configured deny patterns (internal domains, project names) are enforced like the built-ins.
func TestGuardHonoursConfiguredDeny(t *testing.T) {
	extra, err := CompileDeny([]string{`(?i)openvpn\.in\b`, `(?i)\bcipherscale\b`})
	if err != nil {
		t.Fatal(err)
	}
	ms := Scan("deploy on jenkins-prod.openvpn.in and tell CipherScale", extra)
	if Describe(ms) != "deny:1×1, deny:2×1" {
		t.Fatalf("%s", Describe(ms))
	}
	if _, err := CompileDeny([]string{`(`}); err == nil || !strings.Contains(err.Error(), "jev_deny[0]") {
		t.Fatalf("bad regexp accepted: %v", err)
	}
	c := &Client{Key: "k", Deny: extra}
	if _, err := c.Ask(context.Background(), "see wiki.openvpn.in", map[string]Question{"q": Noul("x")}); err == nil {
		t.Fatal("configured deny not enforced by Ask")
	}
}
