package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"

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
	var readTool, bashTool int = -1, -1
	for i, tl := range tools {
		switch tl.Name {
		case "read":
			readTool = i
		case "bash":
			bashTool = i
		}
	}
	if readTool == -1 || bashTool == -1 {
		t.Fatalf("missing read or bash tool")
	}

	resA, err := tools[readTool].Execute(context.Background(), "call-1", map[string]any{"path": "a.txt"}, nil)
	if err != nil || !strings.Contains(ExtractResultText(resA), "hello from dir A") {
		t.Fatalf("expected content from dirA, got err=%v text=%q", err, ExtractResultText(resA))
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
}
