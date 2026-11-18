// Package collect queries GPU metrics over the user's OpenSSH client.
package collect

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/lyingloong/gpuglint/internal/hosts"
)

type GPU struct {
	Index          int      `json:"index"`
	Name           string   `json:"name"`
	UUID           string   `json:"uuid,omitempty"`
	UtilizationPct *float64 `json:"utilization_pct"`
	MemoryUsedMiB  *float64 `json:"memory_used_mib"`
	MemoryTotalMiB *float64 `json:"memory_total_mib"`
	TemperatureC   *float64 `json:"temperature_c"`
}

type Result struct {
	Backend string
	GPUs    []GPU
	Code    string
	Message string
}

//go:embed nvitop_probe.py
var pythonMetrics string

//go:embed interpreter.py
var pythonLauncher string

// Both pieces travel over stdin. Only the fixed "python3 -" or "python -"
// command crosses a remote shell. The source literal is generated once.
var pythonProbe = "metrics_source = " + strconv.Quote(pythonMetrics) + "\n" + pythonLauncher

type probe struct {
	backend string
	command string
	stdin   string
	parse   func([]byte) ([]GPU, error)
}

var probes = []probe{
	{"nvitop", "python3 -", pythonProbe, parseNvitop},
	{"nvitop", "python -", pythonProbe, parseNvitop},
	{"nvtop", "nvtop --snapshot", "", parseNvtop},
	{"nvidia-smi", "nvidia-smi --query-gpu=index,name,uuid,utilization.gpu,memory.used,memory.total,temperature.gpu --format=csv,noheader,nounits", "", parseSMI},
}

type output struct {
	stdout []byte
	detail string
	exit   int // -1 indicates a local failure to run ssh
}

// Poll uses a single host-wide deadline for all fallback attempts.
func Poll(ctx context.Context, host, config string) Result {
	if !hosts.ValidAlias(host) {
		return Result{Code: "invalid_host", Message: "invalid SSH alias"}
	}
	return poll(ctx, func(p probe) output { return runSSH(ctx, host, config, p) })
}

func poll(ctx context.Context, run func(probe) output) Result {
	var failures []string
	unavailable := true
	var empty Result
	for i, p := range probes {
		// Both Python names run the same probe. A working interpreter already
		// reporting zero NVIDIA devices does not need to be tried again.
		if i == 1 && empty.Backend == "nvitop" {
			continue
		}
		out := run(p)
		if err := ctx.Err(); err != nil {
			code := "timeout"
			if errors.Is(err, context.Canceled) {
				code = "cancelled"
			}
			return Result{Code: code, Message: err.Error()}
		}
		if out.exit == -1 || out.exit == 255 {
			return Result{Code: "ssh_error", Message: out.detail}
		}
		if p.backend == "nvidia-smi" && (out.exit == 0 || out.exit == 6) &&
			(strings.TrimSpace(string(out.stdout)) == "No devices were found" || out.detail == "No devices were found") {
			return Result{Backend: p.backend, GPUs: []GPU{}}
		}
		if out.exit == 0 {
			gpus, err := p.parse(out.stdout)
			if err == nil {
				result := Result{Backend: p.backend, GPUs: gpus}
				if len(gpus) != 0 {
					return result
				}
				if empty.Backend == "" {
					empty = result
				}
				continue // A different backend may see other GPU vendors.
			}
			out.detail = err.Error()
		}
		// POSIX shells and the Python probe use 127 for a missing tool.
		// Windows' default SSH shell reports missing commands with exit 1.
		lower := strings.ToLower(out.detail)
		if out.exit != 127 && !strings.Contains(lower, "is not recognized") &&
			!strings.Contains(lower, "is not recognised") {
			unavailable = false
		}
		failures = append(failures, fmt.Sprintf("%s: %s", strings.Fields(p.command)[0], out.detail))
	}
	if empty.Backend != "" {
		return empty
	}
	code := "collection_failed"
	if unavailable {
		code = "no_collector"
	}
	return Result{Code: code, Message: strings.Join(failures, "; ")}
}

// Keep draining both streams even after reaching the memory limit. nvtop
// snapshots can include process details; never include stdout in a diagnostic.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	keep := min(len(p), b.limit-b.buf.Len())
	b.buf.Write(p[:keep])
	b.truncated = b.truncated || keep < len(p)
	return len(p), nil
}

func runSSH(ctx context.Context, host, config string, p probe) output {
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "ClearAllForwardings=yes",
		"-o", "RemoteCommand=none"}
	if config != "" {
		args = append(args, "-F", config)
	}
	args = append(args, "--", host, p.command)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.WaitDelay = time.Second
	cmd.Stdin = strings.NewReader(p.stdin)
	stdout, stderr := &cappedBuffer{limit: 4 << 20}, &cappedBuffer{limit: 8 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.buf.String())
		if detail == "" {
			detail = err.Error()
		}
		var ee *exec.ExitError
		exit := -1
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		}
		return output{stdout: stdout.buf.Bytes(), detail: diagnostic(detail), exit: exit}
	}
	if stdout.truncated {
		return output{exit: 1, detail: "snapshot exceeds 4 MiB"}
	}
	return output{stdout: stdout.buf.Bytes(), detail: diagnostic(stderr.buf.String())}
}

func diagnostic(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	runes := []rune(s)
	if len(runes) > 300 {
		return "…" + string(runes[len(runes)-300:])
	}
	return s
}

func parseNvitop(data []byte) ([]GPU, error) {
	var rows []json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil || rows == nil {
		return nil, errors.New("invalid nvitop probe response")
	}
	devices := make([]GPU, 0, len(rows))
	for _, row := range rows {
		device := GPU{Index: -1}
		if err := json.Unmarshal(row, &device); err != nil || device.Index < 0 {
			return nil, errors.New("invalid nvitop device record")
		}
		devices = append(devices, device)
	}
	return devices, nil
}
