package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lyingloong/gpuglint/internal/collect"
	"github.com/rivo/uniseg"
)

func TestHostOverviewSummarizesCardsAndContainsErrors(t *testing.T) {
	number := func(n float64) *float64 { return &n }
	now := time.Now()
	cards := []collect.GPU{
		{Index: 1, Name: "NVIDIA H100", UtilizationPct: number(100), MemoryUsedMiB: number(1024), MemoryTotalMiB: number(8192)},
		{Index: 0, Name: "NVIDIA H100", UtilizationPct: number(0), MemoryUsedMiB: number(0), MemoryTotalMiB: number(4096)},
	}
	s := snapshot{Updated: now, IntervalSeconds: 15, Hosts: []hostState{
		{Alias: "train-a", Status: "ok", GPUs: cards, LastSuccess: &now},
		{Alias: "train-b", Status: "error", GPUs: cards, LastSuccess: &now, Error: &failure{"ssh_error", "Permission denied\n" + strings.Repeat("sensitive stderr", 200) + "\x1b[2J"}},
		{Alias: "login", Status: "no_gpu"},
	}}
	lines := hostOverview(s, "free", 120, 24, true, false)
	view := strings.Join(lines, "\n")
	for _, want := range []string{"3 hosts · 1 error", "2 × H100", "▁█ 100%", "7.0 GiB", "Auth denied (stale)", "No GPUs", "gpuglint status --errors"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q in\n%s", want, view)
		}
	}
	for _, once := range []string{"train-a", "train-b", "2 × H100", "7.0 GiB"} {
		if strings.Count(view, once) != 1 {
			t.Errorf("expected once: %q in\n%s", once, view)
		}
	}
	if strings.Contains(view, "sensitive stderr") || strings.Contains(view, "\x1b") {
		t.Fatalf("untrusted output:\n%s", view)
	}
	if s.Hosts[0].GPUs[0].Index != 1 {
		t.Fatal("display changed the GPU order in the snapshot")
	}
}

func TestViewportFitsAndPreservesHostSuffix(t *testing.T) {
	s := newSnapshot(nil, time.Second)
	for i := 0; i < 35; i++ {
		s.Hosts = append(s.Hosts, hostState{Alias: fmt.Sprintf("算力中心-训练集群-节点-%02d", i), Status: "pending"})
	}
	for _, w := range []int{12, 24, 39, 40, 48, 62, 72, 78, 80, 95, 96, 103, 104, 120} {
		lines := hostOverview(s, "host", w, 12, true, false)
		if len(lines) > 12 {
			t.Fatalf("%d columns: %d rendered rows", w, len(lines))
		}
		for _, line := range lines {
			if n := uniseg.StringWidth(line); n > w {
				t.Fatalf("width %d > %d: %q", n, w, line)
			}
		}
		if w >= 40 {
			view := strings.Join(lines, "\n")
			if !strings.Contains(view, "-00") || !strings.Contains(view, "+27 hosts") || !strings.Contains(view, "gpuglint status") {
				t.Fatalf("width %d: lost alias, overflow count or action:\n%s", w, view)
			}
		}
	}
}

func TestDashboardUsesRowsBeforeDecorations(t *testing.T) {
	s := newSnapshot([]string{"one", "two", "three", "four", "five", "six", "seven"}, time.Second)
	s.Hosts[6].Status = "error"
	s.Hosts[6].Error = &failure{"timeout", ""}
	lines := hostOverview(s, "host", 47, 10, true, false)
	if len(lines) != 10 {
		t.Fatalf("got %d rows, want 10", len(lines))
	}
	view := strings.Join(lines, "\n")
	for _, h := range s.Hosts {
		if !strings.Contains(view, h.Alias) {
			t.Errorf("lost %s in\n%s", h.Alias, view)
		}
	}
}

func TestNarrowStatusKeepsTheFailureReason(t *testing.T) {
	r := hostRow{status: "Unreachable (stale)"}
	if got := r.cell(column{label: "STATUS", width: 14}); got != "Unreachable" {
		t.Fatalf("narrow status = %q", got)
	}
}

func TestErrorClasses(t *testing.T) {
	for _, tc := range []struct{ code, message, want string }{
		{"ssh_error", "Permission denied (publickey).", "Auth denied"},
		{"ssh_error", "Host key verification failed.", "Host key error"},
		{"ssh_error", "Could not resolve hostname private.invalid", "DNS failure"},
		{"ssh_error", "connect to host port 22: Connection refused", "Conn refused"},
		{"ssh_error", "Network is unreachable", "Unreachable"},
		{"ssh_error", "Operation timed out", "Timed out"},
		{"timeout", "context deadline exceeded", "Timed out"},
		{"no_collector", "command not found", "No GPU tool"},
		{"collection_failed", "Traceback", "Probe failed"},
	} {
		if got := shortError(&failure{tc.code, tc.message}); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.message, got, tc.want)
		}
	}
}
