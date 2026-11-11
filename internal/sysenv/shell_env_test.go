package sysenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseNullEnvOutputIgnoresBanner(t *testing.T) {
	raw := []byte("Welcome to fish/zsh banner\n__PIUI_ENV_START__\x00PATH=/opt/homebrew/bin:/usr/bin\x00ANTHROPIC_API_KEY=sk-test\x00MULTI=line1\nline2\x00")
	got := parseNullEnvOutput(raw, []byte("__PIUI_ENV_START__\x00"))
	if got["PATH"] != "/opt/homebrew/bin:/usr/bin" {
		t.Fatalf("unexpected PATH: %q", got["PATH"])
	}
	if got["ANTHROPIC_API_KEY"] != "sk-test" {
		t.Fatalf("unexpected ANTHROPIC_API_KEY: %q", got["ANTHROPIC_API_KEY"])
	}
	if got["MULTI"] != "line1\nline2" {
		t.Fatalf("unexpected multiline value: %q", got["MULTI"])
	}
}

func TestMergePathListsDeduplicatesPreservingOrder(t *testing.T) {
	sep := string(os.PathListSeparator)
	p1 := strings.Join([]string{"/opt/homebrew/bin", "/usr/bin"}, sep)
	p2 := strings.Join([]string{"/usr/bin", "/usr/local/bin", "/bin"}, sep)
	got := mergePathLists(p1, p2)
	want := strings.Join([]string{"/opt/homebrew/bin", "/usr/bin", "/usr/local/bin", "/bin"}, sep)
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveStartupWorkDirExplicitFileAndDir(t *testing.T) {
	tmp := t.TempDir()
	subFile := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(subFile, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, explicit := ResolveStartupWorkDir([]string{"--flag", tmp})
	if !explicit || dir != tmp {
		t.Fatalf("expected explicit dir %q, got %q (explicit=%v)", tmp, dir, explicit)
	}

	// Passing a file path resolves to its parent directory.
	dir2, explicit2 := ResolveStartupWorkDir([]string{subFile})
	if !explicit2 || dir2 != tmp {
		t.Fatalf("expected parent dir %q for file arg, got %q (explicit=%v)", tmp, dir2, explicit2)
	}
}
