package harness

import (
	"path/filepath"
	"testing"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/coding"

	"piui/internal/store"
)

func TestSessionTreeBranchingAndCheckout(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))

	workDir := t.TempDir()
	cfg := store.LoadOrInit(workDir)

	mgr, err := NewManager(workDir, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Simulate recording a multi-turn session with a tool call and a branch fork:
	// U1 -> A1(toolCall) -> TR1(toolResult) -> A2(final text)
	rec := mgr.rec
	if rec == nil {
		t.Fatal("expected active SessionRecorder")
	}

	u1ID := rec.RecordMessage(ai.NewUserText("Check server status", 1000))
	a1ID := rec.RecordMessage(ai.AssistantMessage{
		Content: ai.ContentList{
			ai.TextContent{Text: "Let me run uptime."},
			ai.ToolCall{ID: "call-1", Name: "bash", Arguments: map[string]any{"command": "uptime"}},
		},
	})
	tr1ID := rec.RecordMessage(ai.ToolResultMessage{
		ToolCallID: "call-1",
		ToolName:   "bash",
		Content:    ai.ContentList{ai.TextContent{Text: "up 42 days"}},
	})
	a2ID := rec.RecordMessage(ai.AssistantMessage{
		Content: ai.ContentList{ai.TextContent{Text: "The server has been up for 42 days."}},
	})

	// Branch A continues linearly from a2ID: U2_branchA -> A3_branchA
	_ = rec.RecordMessage(ai.NewUserText("Now check disk space", 2000))
	branchALeaf := rec.RecordMessage(ai.AssistantMessage{
		Content: ai.ContentList{ai.TextContent{Text: "Disk usage is 35%."}},
	})

	// Verify tree snapshot before forking (1 branch)
	snap1 := mgr.SessionTreeSnapshot()
	if snap1 == nil {
		t.Fatal("expected non-nil SessionTreeSnapshot")
	}
	if snap1.BranchCount != 1 {
		t.Fatalf("expected BranchCount=1 before forking, got %d", snap1.BranchCount)
	}

	// Verify that A1 (which had a tool call) maps its safe checkout EntryID to tr1ID!
	var a1Node *SessionTreeNode
	for i := range snap1.Nodes {
		if snap1.Nodes[i].RawEntryID == a1ID {
			a1Node = &snap1.Nodes[i]
			break
		}
	}
	if a1Node == nil {
		t.Fatalf("expected to find node for a1ID=%s in %+v", a1ID, snap1.Nodes)
	}
	if a1Node.EntryID != tr1ID {
		t.Fatalf("expected tool-calling assistant node safe CheckoutID=%s (tr1ID), got %s", tr1ID, a1Node.EntryID)
	}

	// Now CheckoutBranch back to a2ID and record Branch B!
	msgsAtA2, _, err := mgr.CheckoutBranch(a2ID, cfg.ActiveProfile())
	if err != nil {
		t.Fatalf("CheckoutBranch(a2ID): %v", err)
	}
	// At a2ID, there should be U1, A1, TR1, A2 (4 non-system messages).
	if len(msgsAtA2) != 4 {
		t.Fatalf("expected 4 projected messages at a2ID, got %d", len(msgsAtA2))
	}
	if msgsAtA2[0].EntryID != u1ID {
		t.Fatalf("expected first message EntryID=%s, got %s", u1ID, msgsAtA2[0].EntryID)
	}

	// Record Branch B from a2ID
	_ = mgr.rec.RecordMessage(ai.NewUserText("Instead, check memory usage", 3000))
	branchBLeaf := mgr.rec.RecordMessage(ai.AssistantMessage{
		Content: ai.ContentList{ai.TextContent{Text: "Memory usage is 4.2GB / 16GB."}},
	})

	snap2 := mgr.SessionTreeSnapshot()
	if snap2 == nil || snap2.BranchCount != 2 {
		t.Fatalf("expected BranchCount=2 after forking, got %+v", snap2)
	}

	// Check that a2ID is marked IsForkPoint == true
	foundFork := false
	for _, n := range snap2.Nodes {
		if n.RawEntryID == a2ID && n.IsForkPoint {
			foundFork = true
			break
		}
	}
	if !foundFork {
		t.Fatalf("expected a2ID (%s) to be marked IsForkPoint in %+v", a2ID, snap2.Nodes)
	}

	// Switch back to Branch A leaf and verify last assistant text
	msgsA, _, err := mgr.CheckoutBranch(branchALeaf, cfg.ActiveProfile())
	if err != nil {
		t.Fatalf("CheckoutBranch(branchALeaf): %v", err)
	}
	lastA := msgsA[len(msgsA)-1].Message.(ai.AssistantMessage)
	if got := coding. FormatSize(len(ai.ContentText(lastA.Content, ""))); got == "" || ai.ContentText(lastA.Content, "") != "Disk usage is 35%." {
		t.Fatalf("unexpected Branch A tail message: %v", ai.ContentText(lastA.Content, ""))
	}

	// Switch to Branch B leaf and verify last assistant text
	msgsB, _, err := mgr.CheckoutBranch(branchBLeaf, cfg.ActiveProfile())
	if err != nil {
		t.Fatalf("CheckoutBranch(branchBLeaf): %v", err)
	}
	lastB := msgsB[len(msgsB)-1].Message.(ai.AssistantMessage)
	if ai.ContentText(lastB.Content, "") != "Memory usage is 4.2GB / 16GB." {
		t.Fatalf("unexpected Branch B tail message: %v", ai.ContentText(lastB.Content, ""))
	}

	// Verify ExtractSessionTitle and ListSavedSessionSummaries
	summaries := mgr.ListSavedSessionSummaries()
	if len(summaries) == 0 {
		t.Fatal("expected at least 1 saved session summary")
	}
	if summaries[0].Title != "Check server status" {
		t.Fatalf("expected session summary title 'Check server status', got %q", summaries[0].Title)
	}
}
