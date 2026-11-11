package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"piui/internal/store"
)

func TestRemoteBootstrapDirectoriesAndProcessGroupCleanup(t *testing.T) {
	installFakeSSH(t)

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	if err := os.MkdirAll(filepath.Join(fakeHome, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".pi", "agent", "AGENTS.md"), []byte("Global remote rule"), 0o644); err != nil {
		t.Fatal(err)
	}

	remoteRepo := filepath.Join(t.TempDir(), "srv-gateway")
	if err := os.MkdirAll(filepath.Join(remoteRepo, ".pi", "skills", "k8s-rollout"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(remoteRepo, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(remoteRepo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteRepo, "AGENTS.md"), []byte("Repo-specific remote rule"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(remoteRepo, ".pi", "skills", "k8s-rollout", "SKILL.md"),
		[]byte("---\nname: k8s-rollout\ndescription: Safe canary rollout on cluster\n---\n# Rollout\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	// 1. Untrusted bootstrap reports HasProjectSkills=true, includes global AGENTS.md, and withholds repo AGENTS.md + .pi/skills.
	untrustedBoot := LoadRemoteWorkspaceBootstrap(context.Background(), "deploy@prod", remoteRepo, false)
	if !untrustedBoot.Status.Connected || !untrustedBoot.Status.HasProjectSkills {
		t.Fatalf("expected connected untrusted bootstrap with HasProjectSkills=true, got %+v", untrustedBoot.Status)
	}
	if len(untrustedBoot.Skills) != 0 {
		t.Fatalf("expected 0 skills when untrusted, got %+v", untrustedBoot.Skills)
	}
	if len(untrustedBoot.ContextFiles) != 1 || !strings.Contains(untrustedBoot.ContextFiles[0].Content, "Global remote rule") {
		t.Fatalf("expected only global AGENTS.md when untrusted, got %+v", untrustedBoot.ContextFiles)
	}

	// 2. Trusted bootstrap loads both repo AGENTS.md and .pi/skills with real remote POSIX paths.
	trustedBoot := LoadRemoteWorkspaceBootstrap(context.Background(), "deploy@prod", remoteRepo, true)
	if len(trustedBoot.Skills) != 1 || trustedBoot.Skills[0].Name != "k8s-rollout" {
		t.Fatalf("expected k8s-rollout skill when trusted, got %+v", trustedBoot.Skills)
	}
	if strings.HasPrefix(trustedBoot.Skills[0].FilePath, "ssh://") || !strings.HasSuffix(trustedBoot.Skills[0].FilePath, "/.pi/skills/k8s-rollout/SKILL.md") {
		t.Fatalf("expected authentic remote POSIX skill path, got %q", trustedBoot.Skills[0].FilePath)
	}
	if len(trustedBoot.ContextFiles) != 2 {
		t.Fatalf("expected global + repo AGENTS.md when trusted, got %+v", trustedBoot.ContextFiles)
	}

	// 3. ListRemoteDirectories lists cmd and .pi while hiding .git.
	resolvedDir, dirs, err := ListRemoteDirectories(context.Background(), "deploy@prod", remoteRepo)
	if err != nil {
		t.Fatalf("ListRemoteDirectories: %v", err)
	}
	if resolvedDir == "" || len(dirs) != 2 || dirs[0] != "cmd" || dirs[1] != ".pi" {
		t.Fatalf("expected resolvedDir and [cmd .pi], got resolved=%q dirs=%+v", resolvedDir, dirs)
	}

	// 4. Remote process-group cleanup on context cancellation kills backgrounded child processes.
	pidFile := filepath.Join(t.TempDir(), "remote-child.pid")
	router := NewEnvironmentRouter(store.WorkspaceTarget{
		ID:       "ssh-prod",
		Name:     "prod",
		IsRemote: true,
		WorkDir:  remoteRepo,
		SSHHost:  "deploy@prod",
	}, nil)
	bashTool := findToolByName(t, router.BuildTools(t.TempDir()), "bash")

	runCtx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		_, runErr := bashTool.Execute(runCtx, "tc-abort", map[string]any{
			"command": "sleep 30 &\necho $! > " + shellQuote(pidFile) + "\nwait",
		}, nil)
		doneCh <- runErr
	}()

	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if p, pErr := strconv.Atoi(strings.TrimSpace(string(raw))); pErr == nil && p > 0 {
				childPID = p
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	if childPID == 0 {
		cancel()
		t.Fatal("timed out waiting for remote child PID file")
	}

	cancel()
	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for aborted remote bash tool to return")
	}

	// Verify child process is terminated shortly after SSH abort.
	killed := false
	for i := 0; i < 30; i++ {
		err := syscall.Kill(childPID, 0)
		if err != nil {
			killed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !killed {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("expected remote child process %d to be cleaned up after SSH abort", childPID)
	}
}

func TestManagerWorkspaceSessionIsolationProviderIsolationAndPromptTrust(t *testing.T) {
	installFakeSSH(t)

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("PIUI_SESSION_DIR", "")
	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))

	var mu sync.Mutex
	var authHeaders []string
	var systemPrompts []string

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHeaders = append(authHeaders, "A:"+r.Header.Get("Authorization"))
		mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if msgs, ok := payload["messages"].([]any); ok {
			for _, rawMsg := range msgs {
				if m, ok := rawMsg.(map[string]any); ok && (m["role"] == "system" || m["role"] == "developer") {
					switch c := m["content"].(type) {
					case string:
						mu.Lock()
						systemPrompts = append(systemPrompts, c)
						mu.Unlock()
					case []any:
						var sb strings.Builder
						for _, part := range c {
							if pm, ok := part.(map[string]any); ok {
								if txt, ok := pm["text"].(string); ok {
									sb.WriteString(txt)
								}
							}
						}
						mu.Lock()
						systemPrompts = append(systemPrompts, sb.String())
						mu.Unlock()
					}
				}
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok-from-A\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srvA.Close()

	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-key-B" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid key on B"}}`)
			return
		}
		mu.Lock()
		authHeaders = append(authHeaders, "B:"+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok-from-B\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srvB.Close()

	// TestProviderConnection verifies live endpoint + wire format + credentials.
	profA := store.ProviderProfile{
		ID:      "gw-a",
		Name:    "Gateway A",
		Format:  store.FormatOpenAICompletions,
		BaseURL: srvA.URL + "/v1",
		APIKey:  "sk-key-A",
		ModelID: "deepseek-v3",
	}
	profB := store.ProviderProfile{
		ID:      "gw-b",
		Name:    "Gateway B",
		Format:  store.FormatOpenAICompletions,
		BaseURL: srvB.URL + "/v1",
		APIKey:  "sk-key-B",
		ModelID: "qwen-coder",
	}
	if _, err := TestProviderConnection(context.Background(), profB); err != nil {
		t.Fatalf("TestProviderConnection(profB) failed: %v", err)
	}
	badProfB := profB
	badProfB.APIKey = "wrong-key"
	if _, err := TestProviderConnection(context.Background(), badProfB); err == nil {
		t.Fatal("expected TestProviderConnection with wrong key to fail")
	}

	// Prepare Local Workspace A (with untrusted .pi/skills), Local Workspace B, and Remote SSH Workspace C.
	dirA := filepath.Join(t.TempDir(), "local-repo-a")
	dirB := filepath.Join(t.TempDir(), "local-repo-b")
	dirRemoteC := filepath.Join(t.TempDir(), "remote-repo-c")
	for _, d := range []string{dirA, dirB, dirRemoteC} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dirA, ".pi", "skills", "local-secret-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dirA, ".pi", "skills", "local-secret-skill", "SKILL.md"),
		[]byte("---\nname: local-secret-skill\ndescription: Local repo skill gated by trust\n---\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dirRemoteC, ".pi", "skills", "remote-ops-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirRemoteC, "AGENTS.md"), []byte("REMOTE_CLUSTER_POLICY_V2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dirRemoteC, ".pi", "skills", "remote-ops-skill", "SKILL.md"),
		[]byte("---\nname: remote-ops-skill\ndescription: Remote cluster skill\n---\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	cfg := store.LoadOrInitWithOptions(store.LoadOptions{StartupDir: dirA, ExplicitStartupDir: true})
	cfg.Profiles = []store.ProviderProfile{profA, profB}
	cfg.ActiveProfileID = profA.ID
	wsA := *cfg.ActiveTarget() // Trusted: false by default
	wsB := cfg.UpsertLocalTarget(dirB, "Repo B", false)
	wsC := cfg.UpsertSSHTarget("ops@prod-c", dirRemoteC, "Prod SSH", false)
	wsC.Trusted = true
	cfg.SetTargetTrusted(wsC.ID, true)
	cfg.ActiveTargetID = wsA.ID

	mgr, err := NewManager(dirA, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	if !mgr.HasUntrustedProjectSkills() {
		t.Fatal("expected HasUntrustedProjectSkills=true on untrusted dirA")
	}

	// 1. Run on untrusted Local A -> local-secret-skill must NOT appear in system prompt.
	if res, err := mgr.Run(context.Background(), "hello A1"); err != nil || res.ErrorMessage != "" {
		t.Fatalf("Run A1 failed: err=%v msg=%q", err, res.ErrorMessage)
	}
	mu.Lock()
	if len(systemPrompts) == 0 {
		mu.Unlock()
		t.Fatal("expected captured system/developer prompt on srvA")
	}
	lastSys := systemPrompts[len(systemPrompts)-1]
	mu.Unlock()
	if strings.Contains(lastSys, "local-secret-skill") {
		t.Fatalf("untrusted workspace skill leaked into system prompt: %s", lastSys)
	}

	// 2. Trust Local A mid-session -> next turn immediately includes local-secret-skill without losing history!
	mgr.SetWorkspaceTrust(context.Background(), true)
	if mgr.HasUntrustedProjectSkills() {
		t.Fatal("expected HasUntrustedProjectSkills=false after trusting workspace")
	}
	if res, err := mgr.Run(context.Background(), "hello A2"); err != nil || res.ErrorMessage != "" {
		t.Fatalf("Run A2 failed: err=%v msg=%q", err, res.ErrorMessage)
	}
	mu.Lock()
	lastSys = systemPrompts[len(systemPrompts)-1]
	mu.Unlock()
	if !strings.Contains(lastSys, "local-secret-skill") {
		t.Fatalf("expected local-secret-skill in system prompt after SetWorkspaceTrust(true), got: %s", lastSys)
	}

	// Switch to profB and back to profA to verify isolated provider BaseURL + APIKey (no cross-overwrites).
	if err := mgr.ApplyProfile(&profB); err != nil {
		t.Fatal(err)
	}
	if res, err := mgr.Run(context.Background(), "ping B"); err != nil || res.ErrorMessage != "" {
		t.Fatalf("Run on profB failed: err=%v msg=%q", err, res.ErrorMessage)
	}
	if err := mgr.ApplyProfile(&profA); err != nil {
		t.Fatal(err)
	}

	sessionsInA := mgr.ListSavedSessionSummariesWithTitles(map[string]string{
		mgr.ActiveSessionID(): "自定义会话标题 A",
	})
	if len(sessionsInA) != 1 || sessionsInA[0].Title != "自定义会话标题 A" {
		t.Fatalf("expected 1 session in Workspace A with custom title, got %+v", sessionsInA)
	}

	// 3. Switch to Local Workspace B -> session list must be empty (isolated from A!).
	if err := mgr.SwitchWorkspace(wsB, &profA); err != nil {
		t.Fatalf("SwitchWorkspace(wsB): %v", err)
	}
	if got := mgr.ListSavedSessions(); len(got) != 0 {
		t.Fatalf("expected 0 sessions in fresh Workspace B, got %+v", got)
	}
	if _, err := mgr.Run(context.Background(), "hello from B"); err != nil {
		t.Fatalf("Run in B: %v", err)
	}
	if got := mgr.ListSavedSessions(); len(got) != 1 {
		t.Fatalf("expected 1 session in Workspace B, got %+v", got)
	}

	// 4. Switch to Remote SSH Workspace C (Trusted=true) -> isolated sessions + authentic remote system prompt!
	if err := mgr.SwitchWorkspace(wsC, &profA); err != nil {
		t.Fatalf("SwitchWorkspace(wsC): %v", err)
	}
	stC := mgr.RefreshSSHWorkspace(context.Background(), false)
	if !stC.Connected {
		t.Fatalf("expected SSH workspace C connected, got %+v", stC)
	}
	if got := mgr.ListSavedSessions(); len(got) != 0 {
		t.Fatalf("expected 0 sessions in fresh SSH Workspace C, got %+v", got)
	}
	if _, err := mgr.Run(context.Background(), "check remote cluster"); err != nil {
		t.Fatalf("Run in SSH Workspace C: %v", err)
	}
	mu.Lock()
	remoteSys := systemPrompts[len(systemPrompts)-1]
	mu.Unlock()
	if !strings.Contains(remoteSys, "REMOTE_CLUSTER_POLICY_V2") || !strings.Contains(remoteSys, "remote-ops-skill") || !strings.Contains(remoteSys, dirRemoteC) {
		t.Fatalf("expected remote AGENTS.md, remote-ops-skill, and remote Cwd in SSH system prompt, got:\n%s", remoteSys)
	}
	if strings.Contains(remoteSys, "local-secret-skill") {
		t.Fatalf("local repo skill must NOT leak into remote SSH system prompt!")
	}

	// 5. Delete active session in Workspace C -> removes file from disk and resets cleanly.
	cSessions := mgr.ListSavedSessions()
	if len(cSessions) != 1 {
		t.Fatalf("expected 1 saved session in C before delete, got %d", len(cSessions))
	}
	wasActive, err := mgr.DeleteSavedSession(cSessions[0].Path, &profA)
	if err != nil || !wasActive {
		t.Fatalf("DeleteSavedSession failed: wasActive=%v err=%v", wasActive, err)
	}
	if got := mgr.ListSavedSessions(); len(got) != 0 {
		t.Fatalf("expected 0 sessions in C after deleting active session, got %+v", got)
	}
}
