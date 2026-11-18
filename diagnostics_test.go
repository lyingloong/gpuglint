package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lyingloong/gpuglint/internal/collect"
	"github.com/lyingloong/gpuglint/internal/state"
)

func TestDetailsInScanAndStatus(t *testing.T) {
	dir := t.TempDir()
	cfg, path := filepath.Join(dir, "config"), filepath.Join(dir, "state")
	if err := os.WriteFile(cfg, []byte("Host good bad\n"), 0600); err != nil {
		t.Fatal(err)
	}
	poll := func(_ context.Context, alias, _ string) collect.Result {
		if alias == "bad" {
			return collect.Result{Code: "ssh_error", Message: "SSH permission denied\n\x1b[31m(publickey)"}
		}
		return collect.Result{Backend: "nvtop"}
	}
	var out bytes.Buffer
	runView := func(args ...string) int {
		t.Helper()
		out.Reset()
		code, err := run(context.Background(), args, &out, io.Discard, poll)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	if runView("scan", "--ssh-config", cfg, "--errors") != 2 {
		t.Fatal("expected a partial scan")
	}
	detail := out.String()
	if !strings.Contains(detail, "bad  [ssh_error]") || !strings.Contains(detail, "(publickey)") || strings.ContainsAny(detail, "\x1b") || strings.Contains(detail, "good") {
		t.Fatal(detail)
	}
	runView("scan", "--ssh-config", cfg, "--json")
	w, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Write(out.Bytes()); err != nil {
		t.Fatal(err)
	}
	if runView("status", "--state-file", path, "--errors") != 0 || out.String() != detail {
		t.Fatal(out.String())
	}
}
