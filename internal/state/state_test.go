package state

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".gpuglint")
	path := filepath.Join(dir, "state")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := Read(path); !errors.Is(err, ErrInactive) {
		t.Fatalf("unpublished snapshot: %v", err)
	}
	if other, err := Open(path); err == nil {
		other.Close()
		t.Fatal("second writer acquired live state")
	}
	payload := []byte(`{"hosts":[]}`)
	if err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if !bytes.Equal(got, payload) || err != nil {
		t.Fatalf("Read = %q, %v", got, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new directory was not removed: %v", err)
	}
	if _, err := Read(path); !errors.Is(err, ErrInactive) {
		t.Fatalf("stopped writer: %v", err)
	}
	if err := w.Write(payload); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
}

func TestGuardExistingFiles(t *testing.T) {
	for _, suffix := range []string{"", ".tmp", ".lock"} {
		t.Run("existing"+suffix, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state")
			target := path + suffix
			if err := os.WriteFile(target, []byte("unrelated"), 0600); err != nil {
				t.Fatal(err)
			}
			if w, err := Open(path); err == nil {
				w.Close()
				t.Fatal("overwrote existing file")
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != "unrelated" {
				t.Fatalf("original content lost: %q, %v", got, err)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatalf("artifacts left behind: %v, %v", files, err)
			}
		})
	}
}

func TestConcurrentSnapshots(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	a, b := strings.Repeat("a", 65536), strings.Repeat("b", 32768)
	if err := w.Write([]byte(a)); err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := Read(path)
				if err != nil || (string(got) != a && string(got) != b) {
					t.Errorf("torn or unreadable snapshot: bytes=%d, err=%v", len(got), err)
					return
				}
			}
		}()
	}
	for i := 0; i < 24; i++ {
		if err := w.Write([]byte([]string{a, b}[i%2])); err != nil {
			t.Error(err)
			break
		}
	}
	close(stop)
	readers.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("expected an empty pre-existing directory, got %v, %v", entries, err)
	}
}

func TestCrashRecovery(t *testing.T) {
	if path := os.Getenv("GPUGLINT_TEST_CRASH_PATH"); path != "" {
		w, err := Open(path)
		if err == nil {
			err = w.Write([]byte("crashed"))
		}
		if err != nil {
			t.Fatal(err)
		}
		// Model a process dying during its next write.
		if err := os.WriteFile(path+".tmp", []byte("partial"), 0600); err != nil {
			t.Fatal(err)
		}
		fmt.Println("ready")
		time.Sleep(time.Minute)
		return
	}
	dir := filepath.Join(t.TempDir(), ".gpuglint")
	path := filepath.Join(dir, "state")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashRecovery$")
	cmd.Env = append(os.Environ(), "GPUGLINT_TEST_CRASH_PATH="+path)
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(output).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != "ready" {
			t.Fatalf("unexpected helper output: %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not publish")
	}
	if other, err := Open(path); err == nil {
		other.Close()
		t.Fatal("acquired another process's live state")
	}
	if got, err := Read(path); string(got) != "crashed" || err != nil {
		t.Fatalf("live helper: %q, %v", got, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	if _, err := Read(path); !errors.Is(err, ErrInactive) {
		t.Fatalf("returned abandoned state: %v", err)
	}
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Write([]byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crash artifacts still present: %v", err)
	}
}
