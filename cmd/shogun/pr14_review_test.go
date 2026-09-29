package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/killabayte/shogun/internal/config"
)

type pr14DoctorTransport func(*http.Request) (*http.Response, error)

func (f pr14DoctorTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPR14ReviewDoctorDoesNotClaimUnwrittenRecord(t *testing.T) {
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: pr14DoctorTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader(`{"model":"jev-1.13.0","answers":{"is_ping":{"type":"noul","noul":0.99}},"usage":{"input_tokens":10,"output_tokens":0}}`))}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	for _, mode := range []string{"mkdir_failure", "write_failure"} {
		t.Run(mode, func(t *testing.T) {
			ws := t.TempDir()
			if mode == "mkdir_failure" {
				if err := os.WriteFile(filepath.Join(ws, ".shogun"), []byte("block"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(jevRecordPath(ws), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var out strings.Builder
			a := &app{ctx: context.Background(), cwd: ws, stdout: &out}
			if a.jevLive(config.Default(), "fake-key") || !strings.Contains(out.String(), "FAIL") {
				t.Fatalf("doctor claimed success despite %s: %s", mode, out.String())
			}
		})
	}
}
