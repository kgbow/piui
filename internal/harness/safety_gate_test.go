package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"

	"piui/internal/store"
)

func TestAssessToolCallRiskPolicies(t *testing.T) {
	localTarget := store.WorkspaceTarget{
		ID:       "local-cwd",
		Name:     "Local",
		IsRemote: false,
		WorkDir:  "/Users/test/project",
	}
	remoteTarget := store.WorkspaceTarget{
		ID:       "ssh-prod",
		Name:     "Prod SSH",
		IsRemote: true,
		WorkDir:  "/srv/app",
		SSHHost:  "prod-box",
	}

	// 1. Critical commands must be caught even on Local under default SafetyPolicyRemoteHighRisk.
	crit := AssessToolCallRisk(localTarget, store.SafetyPolicyRemoteHighRisk, "bash", map[string]any{
		"command": "sudo rm -rf /",
	})
	if crit == nil || crit.Level != RiskCritical {
		t.Fatalf("expected critical risk for 'sudo rm -rf /' locally, got %+v", crit)
	}

	// 2. High-risk command (rm -rf build) is allowed locally under RemoteHighRisk,
	//    but intercepted on Remote SSH and under AlwaysHighRisk.
	if got := AssessToolCallRisk(localTarget, store.SafetyPolicyRemoteHighRisk, "bash", map[string]any{"command": "rm -rf ./build"}); got != nil {
		t.Fatalf("expected local 'rm -rf ./build' to pass under RemoteHighRisk, got %+v", got)
	}
	if got := AssessToolCallRisk(remoteTarget, store.SafetyPolicyRemoteHighRisk, "bash", map[string]any{"command": "rm -rf ./build"}); got == nil || got.Level != RiskHigh {
		t.Fatalf("expected remote 'rm -rf ./build' to be intercepted as high risk, got %+v", got)
	}
	if got := AssessToolCallRisk(localTarget, store.SafetyPolicyAlwaysHighRisk, "bash", map[string]any{"command": "systemctl restart nginx"}); got == nil {
		t.Fatalf("expected 'systemctl restart nginx' to be intercepted under AlwaysHighRisk")
	}

	// 3. Sensitive file paths (/etc/nginx/nginx.conf, ~/.ssh/authorized_keys, .env)
	if got := AssessToolCallRisk(localTarget, store.SafetyPolicyRemoteHighRisk, "edit", map[string]any{
		"path":    "/etc/nginx/nginx.conf",
		"oldText": "worker_processes 1;",
		"newText": "worker_processes 4;",
	}); got == nil || got.Level != RiskCritical {
		t.Fatalf("expected /etc/ edit to be critical risk, got %+v", got)
	}
	if got := AssessToolCallRisk(remoteTarget, store.SafetyPolicyRemoteHighRisk, "write", map[string]any{
		"path":    ".env.production",
		"content": "SECRET=123",
	}); got == nil {
		t.Fatalf("expected remote .env.production write to be intercepted")
	}

	// 4. Strict remote policy: read-only commands pass, non-readonly commands and regular file edits prompt.
	if got := AssessToolCallRisk(remoteTarget, store.SafetyPolicyStrictRemote, "bash", map[string]any{"command": "git status"}); got != nil {
		t.Fatalf("expected read-only 'git status' to pass in StrictRemote, got %+v", got)
	}
	if got := AssessToolCallRisk(remoteTarget, store.SafetyPolicyStrictRemote, "edit", map[string]any{"path": "main.go", "oldText": "a", "newText": "b"}); got == nil {
		t.Fatalf("expected regular remote file edit to require confirmation in StrictRemote")
	}

	// 5. SafetyPolicyOff disables all interception.
	if got := AssessToolCallRisk(remoteTarget, store.SafetyPolicyOff, "bash", map[string]any{"command": "rm -rf /"}); got != nil {
		t.Fatalf("expected nil assessment when SafetyPolicyOff, got %+v", got)
	}
}

func TestSafetyGateInteractiveApprovalAndTrust(t *testing.T) {
	remoteTarget := store.WorkspaceTarget{
		ID:       "ssh-prod",
		Name:     "Prod SSH",
		IsRemote: true,
		WorkDir:  "/srv/app",
		SSHHost:  "prod-box",
	}
	router := NewEnvironmentRouter(remoteTarget, nil)
	gate := NewSafetyGate(router, store.SafetyPolicyRemoteHighRisk, nil)

	promptCh := make(chan *ApprovalRequest, 4)
	gate.SetPromptHandler(func(req *ApprovalRequest) {
		promptCh <- req
	})

	callCtx := agent.BeforeToolCallContext{
		ToolCall: ai.ToolCall{ID: "tc-1", Name: "bash"},
		Args:     map[string]any{"command": "systemctl restart app.service"},
	}

	// 1. Test Rejection with user feedback
	doneCh := make(chan *agent.BeforeToolCallResult, 1)
	go func() {
		doneCh <- gate.BeforeToolCall(context.Background(), callCtx)
	}()

	var req1 *ApprovalRequest
	select {
	case req1 = <-promptCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for approval prompt")
	}
	if !gate.Resolve(req1.ID, ApprovalDecision{Approved: false, Feedback: "先检查日志再重启"}) {
		t.Fatal("expected Resolve to succeed")
	}
	res1 := <-doneCh
	if res1 == nil || !res1.Block || !strings.Contains(res1.Reason, "先检查日志再重启") {
		t.Fatalf("expected blocked result with user feedback, got %+v", res1)
	}

	// 2. Test Approval with TrustSignature = true
	go func() {
		doneCh <- gate.BeforeToolCall(context.Background(), callCtx)
	}()
	req2 := <-promptCh
	gate.Resolve(req2.ID, ApprovalDecision{Approved: true, TrustSignature: true})
	res2 := <-doneCh
	if res2 != nil {
		t.Fatalf("expected approved call to return nil BeforeToolCallResult, got %+v", res2)
	}
	if gate.TrustedCount() != 1 {
		t.Fatalf("expected TrustedCount=1, got %d", gate.TrustedCount())
	}

	// 3. Subsequent identical call should auto-pass without triggering prompt handler
	res3 := gate.BeforeToolCall(context.Background(), callCtx)
	if res3 != nil {
		t.Fatalf("expected trusted signature to auto-pass, got %+v", res3)
	}
	select {
	case unexpected := <-promptCh:
		t.Fatalf("unexpected prompt for trusted signature: %+v", unexpected)
	default:
	}
}


func TestStrictRemoteReadOnlyCommandValidation(t *testing.T) {
	remoteTarget := store.WorkspaceTarget{
		ID:       "ssh-prod",
		Name:     "Prod SSH",
		IsRemote: true,
		WorkDir:  "/srv/app",
		SSHHost:  "prod-box",
	}

	mustIntercept := []string{
		"find . -delete",
		"find /var/log -name '*.log' -exec rm {} +",
		`find . -ok rm {} \;`,
		"find . -fprint /tmp/out.txt",
		"git branch -D feature",
		"git branch -d feature",
		"git branch -m old new",
		"git branch new-feature",
		"git remote remove origin",
		"git remote add evil https://example.com/repo.git",
		"git remote set-url origin https://example.com/repo.git",
		"git log --output=/tmp/commits.txt",
		"git -c core.pager=sh log",
		"env rm -rf /tmp/x",
		"fd . -x rm",
		"rg secret --pre sh",
		"hostname pwned-node",
		"date -s 2026-01-01",
		"ls -la\nrm -rf ./build",
		"ls -la & rm -rf ./build",
		"echo $(whoami)",
	}
	for _, cmd := range mustIntercept {
		got := AssessToolCallRisk(remoteTarget, store.SafetyPolicyStrictRemote, "bash", map[string]any{
			"command": cmd,
		})
		if got == nil {
			t.Errorf("expected command %q to be intercepted in StrictRemote, but it was allowed", cmd)
		}
	}

	mustAllowReadOnly := []string{
		"ls -la",
		"find . -maxdepth 2 -name '*.go'",
		"git status --short",
		"git branch -a",
		"git branch --show-current",
		"git remote -v",
		"git remote get-url origin",
		"rg -n 'SafetyGate' internal/harness",
		"hostname",
		"date -u",
	}
	for _, cmd := range mustAllowReadOnly {
		got := AssessToolCallRisk(remoteTarget, store.SafetyPolicyStrictRemote, "bash", map[string]any{
			"command": cmd,
		})
		if got != nil {
			t.Errorf("expected read-only command %q to pass in StrictRemote, got %+v", cmd, got)
		}
	}
}

func TestTargetScopedTrustAndPinningAcrossMidFlightTargetSwitch(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	targetA := store.WorkspaceTarget{
		ID:       "local-a",
		Name:     "Staging Dir",
		IsRemote: false,
		WorkDir:  dirA,
	}
	targetB := store.WorkspaceTarget{
		ID:       "local-b",
		Name:     "Prod Dir",
		IsRemote: false,
		WorkDir:  dirB,
	}

	router := NewEnvironmentRouter(targetA, nil)
	gate := NewSafetyGate(router, store.SafetyPolicyAlwaysHighRisk, nil)

	promptCh := make(chan *ApprovalRequest, 4)
	gate.SetPromptHandler(func(req *ApprovalRequest) {
		promptCh <- req
	})

	tools := router.BuildTools(dirA)
	writeTool := findToolByName(t, tools, "write")

	// 1. Trigger BeforeToolCall on targetA for sensitive .env write ("tc-pin-1").
	call1 := agent.BeforeToolCallContext{
		ToolCall: ai.ToolCall{ID: "tc-pin-1", Name: "write"},
		Args:     map[string]any{"path": ".env", "content": "ENV=staging\n"},
	}
	doneCh := make(chan *agent.BeforeToolCallResult, 1)
	go func() {
		doneCh <- gate.BeforeToolCall(context.Background(), call1)
	}()

	reqA := <-promptCh
	if reqA.Target.WorkDir != dirA {
		t.Fatalf("expected approval target WorkDir=%q, got %q", dirA, reqA.Target.WorkDir)
	}

	// 2. Simulate user switching active target to targetB WHILE approval modal is open!
	router.SetTarget(targetB)

	// 3. Approve WITH TrustSignature=true.
	gate.Resolve(reqA.ID, ApprovalDecision{Approved: true, TrustSignature: true})
	if res := <-doneCh; res != nil {
		t.Fatalf("expected nil block on approved call, got %+v", res)
	}

	// 4. Execute tool "tc-pin-1": must write to pinned targetA, NOT newly switched targetB!
	if _, err := writeTool.Execute(context.Background(), "tc-pin-1", call1.Args, nil); err != nil {
		t.Fatalf("writeTool.Execute: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirA, ".env")); err != nil {
		t.Fatalf("expected .env to be written in pinned dirA: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirB, ".env")); !os.IsNotExist(err) {
		t.Fatalf("expected dirB/.env NOT to exist, got err=%v", err)
	}

	// 5. Same sensitive write on targetB MUST prompt again (trust from targetA must NOT carry over to targetB).
	call2 := agent.BeforeToolCallContext{
		ToolCall: ai.ToolCall{ID: "tc-pin-2", Name: "write"},
		Args:     map[string]any{"path": ".env", "content": "ENV=staging\n"},
	}
	go func() {
		doneCh <- gate.BeforeToolCall(context.Background(), call2)
	}()

	select {
	case reqB := <-promptCh:
		if reqB.Target.WorkDir != dirB {
			t.Fatalf("expected second prompt for dirB, got %q", reqB.Target.WorkDir)
		}
		// Reject and verify pinned target is cleaned up.
		gate.Resolve(reqB.ID, ApprovalDecision{Approved: false, Feedback: "wrong env"})
	case <-time.After(2 * time.Second):
		t.Fatal("expected prompt on targetB because trust signature is scoped to targetA")
	}
	if res := <-doneCh; res == nil || !res.Block {
		t.Fatalf("expected blocked result on targetB rejection, got %+v", res)
	}
	if len(router.pinnedTargets) != 0 {
		t.Fatalf("expected rejected tool call target pin to be released, got %+v", router.pinnedTargets)
	}
}

func TestNewSessionClearsTrustedSignatures(t *testing.T) {
	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("PIUI_SESSION_DIR", filepath.Join(t.TempDir(), "sessions"))

	cwd := t.TempDir()
	cfg := store.LoadOrInit(cwd)
	mgr, err := NewManager(cwd, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Trust a high-risk signature in current session.
	promptCh := make(chan *ApprovalRequest, 2)
	mgr.SafetyGate().SetPromptHandler(func(req *ApprovalRequest) {
		promptCh <- req
	})
	doneCh := make(chan *agent.BeforeToolCallResult, 1)
	go func() {
		doneCh <- mgr.SafetyGate().BeforeToolCall(context.Background(), agent.BeforeToolCallContext{
			ToolCall: ai.ToolCall{ID: "tc-crit-1", Name: "bash"},
			Args:     map[string]any{"command": "sudo rm -rf /"},
		})
	}()
	req := <-promptCh
	mgr.SafetyGate().Resolve(req.ID, ApprovalDecision{Approved: true, TrustSignature: true})
	<-doneCh

	if got := mgr.SafetyGate().TrustedCount(); got != 1 {
		t.Fatalf("expected TrustedCount=1 before session reset, got %d", got)
	}

	if err := mgr.InitOrResetSession(cfg.ActiveProfile()); err != nil {
		t.Fatalf("InitOrResetSession: %v", err)
	}
	if got := mgr.SafetyGate().TrustedCount(); got != 0 {
		t.Fatalf("expected TrustedCount=0 after starting new session, got %d", got)
	}
}
