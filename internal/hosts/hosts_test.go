package hosts

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

func put(t *testing.T, dir, name, contents string) string {
	t.Helper()
	filename := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestDiscovery(t *testing.T) {
	home := fixture(t)
	put(t, home, ".ssh/config", `
Include = "parts/a b/*.conf" # sorted before the main file's hosts
HOST=first "训练机" first bad !bad -* !x !y
  HostName target.example.com
Host gpu-* !gpu-old
  Include ${PARTS}
Match exec "some condition"
  Include "disabled.conf"
Match all
  Include extra\ file.conf
Host *
  ProxyJump jump
`)
	put(t, home, ".ssh/parts/a b/10.conf", "Host shared first\n")
	put(t, home, ".ssh/parts/a b/20.conf", "Host second shared\n")
	put(t, home, ".ssh/parts/a b/.backup.conf", "Host old-configuration\n")
	put(t, home, ".ssh/conditional", "Host gpu-1 gpu-old unrelated\n")
	put(t, home, ".ssh/disabled.conf", "Host not-applicable\n")
	put(t, home, ".ssh/extra file.conf", "Host last\n")
	t.Setenv("PARTS", filepath.Join(home, ".ssh", "conditional"))

	got, err := Select("", nil, nil)
	want := []string{"shared", "first", "second", "训练机", "gpu-1", "last"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("Select() = %v, %v; want %v", got, err, want)
	}
}

func TestSelectExplicitAndPatterns(t *testing.T) {
	home := fixture(t)
	cfg := put(t, home, "custom/config", "Include glob/*.conf\nHost gpu-*\n")
	put(t, home, ".ssh/glob/hosts.conf", "Host gpu-01 gpu-02 gpu-10 cpu-01\n")
	got, err := Select(cfg, []string{"gpu-0?", "gpu-17", "gpu-17"}, []string{"*-02"})
	want := []string{"gpu-01", "gpu-17"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("Select() = %v, %v; want %v", got, err, want)
	}
}

func TestMissingConfig(t *testing.T) {
	home := fixture(t)
	if got, err := Select("", []string{"127.0.0.1"}, nil); err != nil || !slices.Equal(got, []string{"127.0.0.1"}) {
		t.Fatalf("explicit host with default config absent: %v, %v", got, err)
	}
	if _, err := Select(filepath.Join(home, "typo"), []string{"gpu-01"}, nil); err == nil {
		t.Fatal("missing custom config accepted")
	}
}

func TestUnsafeAliases(t *testing.T) {
	for _, bad := range []string{"-V", "@host", "", "bad host", "../host", "x\r\nhost", "end;echo"} {
		if _, err := Select("", []string{bad}, nil); err == nil {
			t.Errorf("accepted unsafe selector %q", bad)
		}
	}
	for _, good := range []string{"::1", "fe80::1%eth0", "my_server.2", "卡-1"} {
		if !ValidAlias(good) {
			t.Errorf("rejected valid name %q", good)
		}
	}
}

func TestInvalidConfig(t *testing.T) {
	home := fixture(t)
	put(t, home, ".ssh/config", "Include ~/sub/conf\n")
	put(t, home, "sub/conf", "Include %d/sub/conf\n")
	if _, err := Select("", nil, nil); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("recursive include error = %v", err)
	}
	put(t, home, ".ssh/config", "User example\nHost \"unterminated\n")
	if _, err := Select("", nil, nil); err == nil || !strings.Contains(err.Error(), ":2:") {
		t.Fatalf("unclosed quote error = %v", err)
	}
}
