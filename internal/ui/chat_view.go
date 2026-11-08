package ui

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// EntryKind distinguishes user prompts, assistant markdown replies, and tool execution cards.
type EntryKind int

const (
	EntryUser EntryKind = iota
	EntryAssistant
	EntryTool
)

// ChatEntry represents one item in the central conversation timeline.
type ChatEntry struct {
	ID          int
	Kind        EntryKind
	Text        string
	Thinking    string
	ThinkOpen   bool
	Steered     bool
	TargetBadge string
	Done        bool
	ErrorText   string

	// Tool call fields
	ToolCallID  string
	ToolName    string
	ToolSummary string
	DiffOld     string
	DiffNew     string
	ToolOutput  string
	ToolRunning bool
	ToolError   bool
	DurationMs  int64
	CardOpen    bool
}

// IsEmpty returns true if an assistant entry has no text, thinking, or error content.
func (e *ChatEntry) IsEmpty() bool {
	return strings.TrimSpace(e.Text) == "" &&
		strings.TrimSpace(e.Thinking) == "" &&
		strings.TrimSpace(e.ErrorText) == ""
}

// SyncAssistantEntryFromMessage populates final text, thinking, and error message fields
// from a completed ai.AssistantMessage if they were not already populated via streaming deltas.
func SyncAssistantEntryFromMessage(entry *ChatEntry, msg agent.AgentMessage) {
	if entry == nil || msg == nil {
		return
	}
	var am *ai.AssistantMessage
	switch m := msg.(type) {
	case ai.AssistantMessage:
		am = &m
	case *ai.AssistantMessage:
		am = m
	}
	if am == nil {
		return
	}

	if am.ErrorMessage != "" && entry.ErrorText == "" {
		entry.ErrorText = am.ErrorMessage
	}
	if entry.Text == "" || entry.Thinking == "" {
		var textParts, thinkParts []string
		for _, block := range am.Content {
			switch b := block.(type) {
			case ai.TextContent:
				if strings.TrimSpace(b.Text) != "" {
					textParts = append(textParts, b.Text)
				}
			case ai.ThinkingContent:
				if strings.TrimSpace(b.Thinking) != "" {
					thinkParts = append(thinkParts, b.Thinking)
				}
			}
		}
		if entry.Text == "" && len(textParts) > 0 {
			entry.Text = strings.Join(textParts, "\n\n")
		}
		if entry.Thinking == "" && len(thinkParts) > 0 {
			entry.Thinking = strings.Join(thinkParts, "\n")
		}
	}
}

// RenderChatEntry renders a single ChatEntry inside the central list.
func RenderChatEntry(c *ui.Context, entry *ChatEntry, mdOpts *markdown.Options) {
	t := c.Theme()

	switch entry.Kind {
	case EntryUser:
		ui.Row(c).FillWidth().Children(func() {
			ui.Spacer(c)
			ui.Column(c).MaxWidth(640).Gap(4).Padding(10, 14).Radius(12).Background(t.Surface).Border(1, t.Border).Children(func() {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "You").Bold().FontSize(12).TextColor(t.Accent)
					if entry.Steered {
						ui.Badge(c, "⚡ Steer 插队")
					}
					if entry.TargetBadge != "" {
						ui.Text(c, entry.TargetBadge).FontSize(11).TextColor(t.TextMuted)
					}
				})
				ui.Text(c, entry.Text).Selectable()
			})
		})

	case EntryAssistant:
		ui.Column(c).FillWidth().MaxWidth(820).Gap(8).Children(func() {
			if entry.Thinking != "" {
				label := "💭 思考过程 (Reasoning)"
				if !entry.Done && entry.Text == "" {
					label = "💭 正在深度思考..."
				}
				ui.Collapsible(c, label, &entry.ThinkOpen, func() {
					ui.Box(c).Padding(8, 12).Radius(8).Background(t.Surface).Border(1, t.Border).Children(func() {
						ui.Text(c, entry.Thinking).FontSize(12).TextColor(t.TextMuted).Selectable()
					})
				})
			}

			if entry.Text != "" {
				if entry.Done {
					markdown.View(c, entry.Text, mdOpts)
				} else {
					markdown.Stream(c, entry.Text, mdOpts)
				}
			} else if !entry.Done && entry.Thinking == "" {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Spinner(c)
					ui.Text(c, "Agent 正在响应...").FontSize(13).TextColor(t.TextMuted)
				})
			}

			if entry.ErrorText != "" {
				ui.Box(c).Padding(10, 12).Radius(8).Background(ui.RGBA(239, 68, 68, 0.12)).Border(1, t.Danger).Children(func() {
					ui.Text(c, "⚠️ "+entry.ErrorText).TextColor(t.Danger).Selectable()
				})
			}
		})

	case EntryTool:
		statusIcon := "✅"
		statusSuffix := ""
		if entry.ToolRunning {
			statusIcon = "⚙️"
			statusSuffix = " · 执行中..."
		} else if entry.ToolError {
			statusIcon = "❌"
			statusSuffix = " · 失败"
		} else if entry.DurationMs > 0 {
			statusSuffix = fmt.Sprintf(" · %dms", entry.DurationMs)
		}

		header := fmt.Sprintf("%s [%s] %s%s", statusIcon, entry.ToolName, entry.ToolSummary, statusSuffix)
		if entry.TargetBadge != "" {
			header += "  (" + entry.TargetBadge + ")"
		}

		ui.Box(c).FillWidth().MaxWidth(820).Padding(8, 12).Radius(8).Background(t.Surface).Border(1, t.Border).Children(func() {
			ui.Collapsible(c, header, &entry.CardOpen, func() {
				ui.Column(c).Gap(6).Padding(6, 0, 0, 0).Children(func() {
					if entry.DiffOld != "" || entry.DiffNew != "" {
						ui.Text(c, "Diff 预览:").Bold().FontSize(12).TextColor(t.TextMuted)
						if entry.DiffOld != "" {
							ui.Box(c).FillWidth().Padding(6, 8).Radius(6).Background(ui.RGBA(239, 68, 68, 0.12)).Children(func() {
								ui.Text(c, "- "+truncatePreview(entry.DiffOld, 600)).Font("Menlo").FontSize(11).TextColor(ui.RGB(248, 113, 113)).Selectable()
							})
						}
						if entry.DiffNew != "" {
							ui.Box(c).FillWidth().Padding(6, 8).Radius(6).Background(ui.RGBA(34, 197, 94, 0.12)).Children(func() {
								ui.Text(c, "+ "+truncatePreview(entry.DiffNew, 600)).Font("Menlo").FontSize(11).TextColor(ui.RGB(74, 222, 128)).Selectable()
							})
						}
					}

					if entry.ToolOutput != "" {
						ui.Box(c).FillWidth().Padding(8, 10).Radius(6).Background(t.Background).Border(1, t.Border).Children(func() {
							ui.Text(c, truncatePreview(entry.ToolOutput, 1600)).
								Font("Menlo").
								FontSize(11).
								TextColor(t.TextMuted).
								Selectable()
						})
					}
				})
			})
		})
	}
}

// SummarizeToolArgs extracts a human-readable summary and optional edit diff from tool arguments.
func SummarizeToolArgs(toolName string, args map[string]any) (summary, diffOld, diffNew string) {
	if args == nil {
		return "", "", ""
	}
	switch toolName {
	case "bash":
		cmd, _ := args["command"].(string)
		cmd = strings.ReplaceAll(strings.TrimSpace(cmd), "\n", " ; ")
		return truncatePreview(cmd, 80), "", ""
	case "read", "write":
		p, _ := args["path"].(string)
		return p, "", ""
	case "edit":
		p, _ := args["path"].(string)
		oldStr, _ := args["oldText"].(string)
		newStr, _ := args["newText"].(string)
		if oldStr == "" && newStr == "" {
			if edits, ok := args["edits"].([]any); ok && len(edits) > 0 {
				if first, ok := edits[0].(map[string]any); ok {
					oldStr, _ = first["oldText"].(string)
					newStr, _ = first["newText"].(string)
				}
			}
		}
		return p, oldStr, newStr
	default:
		for _, k := range []string{"path", "command", "pattern"} {
			if v, ok := args[k].(string); ok && v != "" {
				return truncatePreview(v, 80), "", ""
			}
		}
		return "", "", ""
	}
}

// ExtractResultText extracts readable text from an AgentToolResult or raw event result.
func ExtractResultText(res any) string {
	switch v := res.(type) {
	case agent.AgentToolResult:
		return ai.ContentText(v.Content, "\n")
	case *agent.AgentToolResult:
		if v != nil {
			return ai.ContentText(v.Content, "\n")
		}
	case string:
		return v
	}
	return ""
}

// RebuildEntriesFromMessages converts a loaded JSONL transcript into UI ChatEntries.
func RebuildEntriesFromMessages(msgs []agent.AgentMessage, nextID *int) []*ChatEntry {
	var out []*ChatEntry
	toolMap := map[string]*ChatEntry{}

	allocID := func() int {
		*nextID++
		return *nextID
	}

	for _, m := range msgs {
		switch msg := m.(type) {
		case ai.UserMessage:
			txt := strings.TrimSpace(ai.ContentText(msg.Content, "\n"))
			if txt != "" {
				out = append(out, &ChatEntry{
					ID:   allocID(),
					Kind: EntryUser,
					Text: txt,
					Done: true,
				})
			}
		case *ai.UserMessage:
			if msg != nil {
				txt := strings.TrimSpace(ai.ContentText(msg.Content, "\n"))
				if txt != "" {
					out = append(out, &ChatEntry{
						ID:   allocID(),
						Kind: EntryUser,
						Text: txt,
						Done: true,
					})
				}
			}
		case ai.AssistantMessage:
			out = appendAssistantMsg(out, &msg, toolMap, allocID)
		case *ai.AssistantMessage:
			if msg != nil {
				out = appendAssistantMsg(out, msg, toolMap, allocID)
			}
		case ai.ToolResultMessage:
			if tc, ok := toolMap[msg.ToolCallID]; ok {
				tc.ToolOutput = ai.ContentText(msg.Content, "\n")
				tc.ToolError = msg.IsError
				tc.ToolRunning = false
			}
		case *ai.ToolResultMessage:
			if msg != nil {
				if tc, ok := toolMap[msg.ToolCallID]; ok {
					tc.ToolOutput = ai.ContentText(msg.Content, "\n")
					tc.ToolError = msg.IsError
					tc.ToolRunning = false
				}
			}
		}
	}
	return out
}

func appendAssistantMsg(out []*ChatEntry, am *ai.AssistantMessage, toolMap map[string]*ChatEntry, allocID func() int) []*ChatEntry {
	var textParts []string
	var thinkParts []string
	var toolCalls []ai.ToolCall

	for _, block := range am.Content {
		switch b := block.(type) {
		case ai.TextContent:
			if strings.TrimSpace(b.Text) != "" {
				textParts = append(textParts, b.Text)
			}
		case ai.ThinkingContent:
			if strings.TrimSpace(b.Thinking) != "" {
				thinkParts = append(thinkParts, b.Thinking)
			}
		case ai.ToolCall:
			toolCalls = append(toolCalls, b)
		}
	}

	if len(textParts) > 0 || len(thinkParts) > 0 || am.ErrorMessage != "" {
		out = append(out, &ChatEntry{
			ID:        allocID(),
			Kind:      EntryAssistant,
			Text:      strings.Join(textParts, "\n\n"),
			Thinking:  strings.Join(thinkParts, "\n"),
			Done:      true,
			ErrorText: am.ErrorMessage,
		})
	}

	for _, tc := range toolCalls {
		sum, dOld, dNew := SummarizeToolArgs(tc.Name, tc.Arguments)
		entry := &ChatEntry{
			ID:          allocID(),
			Kind:        EntryTool,
			ToolCallID:  tc.ID,
			ToolName:    tc.Name,
			ToolSummary: sum,
			DiffOld:     dOld,
			DiffNew:     dNew,
			Done:        true,
		}
		toolMap[tc.ID] = entry
		out = append(out, entry)
	}
	return out
}

// truncatePreview truncates s at a valid UTF-8 rune boundary up to maxRunes characters.
func truncatePreview(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "\n... (已截断)"
}
