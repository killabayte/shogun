//go:build jevlive

package eval

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type pr17LiveTransport func(*http.Request) (*http.Response, error)

func (f pr17LiveTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Executed only in a controlled child process. A fake transport records that an
// HTTP attempt would have occurred, then refuses it; it never opens a connection.
func TestPR17LiveHarnessHelper(t *testing.T) {
	if os.Getenv("PR17_GUARD_CHILD") != "1" {
		t.Skip("offline subprocess helper")
	}
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: pr17LiveTransport(func(*http.Request) (*http.Response, error) {
		f, err := os.OpenFile(os.Getenv("PR17_CALLS_FILE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = f.WriteString("attempt\n")
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("offline canary: external networking is disabled")
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	TestLiveEvaluation(t)
}

func TestPR17LiveConfigurationFailsClosed(t *testing.T) {
	for _, mode := range []string{"workspace_deny", "invalid_global_deny"} {
		t.Run(mode, func(t *testing.T) {
			home, ws := t.TempDir(), t.TempDir()
			pkg := filepath.Join(ws, "internal", "jev", "eval")
			if err := os.MkdirAll(pkg, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module review.test\n\ngo 1.26.3\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var configPath, configText string
			if mode == "workspace_deny" {
				configPath = filepath.Join(ws, ".shogun", "config.toml")
				configText = "jev_deny = [\"(?i)shogun\"]\n"
			} else {
				configPath = filepath.Join(home, ".config", "shogun", "config.toml")
				configText = "jev_deny = [\"(\"]\n"
			}
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
				t.Fatal(err)
			}
			callsPath := filepath.Join(t.TempDir(), "calls")
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("JEV_DEFAULT_TEST", "review-only-key")
			t.Setenv("TYPESAFE_API_KEY", "")
			t.Setenv("JEV_EVAL_OUT", filepath.Join(t.TempDir(), "report.json"))
			t.Setenv("PR17_GUARD_CHILD", "1")
			t.Setenv("PR17_CALLS_FILE", callsPath)
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin, "-test.run=^TestPR17LiveHarnessHelper$")
			cmd.Dir = pkg
			output, runErr := cmd.CombinedOutput()
			calls, _ := os.ReadFile(callsPath)
			if len(calls) != 0 {
				t.Fatalf("%s policy did not stop egress: %d transport attempts (all intercepted offline)", mode, strings.Count(string(calls), "attempt\n"))
			}
			if runErr == nil {
				t.Fatalf("expected a policy/config rejection, got success: %s", output)
			}
		})
	}
}
