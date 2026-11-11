package harness

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/coding"
)

// SavedSessionSummary enriches coding.SessionInfo with a human-readable conversation title
// extracted from the first user prompt in the JSONL file and compact relative age metadata.
type SavedSessionSummary struct {
	ID           string
	Path         string
	Title        string
	Timestamp    string
	RelativeTime string
	UpdatedAt    time.Time
	Messages     int
}

// ProjectedTurnMessage pairs a reconstructed AgentMessage with its JSONL tree provenance
// so UI entries can trigger exact node checkout or forking.
type ProjectedTurnMessage struct {
	EntryID         string // Safe checkout EntryID (includes trailing toolResults if this message made tool calls)
	RawEntryID      string // Exact EntryID of this message in the JSONL file
	ParentEntryID   string // Parent EntryID right before this message (used for rewind-and-edit on user prompts)
	Message         agent.AgentMessage
	IsCompactionSum bool
	TargetName      string // Inferred workspace target short name at this turn (if switched in-session)
	IsRemote        bool   // Inferred remote SSH state at this turn
	HasTargetHint   bool
}

// SessionTreeNode represents a visible node in the Left Sidebar's Session Branch Tree.
type SessionTreeNode struct {
	EntryID        string // Safe checkout EntryID when clicking this node
	RawEntryID     string // Raw JSONL entry ID
	ParentEntryID  string // Raw parent entry ID
	Role           string // "user", "assistant", "compaction"
	Label          string // Single-line summary for tree display
	Timestamp      string
	Depth          int  // Indentation depth (increments at fork points)
	BranchIndex    int  // 1-based branch index when parent is a fork point (0 if linear)
	TotalBranches  int  // Total sibling branches at this fork point
	IsForkPoint    bool // True if >1 visible branches diverge after this node
	IsLeaf         bool // True if this node is at the tip of a branch
	OnActiveBranch bool // True if this node lies on the active root->leaf path
	IsActiveCursor bool // True if this node is the current active leaf cursor
	ToolCallCount  int  // Number of tool calls executed in this step
}

// SessionTreeSnapshot holds the parsed visual branch tree and branch statistics for the UI.
type SessionTreeSnapshot struct {
	SessionID    string
	Path         string
	ActiveLeafID string
	BranchCount  int // Number of distinct branch leaves
	TotalEntries int
	Nodes        []SessionTreeNode
}

// ExtractProjectedTurnMessages converts a coding.BranchProjection into ProjectedTurnMessages
// with safe checkout EntryIDs and ParentEntryIDs.
func ExtractProjectedTurnMessages(tree *coding.SessionTree, proj coding.BranchProjection) []ProjectedTurnMessage {
	// Build a lookup of raw entry -> trailing toolResult entry ID if the entry is an AssistantMessage with tool calls.
	safeCheckoutID := buildSafeCheckoutMap(tree)

	var out []ProjectedTurnMessage
	var curTargetName string
	var curIsRemote bool
	var hasHint bool

	for _, pe := range proj.Entries {
		if pe.SourceEntry == nil || len(pe.Messages) == 0 {
			continue
		}
		src := pe.SourceEntry
		checkoutID := safeCheckoutID[src.ID]
		if checkoutID == "" {
			checkoutID = src.ID
		}
		isCompaction := src.Type == "compaction"
		for _, msg := range pe.Messages {
			if msg == nil || msg.MessageRole() == ai.RoleSystem {
				continue
			}
			if msg.MessageRole() == ai.RoleUser {
				rawTxt := extractUserMessageRawText(msg)
				if tName, rem, ok := parseSwitchNoticeTarget(rawTxt); ok {
					curTargetName = tName
					curIsRemote = rem
					hasHint = true
				}
			}
			out = append(out, ProjectedTurnMessage{
				EntryID:         checkoutID,
				RawEntryID:      src.ID,
				ParentEntryID:   src.ParentID,
				Message:         msg,
				IsCompactionSum: isCompaction,
				TargetName:      curTargetName,
				IsRemote:        curIsRemote,
				HasTargetHint:   hasHint,
			})
		}
	}
	return out
}

// BuildSessionTreeSnapshot constructs a visual branch tree from a coding.SessionTree.
// Linear sequences remain at the same indentation depth; indentation increases only when
// a conversation node forks into multiple branches.
func BuildSessionTreeSnapshot(tree *coding.SessionTree, activeLeafID string) *SessionTreeSnapshot {
	if tree == nil {
		return nil
	}
	effLeaf := activeLeafID
	if effLeaf == "" {
		effLeaf = tree.LeafID
	}

	// Mark all raw entry IDs on the active branch path.
	activeSet := map[string]bool{}
	if effLeaf != "" {
		for _, e := range tree.Branch(effLeaf) {
			activeSet[e.ID] = true
		}
	}

	safeCheckout := buildSafeCheckoutMap(tree)

	// Build raw children adjacency list.
	rawChildren := map[string][]*coding.SessionEntry{}
	byID := map[string]*coding.SessionEntry{}
	for _, e := range tree.Entries {
		byID[e.ID] = e
		rawChildren[e.ParentID] = append(rawChildren[e.ParentID], e)
	}

	// Determine which entries are visible conversation nodes ("user", "assistant", "compaction").
	// Intermediate "toolResult", "system", "model_change", "thinking_level_change" are collapsed
	// into the tree structure while preserving their children and fork topology.
	type visNode struct {
		entry       *coding.SessionEntry
		checkoutID  string
		role        string
		label       string
		toolCalls   int
		children    []*visNode
		onActive    bool
		isCursor    bool
	}

	var collectVisibleChildren func(rawParentID string) []*visNode
	collectVisibleChildren = func(rawParentID string) []*visNode {
		var result []*visNode
		for _, child := range rawChildren[rawParentID] {
			role, label, toolCount, visible := classifySessionEntry(child)
			if !visible {
				// Pass through non-visible entry (e.g. system, model_change, toolResult) to its descendants.
				result = append(result, collectVisibleChildren(child.ID)...)
				continue
			}

			chkID := safeCheckout[child.ID]
			if chkID == "" {
				chkID = child.ID
			}

			// If this assistant node had tool calls whose toolResults were absorbed into chkID,
			// any visible children may hang off either child.ID or the absorbed toolResult chain.
			vn := &visNode{
				entry:      child,
				checkoutID: chkID,
				role:       role,
				label:      label,
				toolCalls:  toolCount,
				onActive:   activeSet[child.ID] || activeSet[chkID],
				isCursor:   effLeaf != "" && (child.ID == effLeaf || chkID == effLeaf),
			}
			vn.children = collectVisibleChildren(child.ID)
			result = append(result, vn)
		}
		return result
	}

	roots := collectVisibleChildren("")

	// Flatten into DFS visual order, incrementing depth only when a node forks (len(children) > 1).
	var flat []SessionTreeNode
	leafCount := 0

	var walk func(nodes []*visNode, depth int, isForkSiblings bool)
	walk = func(nodes []*visNode, depth int, isForkSiblings bool) {
		total := len(nodes)
		for i, n := range nodes {
			isFork := len(n.children) > 1
			isLeaf := len(n.children) == 0
			if isLeaf {
				leafCount++
			}
			branchIdx := 0
			totalBranches := 1
			if isForkSiblings {
				branchIdx = i + 1
				totalBranches = total
			}

			flat = append(flat, SessionTreeNode{
				EntryID:        n.checkoutID,
				RawEntryID:     n.entry.ID,
				ParentEntryID:  n.entry.ParentID,
				Role:           n.role,
				Label:          n.label,
				Timestamp:      n.entry.Timestamp,
				Depth:          depth,
				BranchIndex:    branchIdx,
				TotalBranches:  totalBranches,
				IsForkPoint:    isFork,
				IsLeaf:         isLeaf,
				OnActiveBranch: n.onActive,
				IsActiveCursor: n.isCursor,
				ToolCallCount:  n.toolCalls,
			})

			nextDepth := depth
			if isFork {
				nextDepth = depth + 1
				if nextDepth > 6 {
					nextDepth = 6
				}
			}
			walk(n.children, nextDepth, isFork)
		}
	}

	walk(roots, 0, len(roots) > 1)
	if leafCount == 0 && len( tree.Leaves() ) > 0 {
		leafCount = len(tree.Leaves())
	}

	return &SessionTreeSnapshot{
		SessionID:    tree.Header.ID,
		Path:         tree.Header.Path,
		ActiveLeafID: effLeaf,
		BranchCount:  leafCount,
		TotalEntries: len(tree.Entries),
		Nodes:        flat,
	}
}

// buildSafeCheckoutMap maps each entry ID to a protocol-safe checkout entry ID.
// Specifically, if an AssistantMessage contains ToolCalls, checking out the AssistantMessage
// without its immediately following ToolResultMessage(s) would violate LLM API contracts.
// Thus, its safe checkout target advances through its linear chain of ToolResultMessages.
func buildSafeCheckoutMap(tree *coding.SessionTree) map[string]string {
	out := map[string]string{}
	if tree == nil {
		return out
	}

	children := map[string][]*coding.SessionEntry{}
	for _, e := range tree.Entries {
		out[e.ID] = e.ID
		children[e.ParentID] = append(children[e.ParentID], e)
	}

	for _, e := range tree.Entries {
		if e.Type != "message" || e.Message == nil || e.Message.MessageRole() != ai.RoleAssistant {
			continue
		}
		cur := e.ID
		for {
			kids := children[cur]
			if len(kids) == 0 {
				break
			}
			// Follow the first child if it is a toolResult message.
			var trChild *coding.SessionEntry
			for _, k := range kids {
				if k.Type == "message" && k.Message != nil && k.Message.MessageRole() == ai.RoleToolResult {
					trChild = k
					break
				}
			}
			if trChild == nil {
				break
			}
			cur = trChild.ID
		}
		out[e.ID] = cur
	}
	return out
}

func classifySessionEntry(e *coding.SessionEntry) (role, label string, toolCalls int, visible bool) {
	if e == nil {
		return "", "", 0, false
	}
	switch e.Type {
	case "compaction":
		sum := truncateOneLine(e.Summary, 42)
		if sum == "" {
			sum = "上下文自动压缩节点"
		}
		return "compaction", "[Compaction] " + sum, 0, true

	case "branch_summary":
		sum := truncateOneLine(e.Summary, 42)
		return "compaction", "[分支摘要] " + sum, 0, true

	case "message":
		if e.Message == nil {
			return "", "", 0, false
		}
		switch m := e.Message.(type) {
		case ai.UserMessage:
			txt := stripSystemSwitchNotice(ai.ContentText(m.Content, " "))
			if strings.TrimSpace(txt) == "" {
				return "", "", 0, false
			}
			return "user", truncateOneLine(txt, 44), 0, true

		case *ai.UserMessage:
			if m == nil {
				return "", "", 0, false
			}
			txt := stripSystemSwitchNotice(ai.ContentText(m.Content, " "))
			if strings.TrimSpace(txt) == "" {
				return "", "", 0, false
			}
			return "user", truncateOneLine(txt, 44), 0, true

		case ai.AssistantMessage:
			return summarizeAssistantTreeEntry(&m)

		case *ai.AssistantMessage:
			return summarizeAssistantTreeEntry(m)
		}
	}
	return "", "", 0, false
}

func summarizeAssistantTreeEntry(am *ai.AssistantMessage) (role, label string, toolCalls int, visible bool) {
	if am == nil {
		return "", "", 0, false
	}
	var textParts []string
	var toolNames []string
	for _, block := range am.Content {
		switch b := block.(type) {
		case ai.TextContent:
			if t := strings.TrimSpace(b.Text); t != "" {
				textParts = append(textParts, t)
			}
		case ai.ToolCall:
			toolCalls++
			toolNames = append(toolNames, b.Name)
		}
	}

	txt := strings.Join(textParts, " ")
	if txt != "" {
		if toolCalls > 0 {
			return "assistant", fmt.Sprintf("%s (%d tools)", truncateOneLine(txt, 34), toolCalls), toolCalls, true
		}
		return "assistant", truncateOneLine(txt, 44), 0, true
	}
	if toolCalls > 0 {
		return "assistant", fmt.Sprintf("调用工具: %s", strings.Join(toolNames, ", ")), toolCalls, true
	}
	if am.ErrorMessage != "" {
		return "assistant", "[Error] " + truncateOneLine(am.ErrorMessage, 38), 0, true
	}
	return "", "", 0, false
}

func stripSystemSwitchNotice(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[System Notice: Active execution environment switched") {
		if idx := strings.Index(s, "]\n\n"); idx != -1 {
			return strings.TrimSpace(s[idx+3:])
		}
	}
	return s
}

func extractUserMessageRawText(msg agent.AgentMessage) string {
	switch m := msg.(type) {
	case ai.UserMessage:
		return ai.ContentText(m.Content, " ")
	case *ai.UserMessage:
		if m != nil {
			return ai.ContentText(m.Content, " ")
		}
	}
	return ""
}

func parseSwitchNoticeTarget(raw string) (targetName string, isRemote bool, ok bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "[System Notice: Active execution environment switched to ") {
		return "", false, false
	}
	end := strings.Index(raw, "]")
	if end == -1 {
		return "", false, false
	}
	header := raw[:end]
	if strings.Contains(header, "Remote SSH host ") {
		q1 := strings.Index(header, `"`)
		if q1 != -1 {
			q2 := strings.Index(header[q1+1:], `"`)
			if q2 != -1 {
				return header[q1+1 : q1+1+q2], true, true
			}
		}
		return "remote", true, true
	}
	if strings.Contains(header, "Local workspace") {
		q1 := strings.Index(header, `"`)
		if q1 != -1 {
			q2 := strings.Index(header[q1+1:], `"`)
			if q2 != -1 {
				wd := header[q1+1 : q1+1+q2]
				return filepath.Base(wd), false, true
			}
		}
		return "local", false, true
	}
	return "", false, false
}

// ExtractSessionTitle reads a JSONL session tree and extracts the first user prompt as a concise title.
func ExtractSessionTitle(sessionPath string) string {
	if strings.TrimSpace(sessionPath) == "" {
		return "新建对话"
	}
	tree, err := coding.LoadSessionTree(sessionPath)
	if err != nil || tree == nil {
		return "历史会话"
	}
	for _, e := range tree.Entries {
		if e == nil || e.Type != "message" || e.Message == nil {
			continue
		}
		if e.Message.MessageRole() == ai.RoleUser {
			raw := extractUserMessageRawText(e.Message)
			clean := stripSystemSwitchNotice(raw)
			if clean != "" {
				return truncateOneLine(clean, 34)
			}
		}
	}
	return "新建对话"
}

// FormatSessionTimestamp converts an RFC3339 timestamp to "YYYY-MM-DD HH:MM" for sidebar cards.
func FormatSessionTimestamp(ts string) string {
	ts = strings.TrimSpace(ts)
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Local().Format("2006-01-02 15:04")
	}
	if len(ts) >= 16 {
		return strings.ReplaceAll(ts[:16], "T", " ")
	}
	return ts
}

// FormatRelativeAge formats t relative to now using compact sidebar units ("now", "4m", "21h", "2d", "3mo", "1y").
func FormatRelativeAge(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	if d < time.Minute {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	days := int(d / (24 * time.Hour))
	if days < 30 {
		return fmt.Sprintf("%dd", days)
	}
	if days < 365 {
		return fmt.Sprintf("%dmo", max(days/30, 1))
	}
	return fmt.Sprintf("%dy", max(days/365, 1))
}
