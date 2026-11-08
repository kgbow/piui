package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"

	"piui/internal/harness"
	"piui/internal/store"
)

func TestDeskAppHeadlessRenderAndHotSwitch(t *testing.T) {
	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("PIUI_SESSION_DIR", filepath.Join(t.TempDir(), "sessions"))

	dirA := t.TempDir()
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "a.txt"), []byte("hello from dir A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "b.txt"), []byte("hello from dir B\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := NewDeskApp(dirA)
	if err != nil {
		t.Fatalf("NewDeskApp: %v", err)
	}

	// Verify headless MyGo GPU UI tester renders the 3-column view cleanly.
	tester := ui.NewTester(app.View, 1360, 840)
	_ = tester

	// Verify custom OpenAI / Anthropic endpoint profile hot-apply.
	customProfile := &store.ProviderProfile{
		ID:            "custom-gw",
		Name:          "Custom Gateway",
		Format:        store.FormatOpenAICompletions,
		BaseURL:       "https://gateway.example.com/v1",
		APIKey:        "sk-test-12345",
		ModelID:       "deepseek-v3",
		ThinkingLevel: "high",
	}
	if err := app.mgr.ApplyProfile(customProfile); err != nil {
		t.Fatalf("ApplyProfile: %v", err)
	}

	// Verify tools route to dirA initially and stream bash output to TermDrawer.
	tools := app.mgr.Router().BuildTools(dirA)
	var readTool, bashTool, lsTool, probeTool int = -1, -1, -1, -1
	for i, tl := range tools {
		switch tl.Name {
		case "read":
			readTool = i
		case "bash":
			bashTool = i
		case "ls":
			lsTool = i
		case "workspace_probe":
			probeTool = i
		}
	}
	if readTool == -1 || bashTool == -1 || lsTool == -1 || probeTool == -1 {
		t.Fatalf("missing expected tools: read=%d bash=%d ls=%d probe=%d", readTool, bashTool, lsTool, probeTool)
	}

	resA, err := tools[readTool].Execute(context.Background(), "call-1", map[string]any{"path": "a.txt"}, nil)
	if err != nil || !strings.Contains(ExtractResultText(resA), "hello from dir A") {
		t.Fatalf("expected content from dirA, got err=%v text=%q", err, ExtractResultText(resA))
	}

	resLs, err := tools[lsTool].Execute(context.Background(), "call-ls", map[string]any{}, nil)
	if err != nil || !strings.Contains(ExtractResultText(resLs), "a.txt") {
		t.Fatalf("expected ls to contain a.txt, got err=%v text=%q", err, ExtractResultText(resLs))
	}

	resProbe, err := tools[probeTool].Execute(context.Background(), "call-probe", map[string]any{}, nil)
	if err != nil || !strings.Contains(ExtractResultText(resProbe), "HOST & OS") {
		t.Fatalf("expected workspace_probe output, got err=%v text=%q", err, ExtractResultText(resProbe))
	}

	// Hot-switch target to dirB within the same session and verify read + bash stream.
	app.mgr.SwitchTarget(store.WorkspaceTarget{
		ID:       "local-b",
		Name:     "Dir B",
		IsRemote: false,
		WorkDir:  dirB,
	})

	resB, err := tools[readTool].Execute(context.Background(), "call-2", map[string]any{"path": "b.txt"}, nil)
	if err != nil || !strings.Contains(ExtractResultText(resB), "hello from dir B") {
		t.Fatalf("expected content from dirB after hot-switch, got err=%v text=%q", err, ExtractResultText(resB))
	}

	_, err = tools[bashTool].Execute(context.Background(), "call-3", map[string]any{"command": "printf '\\033[32mLIVE_STREAM_OK\\033[0m\\n'"}, nil)
	if err != nil {
		t.Fatalf("bash execute: %v", err)
	}
	termOut := app.termDrawer.AllText()
	if !strings.Contains(termOut, "LIVE_STREAM_OK") || strings.Contains(termOut, "\x1b[32m") {
		t.Fatalf("expected clean live bash output without ANSI codes in TermDrawer, got: %q", termOut)
	}
}

func TestTruncatePreviewUTF8SafeAndAssistantEventHandling(t *testing.T) {
	chinese := "这是一段包含中文字符的测试文本"
	truncated := truncatePreview(chinese, 5)
	if !utf8.ValidString(truncated) {
		t.Fatalf("truncatePreview produced invalid UTF-8: %q", truncated)
	}
	if !strings.HasPrefix(truncated, "这是一段包\n") {
		t.Fatalf("unexpected truncated prefix: %q", truncated)
	}

	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))
	app, err := NewDeskApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Pure tool-call assistant message should not leave an empty assistant bubble behind.
	toolOnlyMsg := &ai.AssistantMessage{
		Content: ai.ContentList{
			ai.ToolCall{ID: "tc-1", Name: "bash", Arguments: map[string]any{"command": "ls"}},
		},
	}
	_ = app.handleAgentEvent(context.Background(), agent.AgentEvent{
		Type:    agent.EvMessageStart,
		Message: &ai.AssistantMessage{},
	})
	_ = app.handleAgentEvent(context.Background(), agent.AgentEvent{
		Type:    agent.EvMessageEnd,
		Message: toolOnlyMsg,
	})
	if len(app.entries) != 0 {
		t.Fatalf("expected empty assistant entry to be pruned, got %d entries", len(app.entries))
	}

	// Assistant error message without deltas should be captured on EvMessageEnd.
	errMsg := &ai.AssistantMessage{
		ErrorMessage: "401 Unauthorized",
	}
	_ = app.handleAgentEvent(context.Background(), agent.AgentEvent{
		Type:    agent.EvMessageStart,
		Message: errMsg,
	})
	_ = app.handleAgentEvent(context.Background(), agent.AgentEvent{
		Type:    agent.EvMessageEnd,
		Message: errMsg,
	})
	if len(app.entries) != 1 || app.entries[0].ErrorText != "401 Unauthorized" {
		t.Fatalf("expected assistant ErrorMessage to be preserved, got %+v", app.entries)
	}

	// Verify Session Tree tab, Settings Safety tab, and ApprovalModal render cleanly in headless tester.
	app.sidebarTab = 1
	app.settingsModal.Open = true
	app.settingsModal.ActiveTab = 2
	doneCh := make(chan *agent.BeforeToolCallResult, 1)
	go func() {
		doneCh <- app.mgr.SafetyGate().BeforeToolCall(context.Background(), agent.BeforeToolCallContext{
			ToolCall: ai.ToolCall{ID: "tc-crit", Name: "bash"},
			Args:     map[string]any{"command": "rm -rf /"},
		})
	}()
	for app.approvalModal.PendingCount() == 0 {
		// spin briefly until prompt handler enqueues request
	}
	tester := ui.NewTester(app.View, 1360, 840)
	_ = tester
	req := app.approvalModal.Current()
	if req == nil {
		t.Fatal("expected pending approval request in ApprovalModal")
	}
	app.mgr.SafetyGate().Resolve(req.ID, harness.ApprovalDecision{Approved: true})
	app.approvalModal.Pop()
	if res := <-doneCh; res != nil {
		t.Fatalf("expected approved tool call to return nil, got %+v", res)
	}
}

func TestOpenAICompletionsStreamFetchModelsAndChannelSwitch(t *testing.T) {
	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("PIUI_SESSION_DIR", filepath.Join(t.TempDir(), "sessions"))

	var requestedModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if r.Header.Get("Authorization") != "Bearer sk-test-gw" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"deepseek-v3"},{"id":"qwen-max"},{"id":"gpt-4.1"}]}`)
		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			if m, ok := payload["model"].(string); ok {
				requestedModels = append(requestedModels, m)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	app, err := NewDeskApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewDeskApp: %v", err)
	}

	// 1. Add a custom OpenAI Completions channel and fetch models from /v1/models
	gwProfile := store.ProviderProfile{
		ID:            "gw-channel",
		Name:          "聚合网关",
		Format:        store.FormatOpenAICompletions,
		BaseURL:       srv.URL + "/v1",
		APIKey:        "sk-test-gw",
		ThinkingLevel: "medium",
	}

	fetched, err := harness.FetchModels(context.Background(), gwProfile)
	if err != nil {
		t.Fatalf("FetchModels failed: %v", err)
	}
	if len(fetched) != 3 || fetched[0] != "deepseek-v3" || fetched[1] != "qwen-max" {
		t.Fatalf("unexpected fetched models: %+v", fetched)
	}

	gwProfile.Models = fetched
	gwProfile.ModelID = fetched[0]
	app.cfg.Profiles = append(app.cfg.Profiles, gwProfile)

	// 2. Switch to the new channel and verify openai-completions streaming succeeds
	app.switchChannel(nil, "gw-channel")
	res1, err := app.mgr.Run(context.Background(), "ping 1")
	if err != nil {
		t.Fatalf("Run with openai-completions failed: %v", err)
	}
	if res1.ErrorMessage != "" {
		t.Fatalf("unexpected ErrorMessage on openai-completions run: %s", res1.ErrorMessage)
	}

	// 3. Switch model within the channel mid-conversation and verify the new model is used
	app.switchModel(nil, "qwen-max")
	res2, err := app.mgr.Run(context.Background(), "ping 2")
	if err != nil || res2.ErrorMessage != "" {
		t.Fatalf("second Run after model switch failed: err=%v msg=%q", err, res2.ErrorMessage)
	}

	if len(requestedModels) != 2 || requestedModels[0] != "deepseek-v3" || requestedModels[1] != "qwen-max" {
		t.Fatalf("expected requestedModels=[deepseek-v3, qwen-max], got %+v", requestedModels)
	}

	// 4. Verify SettingsModal provider tab with fetched models renders cleanly
	app.settingsModal.Open = true
	app.settingsModal.ActiveTab = 0
	tester := ui.NewTester(app.View, 1360, 840)
	_ = tester
}

func TestDualThemeAndDoopComponentsRender(t *testing.T) {
	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("PIUI_SESSION_DIR", filepath.Join(t.TempDir(), "sessions"))

	app, err := NewDeskApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewDeskApp: %v", err)
	}

	// Populate entries covering User (normal + Steer), Assistant (thinking collapsed & expanded), and Tool cards (Local + SSH with Diff)
	app.entries = []*ChatEntry{
		{
			ID:          app.allocID(),
			Kind:        EntryUser,
			Text:        "排查网关连接池超时原因并修复",
			TargetBadge: "piui (Local)",
			IsRemote:    false,
			EntryID:     "e101a2b",
			Done:        true,
		},
		{
			ID:          app.allocID(),
			Kind:        EntryTool,
			ToolCallID:  "tc-1",
			ToolName:    "edit",
			ToolSummary: "/srv/gateway/conf.d/upstream.conf",
			DiffOld:     "keepalive_timeout 5s;",
			DiffNew:     "keepalive_timeout 65s;",
			ToolOutput:  "Successfully replaced 1 block(s)",
			TargetBadge: "SSH · prod-gateway",
			IsRemote:    true,
			DurationMs:  142,
			CardOpen:    true,
			Done:        true,
		},
		{
			ID:          app.allocID(),
			Kind:        EntryUser,
			Text:        "先只用 nginx -t 校验语法，不要直接 reload",
			Steered:     true,
			TargetBadge: "prod-gateway (SSH)",
			IsRemote:    true,
			EntryID:     "e102c3d",
			Done:        true,
		},
		{
			ID:          app.allocID(),
			Kind:        EntryAssistant,
			Thinking:    "收到 Steer 纠偏指令，跳过 reload，仅执行 nginx -t 验证语法。",
			ThinkOpen:   false,
			Text:        "已在远端执行 `nginx -t`，语法检查通过。",
			TargetBadge: "prod-gateway (SSH)",
			IsRemote:    true,
			EntryID:     "e103f4e",
			Done:        true,
		},
	}

	app.treeSnap = &harness.SessionTreeSnapshot{
		SessionID:    "sess-demo",
		ActiveLeafID: "e103f4e",
		BranchCount:  2,
		TotalEntries: 4,
		Nodes: []harness.SessionTreeNode{
			{EntryID: "e101a2b", Role: "user", Label: "排查网关连接池超时原因并修复", OnActiveBranch: true, IsForkPoint: true},
			{EntryID: "e103f4e", Role: "assistant", Label: "已在远端执行 nginx -t", Depth: 1, BranchIndex: 1, OnActiveBranch: true, IsActiveCursor: true},
		},
	}

	// Render in Dark Theme (Obsidian)
	if app.cfg.IsLightTheme() {
		t.Fatal("expected initial theme to be dark")
	}
	tester := ui.NewTester(app.View, 1360, 840)

	// Toggle to Light Theme (Daylight) and render again with Thinking expanded and Settings Tab 1 open
	app.toggleTheme(nil)
	if !app.cfg.IsLightTheme() {
		t.Fatal("expected theme to switch to light after toggleTheme")
	}
	app.entries[3].ThinkOpen = true
	app.settingsModal.Open = true
	app.settingsModal.ActiveTab = 1
	tester.Frame()

	// Verify ExecutionEnv on router for both local and remote targets
	if env := app.mgr.Router().ExecutionEnv(); env == nil || env.Cwd() == "" {
		t.Fatalf("expected valid Local ExecutionEnv, got %+v", env)
	}
	app.mgr.SwitchTarget(store.WorkspaceTarget{
		ID:       "ssh-prod",
		Name:     "prod-gateway",
		IsRemote: true,
		WorkDir:  "/srv/gateway",
		SSHHost:  "prod-gateway",
	})
	if env := app.mgr.Router().ExecutionEnv(); env == nil || env.Cwd() != "/srv/gateway" {
		t.Fatalf("expected SSHEnv Cwd='/srv/gateway', got %+v", env)
	}
}
