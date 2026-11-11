package sysenv

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// HydrateLoginEnvironment ensures that when PiUI is launched as a desktop GUI app
// (e.g. macOS .app bundle or Linux desktop launcher where PATH is minimal and lacks
// Homebrew, Cargo, Go, Node, or user SSH_AUTH_SOCK), key environment variables from
// the user's login shell are merged into the current process environment.
func HydrateLoginEnvironment() {
	if runtime.GOOS == "windows" {
		return
	}

	// Always ensure common Unix developer binary paths are present at minimum,
	// even if login shell probing times out or fails.
	ensureFallbackPaths()

	// Skip spawning a login shell if we already appear to be running inside an
	// interactive terminal session with a rich PATH and TERM set.
	if os.Getenv("TERM") != "" && os.Getenv("SHLVL") != "" && strings.Contains(os.Getenv("PATH"), "local") {
		return
	}

	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		if runtime.GOOS == "darwin" {
			shell = "/bin/zsh"
		} else {
			shell = "/bin/bash"
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	// Use -lc (login, non-interactive) first to avoid blocking on interactive TUI prompts in .zshrc,
	// while still loading /etc/zprofile, ~/.zprofile, ~/.bash_profile, etc.
	envMap, err := probeShellEnvNull(ctx, shell, "-lc")
	if err != nil || len(envMap) == 0 {
		return
	}
	applyHydratedEnv(envMap)
	ensureFallbackPaths()
}

func probeShellEnvNull(ctx context.Context, shell, flag string) (map[string]string, error) {
	// Print a sentinel before env -0 so any noisy banner printed by profile scripts is ignored.
	const marker = "__PIUI_ENV_START__\x00"
	script := `printf '__PIUI_ENV_START__\0' && env -0`
	cmd := exec.CommandContext(ctx, shell, flag, script)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return parseNullEnvOutput(stdout.Bytes(), []byte(marker)), nil
}

func parseNullEnvOutput(raw, marker []byte) map[string]string {
	if idx := bytes.Index(raw, marker); idx >= 0 {
		raw = raw[idx+len(marker):]
	}
	out := make(map[string]string)
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		eq := bytes.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		k := string(entry[:eq])
		v := string(entry[eq+1:])
		out[k] = v
	}
	return out
}

// allowedHydrateKeys restricts which variables are imported from the login shell,
// avoiding clobbering process-specific runtime flags while restoring developer toolchains and auth.
func shouldHydrateKey(k string) bool {
	switch k {
	case "PATH", "SSH_AUTH_SOCK", "LANG", "LC_ALL", "LC_CTYPE",
		"GOPATH", "GOROOT", "CARGO_HOME", "RUSTUP_HOME", "NVM_DIR", "VOLTA_HOME", "PNPM_HOME", "BUN_INSTALL",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"OPENAI_API_KEY", "OPENAI_BASE_URL",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy":
		return true
	}
	return false
}

func applyHydratedEnv(shellEnv map[string]string) {
	if shellPath := strings.TrimSpace(shellEnv["PATH"]); shellPath != "" {
		merged := mergePathLists(shellPath, os.Getenv("PATH"))
		_ = os.Setenv("PATH", merged)
	}
	for k, v := range shellEnv {
		if k == "PATH" || !shouldHydrateKey(k) || strings.TrimSpace(v) == "" {
			continue
		}
		if strings.TrimSpace(os.Getenv(k)) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

func ensureFallbackPaths() {
	var extra []string
	if runtime.GOOS == "darwin" {
		extra = append(extra, "/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin")
	} else {
		extra = append(extra, "/usr/local/bin")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		extra = append(extra,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "go", "bin"),
			filepath.Join(home, ".cargo", "bin"),
		)
	}
	extra = append(extra, "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	merged := mergePathLists(os.Getenv("PATH"), strings.Join(extra, string(os.PathListSeparator)))
	_ = os.Setenv("PATH", merged)
}

func mergePathLists(primary, secondary string) string {
	seen := make(map[string]bool)
	var parts []string
	addAll := func(raw string) {
		for _, item := range filepath.SplitList(raw) {
			item = strings.TrimSpace(item)
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			parts = append(parts, item)
		}
	}
	addAll(primary)
	addAll(secondary)
	return strings.Join(parts, string(os.PathListSeparator))
}

// ResolveStartupWorkDir resolves an optional CLI directory argument or the process working
// directory, rejecting root "/" (which macOS LaunchServices sets for GUI .app bundles unless
// explicitly passed on the command line).
func ResolveStartupWorkDir(args []string) (dir string, explicit bool) {
	for _, arg := range args {
		a := strings.TrimSpace(arg)
		if a == "" || strings.HasPrefix(a, "-") {
			continue
		}
		// Ignore macOS LaunchServices ProcessSerialNumber argument (-psn_0_...)
		if strings.HasPrefix(a, "-psn_") {
			continue
		}
		if strings.HasPrefix(a, "~/") || a == "~" {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				if a == "~" {
					a = home
				} else {
					a = filepath.Join(home, a[2:])
				}
			}
		}
		if abs, err := filepath.Abs(a); err == nil {
			if st, statErr := os.Stat(abs); statErr == nil {
				if !st.IsDir() {
					abs = filepath.Dir(abs)
				}
				return abs, true
			}
		}
	}

	cwd, err := os.Getwd()
	if err == nil && cwd != "" && cwd != "/" {
		if abs, absErr := filepath.Abs(cwd); absErr == nil && abs != "/" {
			return abs, false
		}
	}
	if home, hErr := os.UserHomeDir(); hErr == nil && home != "" {
		return home, false
	}
	return ".", false
}
