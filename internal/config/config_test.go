package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		in, key, value string
		ok             bool
	}{
		{"KANSHI_PORT=8100", "KANSHI_PORT", "8100", true},
		{"  export KANSHI_ACCESS = lan,tailscale ", "KANSHI_ACCESS", "lan,tailscale", true},
		{`KANSHI_STORAGE_ROOTS="C:\,D:\"`, "KANSHI_STORAGE_ROOTS", `C:\,D:\`, true},
		{"KANSHI_ACCESS=local # only this machine", "KANSHI_ACCESS", "local", true},
		{"# KANSHI_PORT=1", "", "", false},
		{"", "", "", false},
		{"no equals sign", "", "", false},
	}
	for _, c := range cases {
		k, v, ok := parseLine(c.in)
		if k != c.key || v != c.value || ok != c.ok {
			t.Errorf("parseLine(%q) = %q, %q, %v; want %q, %q, %v", c.in, k, v, ok, c.key, c.value, c.ok)
		}
	}
}

func TestSaveValueKeepsTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", FileName)
	if err := SaveValue(path, "KANSHI_ACCESS", "lan"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("# my settings\nKANSHI_PORT=9000\nKANSHI_ACCESS=lan\nKANSHI_ACCESS=all\n"), 0o644)
	if err := SaveValue(path, "KANSHI_ACCESS", "tailscale"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := "# my settings\nKANSHI_PORT=9000\nKANSHI_ACCESS=tailscale\n# KANSHI_ACCESS=all\n"
	if string(got) != want {
		t.Errorf("file after save:\n%s\nwant:\n%s", got, want)
	}
	values, _ := ReadFile(path)
	if values["KANSHI_ACCESS"] != "tailscale" || values["KANSHI_PORT"] != "9000" {
		t.Errorf("read back %v", values)
	}
}

func TestLoadPrecedence(t *testing.T) {
	file := filepath.Join(t.TempDir(), FileName)
	os.WriteFile(file, []byte("KANSHI_ACCESS=lan\nKANSHI_PORT=9000\nKANSHI_POLL_INTERVAL=7\n"), 0o644)
	for _, k := range []string{"KANSHI_ACCESS", "KANSHI_HOST", "KANSHI_PORT", "KANSHI_POLL_INTERVAL", "KANSHI_CONFIG", "DOCKER_HOST", "KANSHI_DOCKER_SOCKET"} {
		t.Setenv(k, "")
	}

	cfg := Load(Flags{File: file})
	if cfg.Access != "lan" || cfg.AccessSource != SourceFile || cfg.Port != 9000 || cfg.PollInterval.Seconds() != 7 {
		t.Errorf("file only: access=%q (%s) port=%d poll=%v", cfg.Access, cfg.AccessSource, cfg.Port, cfg.PollInterval)
	}

	t.Setenv("KANSHI_HOST", "0.0.0.0")
	cfg = Load(Flags{File: file})
	if cfg.Access != "all" || cfg.AccessSource != SourceEnv {
		t.Errorf("legacy KANSHI_HOST in env should beat the file: %q (%s)", cfg.Access, cfg.AccessSource)
	}

	t.Setenv("KANSHI_ACCESS", "tailscale")
	cfg = Load(Flags{File: file})
	if cfg.Access != "tailscale" || cfg.AccessSource != SourceEnv {
		t.Errorf("KANSHI_ACCESS should beat KANSHI_HOST: %q (%s)", cfg.Access, cfg.AccessSource)
	}

	cfg = Load(Flags{File: file, Access: "local", Port: 8200})
	if cfg.Access != "local" || cfg.AccessSource != SourceFlag || cfg.Port != 8200 {
		t.Errorf("flags should win: %q (%s) port %d", cfg.Access, cfg.AccessSource, cfg.Port)
	}
}

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{"KANSHI_ACCESS", "KANSHI_HOST", "KANSHI_STORAGE_ROOTS", "DOCKER_HOST", "KANSHI_DOCKER_SOCKET"} {
		t.Setenv(k, "")
	}
	cfg := Load(Flags{File: filepath.Join(t.TempDir(), "missing.env")})
	if cfg.Access != "local" || cfg.AccessSource != SourceDefault {
		t.Errorf("default access = %q (%s), want local", cfg.Access, cfg.AccessSource)
	}
	if len(cfg.StorageRoots) != 1 || cfg.StorageRoots[0] != "auto" {
		t.Errorf("default roots = %v", cfg.StorageRoots)
	}
	if !strings.Contains(cfg.DockerHost, "://") {
		t.Errorf("default docker host %q has no scheme", cfg.DockerHost)
	}
	if cfg.FileLoaded {
		t.Error("a missing file must not count as loaded")
	}

	t.Setenv("KANSHI_DOCKER_SOCKET", "/run/docker.sock")
	if got := Load(Flags{}).DockerHost; got != "unix:///run/docker.sock" {
		t.Errorf("legacy socket path became %q", got)
	}
}

func TestWritable(t *testing.T) {
	if !Writable(filepath.Join(t.TempDir(), "new", FileName)) {
		t.Error("a fresh temp directory should be writable")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o644)
	if Writable(filepath.Join(blocker, "sub", FileName)) {
		t.Error("a path under a regular file cannot be written")
	}
}
