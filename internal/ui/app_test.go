package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/transfer"
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

	resBash, err := tools[bashTool].Execute(context.Background(), "call-3", map[string]any{"command": "printf '\\033[32mLIVE_STREAM_OK\\033[0m\\n'"}, nil)
	if err != nil {
		t.Fatalf("bash execute: %v", err)
	}
	cleanedOut := StripANSI(ExtractResultText(resBash))
	if !strings.Contains(cleanedOut, "LIVE_STREAM_OK") || strings.Contains(cleanedOut, "\x1b[32m") {
		t.Fatalf("expected clean bash output without ANSI codes, got: %q", cleanedOut)
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

	// Verify Branch Popover, Settings Safety tab, and ApprovalModal render cleanly in headless tester.
	app.branchPopoverOpen = true
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

	// Verify code/terminal palette tokens follow theme switching instead of staying dark in Light mode
	dp := DarkPalette()
	lp := LightPalette()
	if dp.CodeBg == lp.CodeBg || dp.BgSurface == lp.BgSurface {
		t.Fatalf("expected distinct dark/light palette tokens, got dark=%+v light=%+v", dp, lp)
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

	// Verify unified cascading selector switch (Provider ▸ Model ▸ Thinking Level)
	app.switchChannelModelThinking(nil, "openai-chat", "gpt-4.1-mini", "high")
	activeProf := app.cfg.ActiveProfile()
	if activeProf == nil || activeProf.ID != "openai-chat" || activeProf.ModelID != "gpt-4.1-mini" || activeProf.ThinkingLevel != "high" {
		t.Fatalf("unexpected profile state after switchChannelModelThinking: %+v", activeProf)
	}
}

func TestLongAPIErrorParsingAndLayoutBounds(t *testing.T) {
	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("PIUI_SESSION_DIR", filepath.Join(t.TempDir(), "sessions"))

	err500 := `OpenAI API error (500): {"message":"not implemented (request id: 202611091400312266598298268d9d68V0z3UAm)","type":"new_api_error","param":"","code":"convert_request_failed"}`
	parsed500 := ParseAPIError(err500)
	if !strings.Contains(parsed500.Title, "OpenAI API error (500)") || !strings.Contains(parsed500.Title, "convert_request_failed") {
		t.Fatalf("unexpected 500 title: %q", parsed500.Title)
	}
	if !strings.Contains(parsed500.Detail, "not implemented") {
		t.Fatalf("unexpected 500 detail: %q", parsed500.Detail)
	}

	err504 := `504 {"type":"https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-5xx-errors/error-504/","title":"Error 504: Gateway time-out","status":504,"detail":"The origin web server did not respond to Cloudflare within the allowed time. This may be due to an overloaded server or a backend process taking too long.","instance":"a57d5915586f6e55","error_code":504,"error_name":"origin_gateway_timeout","error_category":"origin","ray_id":"a57d5915586f6e55","timestamp":"2026-11-09T14:02:11Z","zone":"newapi.kgbow.de","cloudflare_error":true,"retryable":true,"retry_strategy":"Wait a moment and retry.** Back off for at least 120 seconds. If the error persists, the website operator should investigate long-running processes on the origin server.","footer":"This error was generated by Cloudflare on behalf of the website owner."}`
	parsed504 := ParseAPIError(err504)
	if !strings.Contains(parsed504.Title, "Error 504: Gateway time-out") || !strings.Contains(parsed504.Title, "newapi.kgbow.de") {
		t.Fatalf("unexpected 504 title: %q", parsed504.Title)
	}
	if !strings.Contains(parsed504.Detail, "Ray ID: a57d5915586f6e55") {
		t.Fatalf("expected Ray ID in 504 detail, got %q", parsed504.Detail)
	}
	if parsed504.PrettyRaw == "" {
		t.Fatal("expected non-empty PrettyRaw for JSON error")
	}

	app, err := NewDeskApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewDeskApp: %v", err)
	}

	app.entries = []*ChatEntry{
		{
			ID:      app.allocID(),
			Kind:    EntryUser,
			Text:    "hello",
			EntryID: "4d2987",
			Done:    true,
		},
		{
			ID:        app.allocID(),
			Kind:      EntryAssistant,
			ErrorText: err500,
			EntryID:   "8f2eac",
			Done:      true,
		},
		{
			ID:      app.allocID(),
			Kind:    EntryUser,
			Text:    "hello",
			EntryID: "c532e0",
			Done:    true,
		},
		{
			ID:              app.allocID(),
			Kind:            EntryAssistant,
			ErrorText:       err504,
			ErrorDetailOpen: true,
			EntryID:         "fa07da",
			Done:            true,
		},
	}

	tester := ui.NewTester(app.View, 1360, 840)

	// Verify right-side top-bar icon buttons and central stream stay inside the 1360px 2-pane window!
	safetyAcc, ok := tester.Access("安全拦截策略: 远程高危拦截")
	if !ok || safetyAcc.Bounds.X+safetyAcc.Bounds.W > 1360 {
		t.Fatalf("expected safety icon button inside 1360px window, got ok=%v info=%+v", ok, safetyAcc)
	}
	helloRect, ok := tester.Find("hello")
	if !ok || helloRect.X < 260 || helloRect.X+helloRect.W > 1360 {
		t.Fatalf("expected 'hello' bubble inside CenterMain [265, 1360], got ok=%v rect=%+v", ok, helloRect)
	}
}


func TestPersistenceWarningsAndPinnedToolCardBadge(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "config.json")
	if err := os.WriteFile(cfgFile, []byte("{corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make PIUI_SESSION_DIR a regular file so session recorder initialization fails visibly.
	blockerFile := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(blockerFile, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PIUI_CONFIG_PATH", cfgFile)
	t.Setenv("HOME", blockerFile)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(blockerFile, "agent"))

	app, err := NewDeskApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewDeskApp should still start with degraded warnings, got err: %v", err)
	}
	if !strings.Contains(app.lastError, "JSON 解析失败") || !strings.Contains(app.lastError, ".corrupt-") {
		t.Fatalf("expected corrupt config warning in app.lastError, got %q", app.lastError)
	}
	if !strings.Contains(app.lastError, "不会落盘") {
		t.Fatalf("expected session recorder warning in app.lastError, got %q", app.lastError)
	}

	// Make config directory read-only so atomic cfg.Save() fails and surfaces error.
	if err := os.Chmod(tmp, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(tmp, 0o700) }()
	app.lastError = ""
	app.toggleTheme(nil)
	_ = os.Chmod(tmp, 0o700)
	if !strings.Contains(app.lastError, "保存") {
		t.Fatalf("expected cfg.Save failure surfaced in app.lastError, got %q", app.lastError)
	}

	// Verify pinned tool target badge is used on EvToolExecStart even if router active target changed.
	pinnedWT := app.mgr.Router().PinToolTarget("tc-pinned-ui")
	app.mgr.SwitchTarget(store.WorkspaceTarget{
		ID:       "ssh-other",
		Name:     "SSH: other-host",
		IsRemote: true,
		WorkDir:  "/srv/other",
		SSHHost:  "other-host",
	})
	_ = app.handleAgentEvent(context.Background(), agent.AgentEvent{
		Type:       agent.EvToolExecutionStart,
		ToolCallID: "tc-pinned-ui",
		ToolName:   "bash",
		Args:       map[string]any{"command": "pwd"},
	})
	if len(app.entries) == 0 || app.entries[len(app.entries)-1].TargetBadge != pinnedWT.ToolCardBadge() {
		t.Fatalf("expected tool card badge=%q from pinned target, got %+v", pinnedWT.ToolCardBadge(), app.entries)
	}
}

func TestCaptureVisualSnapshots(t *testing.T) {
	outDir := os.Getenv("PIUI_SCREENSHOT_DIR")
	if outDir == "" {
		t.Skip("set PIUI_SCREENSHOT_DIR to export PNG UI snapshots")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("PIUI_SESSION_DIR", filepath.Join(t.TempDir(), "sessions"))

	app, err := NewDeskApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app.cfg.Targets = append(app.cfg.Targets, store.WorkspaceTarget{
		ID:       "ssh-prod",
		Name:     "prod-gateway",
		IsRemote: true,
		WorkDir:  "/srv/gateway",
		SSHHost:  "prod-gateway",
	})
	app.savedSessions = []harness.SavedSessionSummary{
		{ID: "s1", Title: "排查生产网关连接池超时与重试雪崩", Timestamp: "11-10 22:14", Messages: 12},
		{ID: "s2", Title: "重构用户权限鉴权中间件与单元测试", Timestamp: "11-09 16:30", Messages: 8},
	}
	app.activeSessID = "s1"

	saveSnap := func(tester *ui.Tester, name string) {
		tester.Frame()
		img := tester.Image()
		f, err := os.Create(filepath.Join(outDir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
	}

	tester := ui.NewTester(app.View, 1360, 840)
	tester.SetScale(2)
	tester.SetTitleBar(ui.TitleBar{Height: 28, Left: 78})

	// 1. Empty state (Dark)
	saveSnap(tester, "01_empty_dark")

	// 2. Rich Chat + Tool Cards + Code Block (Dark)
	app.entries = []*ChatEntry{
		{
			ID:          app.allocID(),
			Kind:        EntryUser,
			Text:        "帮我检查远程网关的 upstream 超时配置，把 keepalive_timeout 调优到 65s 并验证语法。",
			TargetBadge: "prod-gateway (SSH)",
			IsRemote:    true,
			EntryID:     "e101a2b",
			Done:        true,
		},
		{
			ID:          app.allocID(),
			Kind:        EntryAssistant,
			Thinking:    "先读取 /srv/gateway/conf.d/upstream.conf 确认当前 keepalive_timeout 参数，再通过 edit 修改并用 nginx -t 验证配置有效性。",
			ThinkOpen:   false,
			Text:        "我来先检查并更新远端 `upstream.conf` 的连接保持超时设置：",
			TargetBadge: "prod-gateway (SSH)",
			IsRemote:    true,
			EntryID:     "e102b3c",
			Done:        true,
		},
		{
			ID:          app.allocID(),
			Kind:        EntryTool,
			ToolCallID:  "tc-read",
			ToolName:    "read",
			ToolSummary: "/srv/gateway/conf.d/upstream.conf",
			TargetBadge: "Local · piui",
			IsRemote:    false,
			DurationMs:  19,
			CardOpen:    false,
			Done:        true,
		},
		{
			ID:          app.allocID(),
			Kind:        EntryTool,
			ToolCallID:  "tc-edit",
			ToolName:    "edit",
			ToolSummary: "/srv/gateway/conf.d/upstream.conf",
			DiffOld:     "upstream api_backend {\n    keepalive 32;\n    keepalive_timeout 5s;\n}",
			DiffNew:     "upstream api_backend {\n    keepalive 64;\n    keepalive_timeout 65s;\n}",
			ToolOutput:  "Updated 1 hunk in /srv/gateway/conf.d/upstream.conf",
			TargetBadge: "SSH · prod-gateway",
			IsRemote:    true,
			DurationMs:  138,
			CardOpen:    true,
			Done:        true,
		},
		{
			ID:      app.allocID(),
			Kind:    EntryAssistant,
			Text:    "已完成参数调优并通过 `nginx -t` 校验。核心变更如下：\n\n```nginx\nupstream api_backend {\n    keepalive 64;\n    keepalive_timeout 65s;\n}\n```\n\n如需平滑生效，确认后即可执行 `nginx -s reload`。",
			EntryID: "e105f9a",
			Done:    true,
		},
	}
	app.treeSnap = &harness.SessionTreeSnapshot{
		SessionID:    "s1",
		ActiveLeafID: "e105f9a",
		BranchCount:  2,
		TotalEntries: 5,
		Nodes: []harness.SessionTreeNode{
			{EntryID: "e101a2b", Role: "user", Label: "帮我检查远程网关的 upstream 超时配置", OnActiveBranch: true, IsForkPoint: true},
			{EntryID: "e102old", Role: "assistant", Label: "直接执行 systemctl restart nginx", Depth: 1, BranchIndex: 1, TotalBranches: 2, IsLeaf: true},
			{EntryID: "e102b3c", Role: "assistant", Label: "先检查并更新 upstream.conf 设置", Depth: 1, BranchIndex: 2, TotalBranches: 2, OnActiveBranch: true},
			{EntryID: "e105f9a", Role: "assistant", Label: "已完成参数调优并通过 nginx -t 校验", Depth: 1, OnActiveBranch: true, IsActiveCursor: true, IsLeaf: true},
		},
	}
	saveSnap(tester, "02_chat_dark")

	// 3. Branch Tree Popover + Light Theme
	app.branchPopoverOpen = true
	app.toggleTheme(nil)
	saveSnap(tester, "03_chat_tree_light")
	app.branchPopoverOpen = false

	// 4. Settings Modal Tab 0 (Dark)
	app.toggleTheme(nil)
	app.settingsModal.Open = true
	app.settingsModal.ActiveTab = 0
	saveSnap(tester, "04_settings_tab0_dark")

	// 5. Settings Modal Tab 1 (Light)
	app.toggleTheme(nil)
	app.settingsModal.ActiveTab = 1
	saveSnap(tester, "05_settings_tab1_light")

	// 6. Settings Modal Tab 2 (Dark)
	app.toggleTheme(nil)
	app.settingsModal.ActiveTab = 2
	saveSnap(tester, "06_settings_tab2_dark")

	// 7. Approval Modal (Dark)
	app.settingsModal.Open = false
	app.approvalModal.Enqueue(&harness.ApprovalRequest{
		ID:       "req-1",
		ToolName: "bash",
		Target: store.WorkspaceTarget{
			ID:       "ssh-prod",
			Name:     "prod-gateway",
			IsRemote: true,
			WorkDir:  "/srv/gateway",
			SSHHost:  "prod-gateway",
		},
		Assessment: harness.RiskAssessment{
			Level:    harness.RiskHigh,
			Category: "远程服务重载 / 进程变更",
			Reason:   "在远端 SSH 主机上执行 nginx 服务变更指令，可能影响线上流量",
			Preview:  "sudo nginx -s reload",
		},
	})
	saveSnap(tester, "07_approval_dark")
	app.approvalModal.Clear()

	// 8. Collapsed Left Sidebar + Expanded Thinking (Dark)
	app.sidebarCollapsed = true
	app.entries[1].ThinkOpen = true
	saveSnap(tester, "08_collapsed_sidebar_dark")
}

func TestSettingsModalFixedHeightSidebarCollapseAndTreeTopology(t *testing.T) {
	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("PIUI_SESSION_DIR", filepath.Join(t.TempDir(), "sessions"))

	app, err := NewDeskApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewDeskApp: %v", err)
	}

	tester := ui.NewTester(app.View, 1360, 840)

	// 1. Verify Cmd+B collapses sidebar and topbar expand button restores it
	tester.Key(ui.Cmd, ui.KeyB)
	if !app.sidebarCollapsed {
		t.Fatal("expected Cmd+B to collapse left sidebar")
	}
	expandBtn, ok := tester.Access("展开侧边栏 (Ctrl/Cmd+B)")
	if !ok {
		t.Fatal("expected expand-sidebar button in topbar when collapsed")
	}
	tester.ClickAt(expandBtn.Bounds.X+expandBtn.Bounds.W/2, expandBtn.Bounds.Y+expandBtn.Bounds.H/2)
	if app.sidebarCollapsed {
		t.Fatal("expected clicking expand button to restore left sidebar")
	}

	// 2. Verify SettingsModal maintains identical 560px dialog height across all 4 tabs (Providers, Workspaces, Safety, MCP)
	app.settingsModal.Open = true
	var tabHeights [4]float32
	for tab := 0; tab < 4; tab++ {
		app.settingsModal.ActiveTab = tab
		tester.Frame()
		dlg, ok := tester.AccessByRole(ui.RoleDialog, "")
		if !ok {
			t.Fatalf("expected RoleDialog on tab %d", tab)
		}
		if dlg.Bounds.H != 560 {
			t.Fatalf("expected SettingsModal RoleDialog height=560 on tab %d, got %.1f", tab, dlg.Bounds.H)
		}
		tabHeights[tab] = dlg.Bounds.H
	}
	if tabHeights[0] != tabHeights[1] || tabHeights[1] != tabHeights[2] || tabHeights[2] != tabHeights[3] {
		t.Fatalf("expected zero height jump across all 4 tabs, got %+v", tabHeights)
	}

	// 4. Verify DFS vector tree trunk continuation geometry
	nodes := []harness.SessionTreeNode{
		{EntryID: "root", Depth: 0, IsForkPoint: true},
		{EntryID: "b1", Depth: 1, BranchIndex: 1, TotalBranches: 2, IsLeaf: true},
		{EntryID: "b2", Depth: 1, BranchIndex: 2, TotalBranches: 2, IsLeaf: false},
		{EntryID: "b2-child", Depth: 1, BranchIndex: 0, IsLeaf: true},
	}
	if !treeColumnContinuesBelow(nodes, 0, 0) {
		t.Fatal("root col 0 should continue below to feed branches")
	}
	if !treeColumnContinuesBelow(nodes, 1, 0) {
		t.Fatal("b1 col 0 should continue below to feed sibling b2")
	}
	if treeColumnContinuesBelow(nodes, 1, 1) {
		t.Fatal("b1 col 1 is a leaf and must NOT connect to sibling b2")
	}
	if treeColumnContinuesBelow(nodes, 2, 0) {
		t.Fatal("b2 is the last branch of root; col 0 must stop at elbow")
	}
	if !treeColumnContinuesBelow(nodes, 2, 1) {
		t.Fatal("b2 col 1 should continue below to linear child b2-child")
	}
	if treeColumnContinuesBelow(nodes, 3, 0) || treeColumnContinuesBelow(nodes, 3, 1) {
		t.Fatal("last node must not continue any column below")
	}
}

func TestWorkspaceSwitchSessionLifecycleAndTrustBanner(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("PIUI_SESSION_DIR", "")
	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	dirA := filepath.Join(t.TempDir(), "proj-alpha")
	dirB := filepath.Join(t.TempDir(), "proj-beta")
	if err := os.MkdirAll(filepath.Join(dirA, ".pi", "skills", "lint-check"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dirA, ".pi", "skills", "lint-check", "SKILL.md"),
		[]byte("---\nname: lint-check\ndescription: Custom linter\n---\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	app, err := NewDeskApp(dirA)
	if err != nil {
		t.Fatalf("NewDeskApp: %v", err)
	}
	defer app.Close()

	// Configure mock OpenAI channel so Run persists real JSONL sessions.
	gwProf := store.ProviderProfile{
		ID:      "mock-gw",
		Name:    "MockGW",
		Format:  store.FormatOpenAICompletions,
		BaseURL: srv.URL + "/v1",
		APIKey:  "sk-mock",
		ModelID: "gpt-4.1",
	}
	app.cfg.Profiles = []store.ProviderProfile{gwProf}
	app.switchChannel(nil, gwProf.ID)

	wsA := *app.cfg.ActiveTarget()
	wsB := app.cfg.UpsertLocalTarget(dirB, "proj-beta", false)
	wsSSH := app.cfg.UpsertSSHTarget("deploy@edge", "/srv/edge", "edge-ssh", false)

	tester := ui.NewTester(app.View, 1360, 840)

	// 1. Untrusted .pi/skills banner is visible and clicking "信任此工作区" trusts & hides it.
	trustBtn, ok := tester.Access("信任此工作区")
	if !ok {
		t.Fatal("expected '信任此工作区' button when untrusted .pi/skills exist")
	}
	tester.ClickAt(trustBtn.Bounds.X+trustBtn.Bounds.W/2, trustBtn.Bounds.Y+trustBtn.Bounds.H/2)
	tester.Frame()
	if app.mgr.HasUntrustedProjectSkills() {
		t.Fatal("expected workspace A to become trusted after clicking trust button")
	}
	if _, stillVisible := tester.Access("信任此工作区"); stillVisible {
		t.Fatal("expected trust banner to disappear once trusted")
	}

	// 2. Run a turn in Workspace A -> creates saved session, rename it, and verify sidebar renders custom title.
	if res, err := app.mgr.Run(context.Background(), "重构订单模块"); err != nil || res.ErrorMessage != "" {
		t.Fatalf("Run in A failed: err=%v msg=%q", err, res.ErrorMessage)
	}
	app.refreshSessionsList()
	if len(app.savedSessions) != 1 {
		t.Fatalf("expected 1 saved session in Workspace A, got %d", len(app.savedSessions))
	}
	sessA := app.savedSessions[0]
	app.RenameSession(nil, sessA.ID, "订单模块架构重构")
	tester.Frame()
	if _, found := tester.Find("订单模块架构重构"); !found {
		t.Fatal("expected renamed session title '订单模块架构重构' in sidebar")
	}

	// 3. Switch to Workspace B -> session list is isolated (0 sessions in B).
	app.SwitchWorkspace(nil, wsB)
	tester.Frame()
	if len(app.savedSessions) != 0 {
		t.Fatalf("expected 0 saved sessions in Workspace B, got %+v", app.savedSessions)
	}

	// 4. Switch to Remote SSH Workspace -> top bar renders SSH status pill within 1360px bounds.
	app.SwitchWorkspace(nil, wsSSH)
	tester.Frame()
	if _, found := tester.Find("连接检测中"); !found {
		t.Fatal("expected SSH checking status pill in top bar")
	}

	// 5. Switch back to Workspace A -> renamed session is still preserved, then delete it cleanly.
	wsA.Trusted = true
	app.SwitchWorkspace(nil, wsA)
	tester.Frame()
	if len(app.savedSessions) != 1 || app.savedSessions[0].Title != "订单模块架构重构" {
		t.Fatalf("expected Workspace A renamed session restored, got %+v", app.savedSessions)
	}
	app.DeleteSession(nil, app.savedSessions[0])
	tester.Frame()
	if len(app.savedSessions) != 0 {
		t.Fatalf("expected 0 sessions in Workspace A after DeleteSession, got %+v", app.savedSessions)
	}
}

func TestPhase2AutocompleteAttachmentsRollbackGitAndCommandPalette(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("PIUI_SESSION_DIR", "")
	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))
	store.SetSecretStore(store.NewMemorySecretStore())

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodyStr := string(raw)
		reply := "已完成代码检查。"
		if strings.Contains(bodyStr, "context summarization assistant") {
			reply = "## Goal\n- 验证阶段二核心功能\n\n## Progress\n- [x] 已归档前序上下文"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{
			"id":      "chatcmpl-ui-p2",
			"object":  "chat.completion.chunk",
			"created": 1700000000,
			"model":   "gpt-4o",
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         map[string]any{"role": "assistant", "content": reply},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     310,
				"completion_tokens": 64,
				"total_tokens":      374,
			},
		})
		_, _ = w.Write([]byte("data: " + string(chunk) + "\n\ndata: [DONE]\n\n"))
	}))
	defer mockLLM.Close()

	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "internal", "mgr"), 0o755); err != nil {
		t.Fatal(err)
	}
	servicePath := filepath.Join(workDir, "service.go")
	origService := "package main\n\nfunc Version() string {\n\treturn \"v1\"\n}\n"
	if err := os.WriteFile(servicePath, []byte(origService), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "internal", "mgr", "manager.go"), []byte("package mgr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(workDir, "deploy.yaml")
	if err := os.WriteFile(yamlPath, []byte("replicas: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Initialize Git repo so Git status pill & popover can be tested end-to-end
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", workDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	runGit("init", "-b", "feat/ui-phase2")
	runGit("add", "service.go", "internal/mgr/manager.go")
	runGit("commit", "-m", "init")

	app, err := NewDeskAppWithOptions(store.LoadOptions{
		StartupDir:         workDir,
		ExplicitStartupDir: true,
	})
	if err != nil {
		t.Fatalf("NewDeskAppWithOptions: %v", err)
	}
	defer app.Close()

	app.cfg.Profiles = []store.ProviderProfile{
		{
			ID:            "p2-prof",
			Name:          "Phase2 Gateway",
			Format:        store.FormatOpenAICompletions,
			BaseURL:       mockLLM.URL + "/v1",
			APIKey:        "sk-p2",
			ModelID:       "gpt-4o",
			ThinkingLevel: "medium",
		},
	}
	app.cfg.ActiveProfileID = "p2-prof"
	if err := app.mgr.ApplyProfile(app.cfg.ActiveProfile()); err != nil {
		t.Fatal(err)
	}

	app.RefreshGitStatusSync()
	tester := ui.NewTester(app.View, 1360, 840)

	// 1. Verify Git branch pill renders in top bar and opens popover on click
	gitBranchRect, found := tester.Find("feat/ui-phase2")
	if !found {
		t.Fatal("expected Git branch pill 'feat/ui-phase2' in top bar")
	}
	tester.ClickAt(gitBranchRect.X+gitBranchRect.W/2, gitBranchRect.Y+gitBranchRect.H/2)
	if _, foundFile := tester.Find("deploy.yaml"); !foundFile {
		t.Fatal("expected untracked deploy.yaml in Git status popover")
	}
	tester.Key(0, ui.KeyEscape) // close git popover

	// 2. Verify @file fuzzy autocomplete in composer + Tab completion
	app.input = "请审查 @man"
	tester.Frame()
	if !tester.HasText("manager.go") {
		t.Fatal("expected @man autocomplete popup to suggest manager.go")
	}
	if err := tester.Click("引用文件 internal/mgr/manager.go"); err != nil {
		t.Fatalf("click @file suggestion failed: %v", err)
	}
	if app.input != "请审查 @internal/mgr/manager.go " {
		t.Fatalf("expected @file insertion into composer input, got %q", app.input)
	}

	// 3. Verify native File Drop & Clipboard PNG Paste into composer attachments
	if !tester.DropFiles(680, 420, yamlPath) {
		t.Fatal("expected DropFiles onto center main to succeed")
	}
	tester.Frame()
	if len(app.pendingAttachments) != 1 || app.pendingAttachments[0].Attachment.Name != "deploy.yaml" {
		t.Fatalf("expected dropped deploy.yaml attachment, got %+v", app.pendingAttachments)
	}

	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, G: 80, B: 80, A: 255})
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)
	tester.SetClipboardData(transfer.New(transfer.NewItem(transfer.Bytes(transfer.PNG, pngBuf.Bytes()))))
	tester.Key(ui.Cmd, ui.KeyV)
	tester.Frame()
	if len(app.pendingAttachments) != 2 || app.pendingAttachments[1].Attachment.Kind != harness.AttachmentImage {
		t.Fatalf("expected Cmd+V screenshot paste to add image attachment, got %+v", app.pendingAttachments)
	}

	// 4. Verify structured unified diff & one-click rollback ("撤销修改") on edit tool card
	tools := app.mgr.Router().BuildTools(workDir)
	var editIdx = -1
	for i, tl := range tools {
		if tl.Name == "edit" {
			editIdx = i
			break
		}
	}
	args := map[string]any{
		"path":    "service.go",
		"oldText": "return \"v1\"",
		"newText": "return \"v2-updated\"",
	}
	_ = app.handleAgentEvent(context.Background(), agent.AgentEvent{
		Type:       agent.EvToolExecutionStart,
		ToolCallID: "call-edit-p2",
		ToolName:   "edit",
		Args:       args,
	})
	resEdit, err := tools[editIdx].Execute(context.Background(), "call-edit-p2", args, nil)
	if err != nil {
		t.Fatalf("edit execute: %v", err)
	}
	_ = app.handleAgentEvent(context.Background(), agent.AgentEvent{
		Type:       agent.EvToolExecutionEnd,
		ToolCallID: "call-edit-p2",
		ToolName:   "edit",
		Result:     resEdit,
	})
	tester.Frame()

	if !tester.HasText("+1") || !tester.HasText("-1") {
		t.Fatal("expected +1 / -1 diff line counters on edit tool card")
	}
	if err := tester.Click("撤销修改"); err != nil {
		t.Fatalf("expected clicking '撤销修改' button to succeed: %v", err)
	}
	tester.Frame()
	diskAfterRevert, _ := os.ReadFile(servicePath)
	if string(diskAfterRevert) != origService {
		t.Fatalf("expected service.go restored to %q after clicking 撤销修改, got %q", origService, string(diskAfterRevert))
	}
	if !tester.HasText("已撤销") {
		t.Fatal("expected '已撤销' badge after rolling back tool mutation")
	}

	// 5. Verify Cmd+K Command Palette & /compact slash command
	tester.Key(ui.Cmd, ui.KeyK)
	if !app.cmdPalette.Open {
		t.Fatal("expected Cmd+K to open Command Palette")
	}
	app.cmdPalette.Query = "管理 MCP 工具服务"
	tester.Frame()
	tester.Key(0, ui.KeyEnter)
	if app.cmdPalette.Open || !app.settingsModal.Open || app.settingsModal.ActiveTab != 3 {
		t.Fatalf("expected Command Palette Enter on '管理 MCP 工具服务' to open Settings MCP tab (paletteOpen=%v settingsOpen=%v tab=%d)", app.cmdPalette.Open, app.settingsModal.Open, app.settingsModal.ActiveTab)
	}
	app.settingsModal.Open = false

	// Run one real LLM turn and then trigger /compact slash command
	if _, err := app.mgr.Run(context.Background(), "总结当前服务版本"); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	app.entries = append(app.entries, &ChatEntry{ID: app.allocID(), Kind: EntryUser, Text: "总结当前服务版本", Done: true})
	app.submitPromptWithContext(nil, "/compact")
	tester.Frame()
	if !tester.HasText("已完成上下文压缩检查点") {
		t.Fatal("expected /compact checkpoint message in timeline")
	}
}

func TestPhase3InlineBashBangMCPExportAndDiagnostics(t *testing.T) {
	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("PIUI_SESSION_DIR", filepath.Join(t.TempDir(), "sessions"))
	t.Setenv("PIUI_LOG_DIR", filepath.Join(t.TempDir(), "logs"))
	t.Setenv("PIUI_DISABLE_KEYCHAIN", "1")

	// 1. Mock Streamable HTTP MCP server exposing `fetch_spec`
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      *req.ID,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"serverInfo":      map[string]any{"name": "spec-mcp", "version": "1.0.0"},
				},
			})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      *req.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{
							"name":        "fetch_spec",
							"description": "Fetch architecture spec section",
							"inputSchema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"section": map[string]any{"type": "string"},
								},
								"required": []string{"section"},
							},
						},
					},
				},
			})
		case "tools/call":
			args, _ := req.Params["arguments"].(map[string]any)
			sec, _ := args["section"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      *req.ID,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": "SPEC_DATA_OK:" + sec},
					},
				},
			})
		}
	}))
	defer mcpSrv.Close()

	// 2. Mock OpenAI Chat SSE server that calls the bridged MCP tool `mcp__docs__fetch_spec` on first turn
	turnIdx := 0
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turnIdx++
		w.Header().Set("Content-Type", "text/event-stream")
		if turnIdx == 1 {
			chunk, _ := json.Marshal(map[string]any{
				"id":      "chatcmpl-mcp-1",
				"object":  "chat.completion.chunk",
				"created": 1700000000,
				"model":   "gpt-4.1",
				"choices": []any{
					map[string]any{
						"index": 0,
						"delta": map[string]any{
							"role": "assistant",
							"tool_calls": []any{
								map[string]any{
									"index": 0,
									"id":    "call-mcp-1",
									"type":  "function",
									"function": map[string]any{
										"name":      "mcp__docs__fetch_spec",
										"arguments": `{"section":"auth-v2"}`,
									},
								},
							},
						},
						"finish_reason": "tool_calls",
					},
				},
				"usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 20, "total_tokens": 140},
			})
			_, _ = w.Write([]byte("data: " + string(chunk) + "\n\ndata: [DONE]\n\n"))
			return
		}
		chunk, _ := json.Marshal(map[string]any{
			"id":      "chatcmpl-mcp-2",
			"object":  "chat.completion.chunk",
			"created": 1700000001,
			"model":   "gpt-4.1",
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         map[string]any{"role": "assistant", "content": "已通过 MCP 读取规范章节 auth-v2。"},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{"prompt_tokens": 180, "completion_tokens": 18, "total_tokens": 198},
		})
		_, _ = w.Write([]byte("data: " + string(chunk) + "\n\ndata: [DONE]\n\n"))
	}))
	defer llmSrv.Close()

	workDir := t.TempDir()
	app, err := NewDeskApp(workDir)
	if err != nil {
		t.Fatalf("NewDeskApp: %v", err)
	}
	defer func() { _ = app.Close() }()

	app.cfg.ActiveProfileID = "openai-chat"
	prof := app.cfg.ActiveProfile()
	prof.BaseURL = llmSrv.URL + "/v1"
	prof.APIKey = "test-key"
	prof.ModelID = "gpt-4.1"
	_ = app.mgr.ApplyProfile(prof)

	// 3. Hot-sync MCP server into DeskApp & verify top-bar MCP · 1 pill
	mcpCfg := app.cfg.UpsertMCPServer(store.MCPServerConfig{
		Name:      "docs",
		Transport: store.MCPTransportHTTP,
		Enabled:   true,
		URL:       mcpSrv.URL,
	})
	statuses := app.mgr.SyncMCPServers(context.Background(), []store.MCPServerConfig{mcpCfg})
	if len(statuses) != 1 || !statuses[0].Connected || app.mgr.MCPActiveToolCount() != 1 {
		t.Fatalf("expected MCP server connected with 1 tool, got statuses=%+v", statuses)
	}

	tester := ui.NewTester(app.View, 1360, 840)
	if !tester.HasText("MCP · 1") {
		t.Fatal("expected top-bar 'MCP · 1' badge when active MCP tools are mounted")
	}

	// 4. Verify Agent end-to-end tool call into MCP server + UI tool card rendering (`mcp:docs/fetch_spec`)
	if _, err := app.mgr.Run(context.Background(), "请查询 auth-v2 规范"); err != nil {
		t.Fatalf("Agent Run with MCP tool failed: %v", err)
	}
	tester.Frame()
	if !tester.HasText("mcp:docs/fetch_spec") {
		t.Fatal("expected MCP tool card label 'mcp:docs/fetch_spec' in chat timeline")
	}
	if !tester.HasText("已通过 MCP 读取规范章节 auth-v2") {
		t.Fatal("expected final assistant response after MCP tool execution")
	}

	// 5. Verify live streaming tail preview while bash tool is running + auto-expand on error
	_ = app.handleAgentEvent(context.Background(), agent.AgentEvent{
		Type:       agent.EvToolExecutionStart,
		ToolCallID: "tc-live-bash",
		ToolName:   "bash",
		Args:       map[string]any{"command": "go test ./..."},
	})
	_ = app.handleAgentEvent(context.Background(), agent.AgentEvent{
		Type:          agent.EvToolExecutionUpdate,
		ToolCallID:    "tc-live-bash",
		ToolName:      "bash",
		PartialResult: "\x1b[36m=== RUN   TestGateway\x1b[0m\n--- FAIL: TestGateway (0.04s)\n",
	})
	tester.Frame()
	if !tester.HasText("=== RUN   TestGateway") {
		t.Fatal("expected running bash card to render live streaming tail preview inline (without ANSI codes)")
	}
	_ = app.handleAgentEvent(context.Background(), agent.AgentEvent{
		Type:       agent.EvToolExecutionEnd,
		ToolCallID: "tc-live-bash",
		ToolName:   "bash",
		IsError:    true,
		Result:     "\x1b[31mFAIL\tgateway/auth\x1b[0m\n",
	})
	if !app.toolCards["tc-live-bash"].CardOpen {
		t.Fatal("expected failed bash tool card to auto-expand (CardOpen=true) on EvToolExecutionEnd")
	}

	// 6. Verify inline `! <command>` direct shell execution in composer
	app.submitPromptWithContext(nil, "!printf '\\033[32mINLINE_BANG_SHELL_OK\\033[0m\\n'")
	tester.Frame()
	if !tester.HasText("INLINE_BANG_SHELL_OK") {
		t.Fatal("expected '! <cmd>' direct shell output rendered inline in bash tool card")
	}

	// 7. Verify `/export <path>` Markdown export & Diagnostic report generation
	exportPath := filepath.Join(workDir, "exported-session.md")
	app.submitPromptWithContext(nil, "/export "+exportPath)
	mdBytes, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("expected /export to write %s: %v", exportPath, err)
	}
	mdStr := string(mdBytes)
	if !strings.Contains(mdStr, "mcp:docs/fetch_spec") || !strings.Contains(mdStr, "INLINE_BANG_SHELL_OK") {
		t.Fatalf("expected exported Markdown to include MCP tool call and bang shell output, got:\n%s", mdStr)
	}

	diagReport := app.CopyDiagnosticSummary(nil)
	if !strings.Contains(diagReport, "`1` servers configured (`1` tools active)") {
		t.Fatalf("expected diagnostic report to include active MCP server/tool count, got:\n%s", diagReport)
	}
}

func TestSidebarWorkspaceSessionTreeOverflowAndCrossWorkspaceOpen(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("PIUI_SESSION_DIR", "")
	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("PIUI_DISABLE_KEYCHAIN", "1")

	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	if got := harness.FormatRelativeAge(now.Add(-4*time.Minute), now); got != "4m" {
		t.Fatalf("expected 4m, got %q", got)
	}
	if got := harness.FormatRelativeAge(now.Add(-21*time.Hour), now); got != "21h" {
		t.Fatalf("expected 21h, got %q", got)
	}
	if got := harness.FormatRelativeAge(now.Add(-49*time.Hour), now); got != "2d" {
		t.Fatalf("expected 2d, got %q", got)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	dirAlpha := filepath.Join(t.TempDir(), "alpha-repo")
	dirBeta := filepath.Join(t.TempDir(), "beta-repo")
	if err := os.MkdirAll(dirAlpha, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirBeta, 0o755); err != nil {
		t.Fatal(err)
	}

	app, err := NewDeskApp(dirAlpha)
	if err != nil {
		t.Fatalf("NewDeskApp: %v", err)
	}
	defer func() { _ = app.Close() }()

	gwProf := store.ProviderProfile{
		ID:      "tree-gw",
		Name:    "TreeGW",
		Format:  store.FormatOpenAICompletions,
		BaseURL: srv.URL + "/v1",
		APIKey:  "sk-tree",
		ModelID: "gpt-4.1",
	}
	app.cfg.Profiles = []store.ProviderProfile{gwProf}
	app.switchChannel(nil, gwProf.ID)

	wsAlpha := *app.cfg.ActiveTarget()
	wsBeta := app.cfg.UpsertLocalTarget(dirBeta, "beta-repo", false)
	_ = app.cfg.UpsertSSHTarget("deploy@rsrelay", "/srv/rsrelay", "rsrelay", false)

	// Create 1 session in alpha-repo
	if _, err := app.mgr.Run(context.Background(), "Alpha 网关协议设计"); err != nil {
		t.Fatalf("Run in alpha: %v", err)
	}

	// Switch to beta-repo and create 8 distinct sessions to verify the "查看全部 (8)" overflow toggle
	app.SwitchWorkspace(nil, wsBeta)
	for idx := 1; idx <= 8; idx++ {
		if idx > 1 {
			app.startNewSession(nil)
		}
		prompt := "Beta 迭代任务 " + string(rune('A'+idx-1))
		if _, err := app.mgr.Run(context.Background(), prompt); err != nil {
			t.Fatalf("Run %d in beta: %v", idx, err)
		}
	}
	app.refreshSessionsList()

	tester := ui.NewTester(app.View, 1360, 840)

	// 1. Both workspaces and their nested sessions appear together in the unified tree!
	if !tester.HasText("alpha-repo") || !tester.HasText("beta-repo") || !tester.HasText("rsrelay") {
		t.Fatal("expected all workspaces (local & manually added SSH) rendered in unified tree")
	}
	if !tester.HasText("Alpha 网关协议设计") {
		t.Fatal("expected alpha-repo's session nested directly under alpha-repo even while beta-repo is active")
	}

	// 2. beta-repo has 8 sessions -> shows top 6 + "查看全部 (8)", then expands on click
	if !tester.HasText("查看全部 (8)") {
		t.Fatal("expected '查看全部 (8)' overflow button when workspace has >6 sessions")
	}
	if err := tester.Click("查看全部 (8)"); err != nil {
		t.Fatalf("Click '查看全部 (8)': %v", err)
	}
	tester.Frame()
	if !tester.HasText("收起") || !tester.HasText("Beta 迭代任务 A") || !tester.HasText("Beta 迭代任务 H") {
		t.Fatal("expected all 8 Beta sessions visible after clicking '查看全部 (8)'")
	}

	// 3. Clicking Alpha's session while Beta is active atomically switches workspace to Alpha and loads its history!
	alphaRect, found := tester.Find("Alpha 网关协议设计")
	if !found {
		t.Fatal("expected 'Alpha 网关协议设计' row in sidebar")
	}
	tester.ClickAt(alphaRect.X+alphaRect.W/2, alphaRect.Y+alphaRect.H/2)
	tester.Frame()

	if got := app.mgr.Router().Target().ID; got != wsAlpha.ID {
		t.Fatalf("expected active workspace to switch to alpha (%q) on session click, got %q", wsAlpha.ID, got)
	}
	if len(app.entries) == 0 || !strings.Contains(app.entries[0].Text, "Alpha 网关协议设计") {
		t.Fatalf("expected Alpha session conversation loaded into chat timeline, got %+v", app.entries)
	}
}
