package store

import (
	"os"
	"path/filepath"
	"strings"
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


func TestCorruptConfigBackedUpWithoutOverwriting(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	t.Setenv("PIUI_CONFIG_PATH", cfgFile)

	rawCorrupt := []byte(`{"active_profile_id":"openai","profiles":[broken-json`)
	if err := os.WriteFile(cfgFile, rawCorrupt, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := LoadOrInit(filepath.Join(dir, "workspace"))
	if cfg.LoadWarning == "" {
		t.Fatal("expected LoadWarning for corrupted config.json, got empty")
	}
	if cfg.BackupPath == "" {
		t.Fatal("expected BackupPath to be created for corrupted config.json")
	}

	// Original corrupted file must NOT be silently overwritten with defaults.
	diskBytes, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("ReadFile original: %v", err)
	}
	if string(diskBytes) != string(rawCorrupt) {
		t.Fatalf("expected corrupted config.json to remain untouched, got %q", string(diskBytes))
	}

	// Backup file must match raw bytes and carry 0600 permissions.
	bakInfo, err := os.Stat(cfg.BackupPath)
	if err != nil {
		t.Fatalf("Stat backup: %v", err)
	}
	if perm := bakInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected backup perm 0600, got %o", perm)
	}
	bakBytes, err := os.ReadFile(cfg.BackupPath)
	if err != nil {
		t.Fatalf("ReadFile backup: %v", err)
	}
	if string(bakBytes) != string(rawCorrupt) {
		t.Fatalf("expected backup content to equal rawCorrupt, got %q", string(bakBytes))
	}
}

func TestEmptyProfilesPreservesTargetsPolicyAndTheme(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	t.Setenv("PIUI_CONFIG_PATH", cfgFile)

	validEmptyProfiles := `{
  "active_profile_id": "",
  "profiles": [],
  "active_target_id": "ssh-staging",
  "targets": [
    {
      "id": "ssh-staging",
      "name": "SSH: staging",
      "is_remote": true,
      "work_dir": "/srv/staging",
      "ssh_host": "deploy@staging"
    }
  ],
  "safety_policy": "strict-remote",
  "theme": "light"
}`
	if err := os.WriteFile(cfgFile, []byte(validEmptyProfiles), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := LoadOrInit(filepath.Join(dir, "workspace"))
	if cfg.LoadWarning != "" {
		t.Fatalf("unexpected LoadWarning for valid JSON with empty profiles: %q", cfg.LoadWarning)
	}
	if len(cfg.Profiles) == 0 {
		t.Fatal("expected default profiles to be populated when profiles array is empty")
	}
	if cfg.SafetyPolicy != SafetyPolicyStrictRemote {
		t.Fatalf("expected SafetyPolicyStrictRemote preserved, got %q", cfg.SafetyPolicy)
	}
	if !cfg.IsLightTheme() {
		t.Fatalf("expected ThemeLight preserved, got %q", cfg.Theme)
	}
	if got := cfg.ActiveTarget(); got.ID != "ssh-staging" || got.SSHHost != "deploy@staging" {
		t.Fatalf("expected ssh-staging target preserved, got %+v", got)
	}
}

func TestAtomicSavePermissionsAndNoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	t.Setenv("PIUI_CONFIG_PATH", cfgFile)

	cfg := LoadOrInit(filepath.Join(dir, "workspace"))
	cfg.SafetyPolicy = SafetyPolicyAlwaysHighRisk
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(cfgFile)
	if err != nil {
		t.Fatalf("Stat config.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected 0600 permissions on saved config.json, got %o", perm)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".piui-config-") {
			t.Fatalf("unexpected leftover temp file after atomic Save: %s", ent.Name())
		}
	}
}
