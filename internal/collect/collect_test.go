package collect

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBackendFixtures(t *testing.T) {
	fixtures := []struct {
		backend string
		data    string
		parse   func([]byte) ([]GPU, error)
	}{
		{"nvitop", `[{"index":0,"name":"GPU #0","uuid":"GPU-uuid","memory_used_mib":4,"memory_total_mib":8192,"temperature_c":null,"utilization_pct":0}]`, parseNvitop},
		{"nvtop", `[{"device_name":"GPU #0","mem_used":"4194304","mem_total":"8589934592","gpu_util":"0%","temp":null,"processes":[]}]`, parseNvtop},
		{"nvidia-smi", "0, GPU #0, GPU-uuid, 0, 4, 8192, [N/A]\r\n", parseSMI},
	}
	for _, f := range fixtures {
		t.Run(f.backend, func(t *testing.T) {
			devices, err := f.parse([]byte(f.data))
			if err != nil || len(devices) != 1 {
				t.Fatalf("parse returned %v, %v", devices, err)
			}
			d := devices[0]
			if d.Name != "GPU #0" || d.TemperatureC != nil || *d.UtilizationPct != 0 ||
				*d.MemoryUsedMiB != 4 || *d.MemoryTotalMiB != 8192 {
				t.Fatalf("unexpected metrics: %+v", d)
			}
		})
	}
}

func TestNvtopOlderSnapshot(t *testing.T) {
	d, err := parseNvtop([]byte(`[{"device_name":"A","mem_util":"80%","gpu_util":null,"temp":"86F"},{"device_name":null}]`))
	if err != nil || len(d) != 2 || d[1].Index != 1 || d[0].MemoryUsedMiB != nil ||
		d[0].MemoryTotalMiB != nil || d[0].UtilizationPct != nil || *d[0].TemperatureC != 30 {
		t.Fatalf("unexpected result: %+v, %v", d, err)
	}
}

func TestNvitopMalformedDevice(t *testing.T) {
	for _, fixture := range []string{"null", "[null]", "[{}]", `[{"index":null}]`} {
		if _, err := parseNvitop([]byte(fixture)); err == nil {
			t.Errorf("accepted device without index: %s", fixture)
		}
	}
}

func TestFallback(t *testing.T) {
	var attempts []string
	result := poll(context.Background(), func(p probe) output {
		attempts = append(attempts, p.backend)
		switch p.backend {
		case "nvitop":
			return output{exit: 127, detail: "not installed"}
		case "nvtop":
			return output{stdout: []byte("not valid JSON")}
		default:
			return output{stdout: []byte("1, Card, GPU-a, [Not Supported], 0, 128, 29\n")}
		}
	})
	if !reflect.DeepEqual(attempts, []string{"nvitop", "nvitop", "nvtop", "nvidia-smi"}) ||
		result.Code != "" || result.Backend != "nvidia-smi" || result.GPUs[0].UtilizationPct != nil {
		t.Fatalf("unexpected fallback: %v %+v", attempts, result)
	}
}

func TestNoGPUAndFailureAreDistinct(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		run                   func(probe) output
		wantCode, wantBackend string
	}{
		{"empty", func(p probe) output {
			if p.backend == "nvtop" {
				return output{stdout: []byte("No GPU to monitor.\n")}
			}
			return output{exit: 127}
		}, "", "nvtop"},
		{"no tools", func(probe) output { return output{exit: 127} }, "no_collector", ""},
		{"broken tools", func(probe) output { return output{exit: 1, detail: "driver error"} }, "collection_failed", ""},
		{"unreadable", func(probe) output { return output{stdout: []byte("not metrics")} }, "collection_failed", ""},
		{"no NVIDIA hardware", func(p probe) output {
			if p.backend == "nvidia-smi" {
				return output{exit: 6, stdout: []byte("No devices were found\n")}
			}
			return output{exit: 127}
		}, "", "nvidia-smi"},
		{"amd only", func(p probe) output {
			if p.backend == "nvitop" {
				return output{stdout: []byte("[]")}
			}
			return output{stdout: []byte(`[{"device_name":"AMD Radeon","temp":"55C"}]`)}
		}, "", "nvtop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := poll(context.Background(), tc.run)
			if r.Backend != tc.wantBackend || r.Code != tc.wantCode {
				t.Fatalf("unexpected result %+v", r)
			}
		})
	}
}

func TestSSHFailureStopsProbing(t *testing.T) {
	n := 0
	r := poll(context.Background(), func(probe) output {
		n++
		return output{exit: 255, detail: "unreachable"}
	})
	if n != 1 || r.Code != "ssh_error" || r.Message != "unreachable" {
		t.Fatalf("unexpected attempts %d, result %+v", n, r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	r = poll(ctx, func(probe) output { return output{exit: -1} })
	if r.Code != "timeout" {
		t.Fatal(r)
	}
	if got := Poll(context.Background(), "-oProxyCommand=oops", ""); got.Code != "invalid_host" {
		t.Fatal(got)
	}
}

func TestSMILive(t *testing.T) {
	if os.Getenv("GPUGLINT_TEST_LOCAL_SMI") != "1" {
		t.Skip("set GPUGLINT_TEST_LOCAL_SMI=1 on a machine with NVIDIA hardware")
	}
	args := strings.Fields(probes[len(probes)-1].command)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, args[0], args[1:]...).Output()
	if err != nil {
		t.Fatal(err)
	}
	devices, err := parseSMI(data)
	if err != nil || len(devices) == 0 {
		t.Fatalf("live probe: %d GPU(s), %v", len(devices), err)
	}
	for _, d := range devices {
		t.Logf("GPU %d: %s", d.Index, d.Name)
	}
}

func TestPythonFindsLauncherVenv(t *testing.T) {
	name, bin, exe := "python3", "bin", "nvitop"
	if runtime.GOOS == "windows" {
		name, bin, exe = "python", "Scripts", "nvitop.exe"
	}
	python, err := exec.LookPath(name)
	if err != nil {
		t.Skip("Python is needed to exercise remote venv discovery")
	}
	root := t.TempDir()
	venv := filepath.Join(root, "tool env")
	build := exec.Command(python, "-m", "venv", "--without-pip", venv)
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("no usable Python venv: %v: %s", err, out)
	}
	toolDir := filepath.Join(venv, bin)
	if err := os.WriteFile(filepath.Join(toolDir, exe), nil, 0755); err != nil {
		t.Fatal(err)
	}
	// pipx on POSIX exposes a symlink on PATH outside the environment.
	pathDir := toolDir
	if runtime.GOOS != "windows" {
		pathDir = root
		if err := os.Symlink(filepath.Join(toolDir, exe), filepath.Join(pathDir, exe)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(python, "-I", "-S", "-")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	source := "import sys; print(sys.prefix)"
	cmd.Stdin = strings.NewReader("metrics_source = " + strconv.Quote(source) + "\n" + pythonLauncher)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cannot run interpreter: %v, %q", err, out)
	}
	actual, err := os.Stat(strings.TrimSpace(string(out)))
	expected, statErr := os.Stat(venv)
	if err != nil || statErr != nil || !os.SameFile(actual, expected) {
		t.Fatalf("wanted interpreter prefix %q, got %q", venv, out)
	}
}
