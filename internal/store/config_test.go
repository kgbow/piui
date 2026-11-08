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
