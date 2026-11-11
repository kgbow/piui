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

func TestKeychainSecretStoreScrubsPlaintextAPIKeyOnDisk(t *testing.T) {
	memSecrets := NewMemorySecretStore()
	SetSecretStore(memSecrets)
	t.Cleanup(func() { SetSecretStore(nil) })

	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	t.Setenv("PIUI_CONFIG_PATH", cfgFile)

	// Simulate legacy config file with plaintext api_key on disk.
	legacyJSON := `{
  "active_profile_id": "gw-1",
  "profiles": [
    {
      "id": "gw-1",
      "name": "My Gateway",
      "format": "openai-completions",
      "base_url": "https://api.example.com/v1",
      "api_key": "sk-super-secret-plaintext",
      "model_id": "gpt-4.1"
    }
  ]
}`
	if err := os.WriteFile(cfgFile, []byte(legacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := LoadOrInit(filepath.Join(dir, "proj"))
	if got := cfg.ActiveProfile().APIKey; got != "sk-super-secret-plaintext" {
		t.Fatalf("expected legacy APIKey loaded into memory, got %q", got)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify plaintext key is completely scrubbed from config.json on disk!
	rawDisk, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawDisk), "sk-super-secret-plaintext") {
		t.Fatalf("expected APIKey to be scrubbed from disk JSON, got:\n%s", string(rawDisk))
	}
	if !strings.Contains(string(rawDisk), `"keychain_stored": true`) {
		t.Fatalf("expected keychain_stored=true in disk JSON, got:\n%s", string(rawDisk))
	}

	// Reload from disk and verify SecretStore hydrates APIKey seamlessly.
	reloaded := LoadOrInit(filepath.Join(dir, "proj"))
	if got := reloaded.ActiveProfile().APIKey; got != "sk-super-secret-plaintext" {
		t.Fatalf("expected APIKey hydrated from keychain on reload, got %q", got)
	}
}

func TestWorkspaceSessionScopeAndUpsertManagement(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("PIUI_CONFIG_PATH", cfgFile)

	dirA := filepath.Join(t.TempDir(), "repo-a")
	dirB := filepath.Join(t.TempDir(), "repo-b")
	cfg := LoadOrInit(dirA)

	wtSSH1 := cfg.UpsertSSHTarget("dev@10.0.0.8", "/srv/backend", "Prod Backend", true)
	wtSSH2 := cfg.UpsertSSHTarget("dev@10.0.0.8", "/srv/frontend", "Prod Frontend", false)
	if wtSSH1.SessionScopeCwd() == wtSSH2.SessionScopeCwd() {
		t.Fatalf("expected distinct SessionScopeCwd for different remote dirs: %q vs %q", wtSSH1.SessionScopeCwd(), wtSSH2.SessionScopeCwd())
	}
	if !strings.HasPrefix(wtSSH1.SessionScopeCwd(), "/__piui_ssh__/") {
		t.Fatalf("unexpected SSH SessionScopeCwd: %q", wtSSH1.SessionScopeCwd())
	}

	// Upserting duplicate SSH host+dir selects existing target instead of duplicating.
	countBefore := len(cfg.Targets)
	dup := cfg.UpsertSSHTarget("dev@10.0.0.8", "/srv/backend/", "", true)
	if len(cfg.Targets) != countBefore || dup.ID != wtSSH1.ID {
		t.Fatalf("expected duplicate UpsertSSHTarget to reuse existing target, got %+v", dup)
	}

	// GUI startup (ExplicitStartupDir=false) preserves last active SSH target.
	cfg.SetTargetTrusted(wtSSH1.ID, true)
	cfg.SetSessionTitle("sess-123", "修复连接池泄露")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	guiLoaded := LoadOrInitWithOptions(LoadOptions{StartupDir: dirB, ExplicitStartupDir: false})
	if guiLoaded.ActiveTarget().ID != wtSSH1.ID || !guiLoaded.ActiveTarget().Trusted {
		t.Fatalf("expected GUI launch to preserve active trusted SSH target, got %+v", guiLoaded.ActiveTarget())
	}
	if got := guiLoaded.SessionTitle("sess-123", "默认标题"); got != "修复连接池泄露" {
		t.Fatalf("expected custom session title preserved, got %q", got)
	}
}

func TestMCPServerKeychainAndCommandParsing(t *testing.T) {
	cmd, args := ParseCommandLine(`npx -y @modelcontextprotocol/server-filesystem "/Users/dev/My Project"`)
	if cmd != "npx" || len(args) != 3 || args[2] != "/Users/dev/My Project" {
		t.Fatalf("unexpected ParseCommandLine result: cmd=%q args=%#v", cmd, args)
	}
	envMap := ParseKeyValuePairs("GITHUB_TOKEN=\"ghp_abc\"; LOG_LEVEL=debug")
	if envMap["GITHUB_TOKEN"] != "ghp_abc" || envMap["LOG_LEVEL"] != "debug" {
		t.Fatalf("unexpected ParseKeyValuePairs: %#v", envMap)
	}

	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	t.Setenv("PIUI_CONFIG_PATH", cfgFile)

	memSec := NewMemorySecretStore()
	SetSecretStore(memSec)
	t.Cleanup(func() { SetSecretStore(nil) })

	cfg := LoadOrInit(dir)
	added := cfg.UpsertMCPServer(MCPServerConfig{
		Name:        "internal-mcp",
		Transport:   MCPTransportHTTP,
		Enabled:     true,
		URL:         "https://mcp.internal.example/v1",
		BearerToken: "mcp-bearer-ultra-secret",
	})
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "mcp-bearer-ultra-secret") {
		t.Fatalf("expected MCP BearerToken scrubbed from config.json on disk, got:\n%s", string(raw))
	}

	reloaded := LoadOrInit(dir)
	if len(reloaded.MCPServers) != 1 || reloaded.MCPServers[0].BearerToken != "mcp-bearer-ultra-secret" {
		t.Fatalf("expected MCP BearerToken hydrated from SecretStore on reload, got %+v", reloaded.MCPServers)
	}
	if !reloaded.RemoveMCPServer(added.ID) {
		t.Fatal("expected RemoveMCPServer to return true")
	}
	if _, err := memSec.Get("mcp:" + added.ID); err == nil {
		t.Fatal("expected SecretStore entry deleted on RemoveMCPServer")
	}
}

func TestSSHConfigNeverAutoScannedAndLegacyAutoSSHTargetsPruned(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("PIUI_SESSION_DIR", "")
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("PIUI_CONFIG_PATH", cfgFile)
	t.Setenv("PIUI_DISABLE_KEYCHAIN", "1")

	// Create ~/.ssh/config with multiple Host aliases — must NEVER be auto-imported.
	sshDir := filepath.Join(fakeHome, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sshCfgContent := "Host auto-dev-box\n  HostName 10.0.1.10\n\nHost auto-prod-gw\n  HostName 10.0.1.20\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(sshCfgContent), 0o600); err != nil {
		t.Fatal(err)
	}

	projDir := filepath.Join(t.TempDir(), "my-local-repo")
	cfg := LoadOrInit(projDir)
	for _, wt := range cfg.Targets {
		if wt.IsRemote {
			t.Fatalf("expected zero auto-scanned SSH targets on fresh init, got %+v", cfg.Targets)
		}
	}

	// Simulate an existing config.json containing both a legacy auto-scanned SSH entry
	// (id="ssh-<host>", name="SSH: <host>", work_dir="~", trusted=false, 0 sessions)
	// and a user-created SSH workspace.
	withLegacyJSON := `{
  "active_profile_id": "anthropic-default",
  "active_target_id": "ssh-auto-dev-box",
  "targets": [
    {
      "id": "local-cwd",
      "name": "my-local-repo",
      "is_remote": false,
      "work_dir": "` + filepath.ToSlash(projDir) + `"
    },
    {
      "id": "ssh-auto-dev-box",
      "name": "SSH: auto-dev-box",
      "is_remote": true,
      "work_dir": "~",
      "ssh_host": "auto-dev-box"
    },
    {
      "id": "ssh-884120",
      "name": "rsrelay",
      "is_remote": true,
      "work_dir": "~",
      "ssh_host": "rsrelay"
    }
  ]
}`
	if err := os.WriteFile(cfgFile, []byte(withLegacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	reloaded := LoadOrInitWithOptions(LoadOptions{
		StartupDir:         projDir,
		ExplicitStartupDir: false,
	})
	for _, wt := range reloaded.Targets {
		if wt.ID == "ssh-auto-dev-box" {
			t.Fatalf("expected legacy auto-scanned ssh-auto-dev-box target to be pruned, got %+v", reloaded.Targets)
		}
	}

	foundManualSSH := false
	for _, wt := range reloaded.Targets {
		if wt.ID == "ssh-884120" && wt.IsRemote && wt.SSHHost == "rsrelay" {
			foundManualSSH = true
		}
	}
	if !foundManualSSH {
		t.Fatalf("expected manually added SSH workspace 'rsrelay' to be preserved, got %+v", reloaded.Targets)
	}
}
