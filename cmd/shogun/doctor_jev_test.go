package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctor reports Jev per configuration and never prints the key.
func TestDoctorReportsJev(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("JEV_TEST_KEY", "apik-0123456789-secret-secret")
	ws := t.TempDir()
	_, out, _ := runCLI(t, ws, "doctor")
	if !strings.Contains(out, "ok   jev              off") {
		t.Fatalf("default must be off:\n%s", out)
	}
	os.MkdirAll(filepath.Join(ws, ".shogun"), 0o700)
	os.WriteFile(filepath.Join(ws, ".shogun", "config.toml"), []byte("jev = \"advisory\"\njev_api_key_env = \"JEV_MISSING\"\n"), 0o600)
	if _, out, _ = runCLI(t, ws, "doctor"); !strings.Contains(out, "FAIL jev              advisory, but $JEV_MISSING is empty") {
		t.Fatalf("advisory without a key must fail:\n%s", out)
	}
	os.WriteFile(filepath.Join(ws, ".shogun", "config.toml"), []byte("jev = \"advisory\"\njev_api_key_env = \"JEV_TEST_KEY\"\n"), 0o600)
	_, out, _ = runCLI(t, ws, "doctor")
	if !strings.Contains(out, "ok   jev              advisory; key from $JEV_TEST_KEY (apik… (29 chars)); model jev-1.13.0") || strings.Contains(out, "secret") {
		t.Fatalf("advisory with a key:\n%s", out)
	}
	if !strings.Contains(out, "info jev record       no live record") {
		t.Fatalf("no record yet:\n%s", out)
	}
}

// doctor --live pings Jev independently of the CLI certificate and records the resolved model; the
// record is separate from the CLI preflight and a failed ping is its own FAIL row.
func TestDoctorLivePingsJevAndRecords(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("JEV_TEST_KEY", "apik-0123456789-secret-secret")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"model":"jev-1.13.0","answers":{"is_ping":{"type":"noul","noul":0.99}},"usage":{"input_tokens":21,"output_tokens":3}}`))
	}))
	defer srv.Close()
	old := jevBaseURL
	jevBaseURL = srv.URL
	t.Cleanup(func() { jevBaseURL = old })
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, ".shogun"), 0o700)
	os.WriteFile(filepath.Join(ws, ".shogun", "config.toml"), []byte("jev = \"advisory\"\njev_api_key_env = \"JEV_TEST_KEY\"\nclaude_command = \"/nonexistent/claude\"\ncodex_command = \"/nonexistent/codex\"\n"), 0o600)
	_, out, _ := runCLI(t, ws, "doctor", "--live")
	if !strings.Contains(out, "ok   jev live         jev-1.13.0 answered in") || !strings.Contains(out, "(21 input tokens)") || strings.Contains(out, "secret") {
		t.Fatalf("live row:\n%s", out)
	}
	if gotAuth != "Bearer apik-0123456789-secret-secret" {
		t.Fatalf("auth header %q", gotAuth)
	}
	b, err := os.ReadFile(filepath.Join(ws, ".shogun", "preflight", "jev.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec jevRecord
	json.Unmarshal(b, &rec)
	if rec.Model != "jev-1.13.0" || rec.Requested != "jev-1.13.0" || rec.KeyEnv != "JEV_TEST_KEY" || rec.BaseURL != srv.URL || strings.Contains(string(b), "secret") {
		t.Fatalf("record %s", b)
	}
	// The CLI preflight is untouched by the Jev record (the binaries were missing, so none was written).
	if entries, _ := os.ReadDir(filepath.Join(ws, ".shogun", "preflight")); len(entries) != 1 {
		t.Fatalf("preflight dir: %v", entries)
	}
	if _, out, _ = runCLI(t, ws, "doctor"); !strings.Contains(out, "info jev record       jev-1.13.0 answered in") {
		t.Fatalf("record not reported offline:\n%s", out)
	}

	// A 401 is a FAIL row with the API's message, and no record is written.
	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"type":"authentication_error","message":"Missing or invalid API key."}}`))
	}))
	defer srv401.Close()
	jevBaseURL = srv401.URL
	os.Remove(filepath.Join(ws, ".shogun", "preflight", "jev.json"))
	if _, out, _ = runCLI(t, ws, "doctor", "--live"); !strings.Contains(out, "FAIL jev live         jev auth (HTTP 401): authentication_error: Missing or invalid API key.") {
		t.Fatalf("401 row:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(ws, ".shogun", "preflight", "jev.json")); err == nil {
		t.Fatal("a failed ping wrote a record")
	}
}
