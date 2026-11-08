package harness

import (
	"context"
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
