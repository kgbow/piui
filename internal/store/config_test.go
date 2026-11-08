package store

import (
	"path/filepath"
	"testing"
)

func TestLoadOrInitUpdatesLocalCwdAcrossDirectories(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("PIUI_CONFIG_PATH", cfgFile)

	dirA := filepath.Join(t.TempDir(), "proj-a")
	dirB := filepath.Join(t.TempDir(), "proj-b")
	dirCustom := filepath.Join(t.TempDir(), "proj-custom")

	cfg1 := LoadOrInit(dirA)
	if got := cfg1.ActiveTarget().WorkDir; got != dirA {
		t.Fatalf("expected initial local-cwd WorkDir=%q, got %q", dirA, got)
	}

	// Add a secondary custom local target and save.
	cfg1.Targets = append(cfg1.Targets, WorkspaceTarget{
		ID:       "target-custom",
		Name:     "Custom Local",
		IsRemote: false,
		WorkDir:  dirCustom,
	})
	if err := cfg1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Launching PiUI from dirB should update local-cwd to dirB while preserving target-custom.
	cfg2 := LoadOrInit(dirB)
	if got := cfg2.ActiveTarget().WorkDir; got != dirB {
		t.Fatalf("expected updated local-cwd WorkDir=%q, got %q", dirB, got)
	}

	foundCustom := false
	for _, wt := range cfg2.Targets {
		if wt.ID == "target-custom" && wt.WorkDir == dirCustom {
			foundCustom = true
			break
		}
	}
	if !foundCustom {
		t.Fatalf("expected custom target to be preserved in %+v", cfg2.Targets)
	}
}

func TestThemeAndTargetBadges(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("PIUI_CONFIG_PATH", cfgFile)

	cfg := LoadOrInit(filepath.Join(t.TempDir(), "piui"))
	if cfg.IsLightTheme() {
		t.Fatalf("expected default theme to be dark, got %q", cfg.Theme)
	}
	if next := cfg.ToggleTheme(); next != ThemeLight || !cfg.IsLightTheme() {
		t.Fatalf("expected ToggleTheme to switch to light, got %q", next)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := LoadOrInit(filepath.Join(t.TempDir(), "piui"))
	if !reloaded.IsLightTheme() {
		t.Fatalf("expected persisted light theme, got %q", reloaded.Theme)
	}

	localWT := reloaded.ActiveTarget()
	if got := localWT.ShortName(); got != "piui" {
		t.Fatalf("expected local ShortName='piui', got %q", got)
	}
	if got := localWT.TurnBadge(); got != "piui (Local)" {
		t.Fatalf("expected TurnBadge='piui (Local)', got %q", got)
	}
	if got := localWT.ToolCardBadge(); got != "Local · piui" {
		t.Fatalf("expected ToolCardBadge='Local · piui', got %q", got)
	}

	sshWT := WorkspaceTarget{
		ID:       "ssh-prod-gw",
		Name:     "SSH: prod-gateway",
		IsRemote: true,
		WorkDir:  "/srv/gateway",
		SSHHost:  "root@10.24.8.19",
	}
	if got := sshWT.ShortName(); got != "prod-gateway" {
		t.Fatalf("expected ssh ShortName='prod-gateway', got %q", got)
	}
	if got := sshWT.DisplaySub(); got != "root@10.24.8.19:/srv/gateway" {
		t.Fatalf("expected DisplaySub='root@10.24.8.19:/srv/gateway', got %q", got)
	}
	if got := sshWT.TurnBadge(); got != "prod-gateway (SSH)" {
		t.Fatalf("expected TurnBadge='prod-gateway (SSH)', got %q", got)
	}
	if got := sshWT.ToolCardBadge(); got != "SSH · prod-gateway" {
		t.Fatalf("expected ToolCardBadge='SSH · prod-gateway', got %q", got)
	}
}
