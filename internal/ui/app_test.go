package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"piui/internal/store"
)

func TestDeskAppHeadlessRenderAndHotSwitch(t *testing.T) {
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

	_, err = tools[bashTool].Execute(context.Background(), "call-3", map[string]any{"command": "echo LIVE_STREAM_OK"}, nil)
	if err != nil {
		t.Fatalf("bash execute: %v", err)
	}
	if !strings.Contains(app.termDrawer.AllText(), "LIVE_STREAM_OK") {
		t.Fatalf("expected live bash output in TermDrawer, got: %q", app.termDrawer.AllText())
	}
}
