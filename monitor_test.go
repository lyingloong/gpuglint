package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lyingloong/gpuglint/internal/collect"
	"github.com/lyingloong/gpuglint/internal/state"
)

func TestWatchLifecycle(t *testing.T) {
	dir := t.TempDir()
	cfg, file := filepath.Join(dir, "config"), filepath.Join(dir, ".gpuglint", "state")
	if err := os.WriteFile(cfg, []byte("Host gpu-01\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := make(chan struct{})
	var calls atomic.Int32
	poll := func(ctx context.Context, host, config string) collect.Result {
		select {
		case <-ctx.Done():
			return collect.Result{Code: "cancelled"}
		case <-gate:
		}
		if calls.Add(1) == 1 {
			return collect.Result{Backend: "nvidia-smi", GPUs: []collect.GPU{{Index: 0, Name: "A100"}}}
		}
		return collect.Result{Code: "ssh_error", Message: "socket closed"}
	}
	done := make(chan error, 1)
	go func() {
		_, err := run(ctx, []string{"watch", "--headless", "--interval", "5ms", "--ssh-config", cfg, "--state-file", file}, io.Discard, io.Discard, poll)
		done <- err
	}()
	waitHost := func(want string) hostState {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			b, err := state.Read(file)
			var s snapshot
			if err == nil && json.Unmarshal(b, &s) == nil && s.Hosts[0].Status == want {
				return s.Hosts[0]
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", want)
		return hostState{}
	}
	waitHost("pending")
	close(gate)
	h := waitHost("error")
	if h.LastSuccess == nil || h.LastAttempt.Before(*h.LastSuccess) || h.Error.Code != "ssh_error" || len(h.GPUs) != 1 || h.Backend != "nvidia-smi" {
		t.Fatalf("previous sample not preserved: %+v", h)
	}
	var out bytes.Buffer
	args := []string{"status", "--json", "--state-file", file}
	if code, err := run(ctx, args, &out, io.Discard, nil); code != 0 || err != nil || !json.Valid(out.Bytes()) {
		t.Fatalf("status: %d, %v", code, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(file)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state dir still exists: %v", err)
	}
	if code, err := run(context.Background(), args, &out, io.Discard, nil); code != 3 || !errors.Is(err, state.ErrInactive) {
		t.Fatalf("inactive status: %d, %v", code, err)
	}
}

func TestPartialScan(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(cfg, []byte("Host ok offline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	poll := func(_ context.Context, host, _ string) collect.Result {
		if host == "ok" {
			return collect.Result{Backend: "nvtop", GPUs: []collect.GPU{}}
		}
		return collect.Result{Code: "timeout", Message: "poll timed out"}
	}
	var out bytes.Buffer
	code, err := run(context.Background(), []string{"scan", "--json", "--ssh-config", cfg}, &out, io.Discard, poll)
	var s snapshot
	if err != nil || code != 2 || json.Unmarshal(out.Bytes(), &s) != nil || len(s.Hosts) != 2 || s.Hosts[0].Status != "no_gpu" || s.Hosts[1].Error.Code != "timeout" {
		t.Fatalf("partial scan: %d, %v, %s", code, err, out.String())
	}
}

func TestFreeMemorySort(t *testing.T) {
	n := func(v float64) *float64 { return &v }
	now := time.Now().UTC()
	s := newSnapshot([]string{"small", "stale", "large"}, time.Minute)
	for i, size := range []float64{1000, 9000, 4000} {
		s.apply(update{index: i, started: now, ended: now, result: collect.Result{Backend: "nvtop", GPUs: []collect.GPU{{MemoryUsedMiB: n(10), MemoryTotalMiB: n(size)}}}}, options{})
	}
	s.apply(update{index: 1, started: now, ended: now, result: collect.Result{Code: "timeout"}}, options{})
	var out bytes.Buffer
	if err := drawTable(&out, s, "free"); err != nil {
		t.Fatal(err)
	}
	table := out.String()
	if strings.Index(table, "large") > strings.Index(table, "small") || strings.Index(table, "small") > strings.Index(table, "stale") || !strings.Contains(table, "(stale)") {
		t.Fatal(table)
	}
}
