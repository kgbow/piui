package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"

	"piui/internal/harness"
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
	ID              int
	Kind            EntryKind
	Text            string
	Thinking        string
	ThinkOpen       bool
	Steered         bool
	TargetBadge     string
	IsRemote        bool
	Done            bool
	ErrorText       string
	ErrorDetailOpen bool
	EntryID         string // JSONL tree entry ID for branching/checkout
	ParentEntryID   string // JSONL parent entry ID right before this message

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

// EntryTopSpacing computes context-aware vertical rhythm between consecutive timeline entries:
// generous breathing room between conversation turns, tight cohesion between consecutive tool steps.
func EntryTopSpacing(prevKind, curKind EntryKind, isFirst bool) float32 {
	if isFirst {
		return 0
	}
	switch {
	case curKind == EntryUser:
		return 16
	case prevKind == EntryUser:
		return 10
	case prevKind == EntryTool && curKind == EntryTool:
		return 6
	case prevKind == EntryAssistant && curKind == EntryTool:
		return 10
	case prevKind == EntryTool && curKind == EntryAssistant:
		return 14
	default:
		return 14
	}
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

// RenderChatEntry renders a single ChatEntry inside the central list without fork actions.
func RenderChatEntry(c *ui.Context, entry *ChatEntry, mdOpts *markdown.Options) {
	RenderChatEntryWithActions(c, PaletteFor(!c.Theme().Dark), entry, mdOpts, false, nil)
}

func toolVisualMeta(toolName string) (*ui.SVG, string) {
	switch toolName {
	case "bash":
		return IconTerminal, "bash"
	case "read":
		return IconFileText, "read"
	case "edit":
		return IconFileEdit, "edit"
	case "write":
		return IconFileEdit, "write"
	case "grep", "find":
		return IconSearch, toolName
	case "ls":
		return IconFolder, "ls"
	case "workspace_probe":
		return IconCompass, "probe"
	default:
		return IconCode, toolName
	}
}

// RenderChatEntryWithActions renders a single ChatEntry in the Antigravity quiet stream style.
func RenderChatEntryWithActions(c *ui.Context, p Palette, entry *ChatEntry, mdOpts *markdown.Options, canFork bool, onFork func(*ChatEntry)) {
	RenderChatTurnEntry(c, p, entry, mdOpts, canFork, true, onFork)
}

// RenderChatTurnEntry renders a single ChatEntry with optional suppression of the assistant turn-end footer
// when the entry is an intermediate intro step directly preceding tool execution cards.
func RenderChatTurnEntry(c *ui.Context, p Palette, entry *ChatEntry, mdOpts *markdown.Options, canFork bool, showTurnFooter bool, onFork func(*ChatEntry)) {
	isRemote := entry.IsRemote || strings.Contains(entry.TargetBadge, "SSH") || strings.Contains(entry.TargetBadge, "🌐")

	switch entry.Kind {
	case EntryUser:
		turnWrap := ui.Column(c).FillWidth().MinWidth(0).AlignItems(ui.End).Gap(4)
		showActions := turnWrap.Hovered() || turnWrap.FocusWithin()
		targetOp := float32(0.0)
		if showActions {
			targetOp = 1.0
		}
		actionOp := turnWrap.Animate("u-act", targetOp, 140*time.Millisecond)

		turnWrap.Children(func() {
			// Asymmetric refined prompt bubble
			ui.Column(c).MaxWidth(580).MinWidth(0).Padding(10, 14).
				Radius(RadiusLG, RadiusLG, 4, RadiusLG).
				Background(p.BgElevated).Border(1, p.BorderSubtle).Children(func() {
				ui.Text(c, entry.Text).FontSize(FontBody).LineHeight(1.55).TextColor(p.TextPrimary).MinWidth(0).Selectable()
			})

			// Quiet bottom-right metadata & hover-revealed actions
			hasBadges := entry.Steered || (isRemote && entry.TargetBadge != "")
			if hasBadges || entry.Text != "" || (canFork && onFork != nil && entry.EntryID != "") {
				ui.Row(c).Height(22).Gap(6).AlignItems(ui.Center).Padding(0, 2).Children(func() {
					if entry.Steered {
						ui.Row(c).Padding(1, 6).Radius(RadiusSM).Background(p.WarnSoft).Gap(3).AlignItems(ui.Center).
							Tooltip("运行中插队注入的纠偏指令").Children(func() {
							ui.Icon(c, IconBolt).Size(10, 10).TextColor(p.WarnColor)
							ui.Text(c, "Steer").FontWeight(WeightMedium).FontSize(FontCaption).TextColor(p.WarnColor)
						})
					}
					if isRemote && entry.TargetBadge != "" {
						ui.Row(c).Padding(1, 6).Radius(RadiusSM).Background(p.SSHSoft).Gap(4).AlignItems(ui.Center).Children(func() {
							ui.Icon(c, IconServer).Size(10, 10).TextColor(p.SSHColor)
							ui.Text(c, entry.TargetBadge).FontWeight(WeightMedium).FontSize(FontCaption).TextColor(p.SSHColor)
						})
					}

					ui.Row(c).Gap(2).AlignItems(ui.Center).Opacity(actionOp).Children(func() {
						IconTipBtn(c, p, IconCopy, 22, 12.5, "复制指令", func() {
							c.WriteClipboard(entry.Text)
							c.Toast("已复制指令到剪贴板")
						})
						if canFork && onFork != nil && entry.EntryID != "" {
							forkTip := fmt.Sprintf("回退至此处并重新编辑 (#%s)", shortEntryID(entry.EntryID))
							IconTipBtn(c, p, IconBranch, 22, 12.5, forkTip, func() {
								onFork(entry)
							})
						}
					})
				})
			}
		})

	case EntryAssistant:
		turnWrap := ui.Column(c).FillWidth().MaxWidth(760).MinWidth(0).Gap(8)
		showActions := turnWrap.Hovered() || turnWrap.FocusWithin()
		targetOp := float32(0.0)
		if showActions {
			targetOp = 1.0
		}
		actionOp := turnWrap.Animate("a-act", targetOp, 140*time.Millisecond)

		turnWrap.Children(func() {
			// Collapsible Thinking Bar — Quiet & Refined
			if entry.Thinking != "" {
				tb := ui.ButtonBase(c).FillWidth().MinWidth(0).Padding(4, 6).Radius(RadiusSM).Gap(6).AlignItems(ui.Center)
				targetBg := ui.Transparent
				if tb.Hovered() || tb.Pressed() {
					targetBg = p.BgHover
				}
				chev := IconChevronRight
				if entry.ThinkOpen {
					chev = IconChevronDown
				}
				isActivelyThinking := !entry.Done && entry.Text == ""
				title := "思考过程"
				sparkCol := p.TextMuted
				if isActivelyThinking {
					title = "正在思考..."
					sparkCol = p.Accent
				}
				tb.Background(targetBg).Children(func() {
					ui.Icon(c, IconSparkles).Size(12.5, 12.5).TextColor(sparkCol).Shrink(0)
					ui.Text(c, title).FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextSecondary).Shrink(0)
					if !entry.ThinkOpen {
						preview := singleLinePreview(entry.Thinking, 64)
						if preview != "" {
							ui.Text(c, preview).FontSize(FontSmall).TextColor(p.TextMuted).
								Grow(1).MinWidth(0).SingleLine().Ellipsis("...")
						}
					} else {
						ui.Spacer(c)
					}
					ui.Icon(c, chev).Size(12, 12).TextColor(p.TextMuted).Shrink(0)
				})
				if tb.Clicked() {
					entry.ThinkOpen = !entry.ThinkOpen
				}

				if entry.ThinkOpen {
					// Clean left-quote bar instead of a heavy nested box
					ui.Column(c).FillWidth().MinWidth(0).Margin(0, 0, 2, 6).Padding(4, 0, 4, 12).
						BorderWidth(0, 0, 0, 2).BorderColor(p.BorderStrong).Children(func() {
						ui.Text(c, entry.Thinking).FontSize(FontSmall).LineHeight(1.58).TextColor(p.TextSecondary).
							FillWidth().MinWidth(0).Selectable()
					})
				}
			}

			// Prose Markdown Body (.prose)
			if entry.Text != "" {
				if entry.Done {
					markdown.View(c, entry.Text, mdOpts)
				} else {
					markdown.Stream(c, entry.Text, mdOpts)
				}
			} else if !entry.Done && entry.Thinking == "" {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(4, 2).Children(func() {
					ui.Spinner(c).Size(13, 13)
					ui.Text(c, "正在思考并组织响应...").FontSize(FontSmall).TextColor(p.TextMuted)
				})
			}

			// Refined API / Execution Error Card
			if entry.ErrorText != "" {
				parsedErr := ParseAPIError(entry.ErrorText)
				ui.Column(c).FillWidth().MinWidth(0).Padding(10, 12).Radius(RadiusMD).
					Background(p.DangerSoft).Border(1, p.DangerColor.Alpha(0.38)).Gap(6).Children(func() {
					ui.Row(c).FillWidth().MinWidth(0).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Icon(c, IconShieldAlert).Size(14.5, 14.5).TextColor(p.DangerColor).Shrink(0)
						ui.Text(c, parsedErr.Title).FontWeight(WeightSemibold).FontSize(FontSmall).TextColor(p.DangerColor).
							Grow(1).MinWidth(0).Selectable()
						IconTipBtn(c, p, IconCopy, 24, 13, "复制完整报错信息", func() {
							c.WriteClipboard(entry.ErrorText)
							c.Toast("已复制完整报错信息到剪贴板")
						})
						if parsedErr.PrettyRaw != "" {
							detailTip := "查看原始响应详情"
							if entry.ErrorDetailOpen {
								detailTip = "收起原始响应详情"
							}
							ToolbarIconBtn(c, p, IconCode, 24, 13, entry.ErrorDetailOpen, ui.Transparent, detailTip, func() {
								entry.ErrorDetailOpen = !entry.ErrorDetailOpen
							})
						}
					})
					if parsedErr.Detail != "" {
						ui.Text(c, parsedErr.Detail).FontSize(FontSmall).LineHeight(1.5).TextColor(p.TextPrimary).
							FillWidth().MinWidth(0).Selectable()
					}
					if entry.ErrorDetailOpen && parsedErr.PrettyRaw != "" {
						ui.Column(c).FillWidth().MinWidth(0).Padding(8, 10).Radius(RadiusSM).
							Background(p.CodeBg).Border(1, p.BorderSubtle).Children(func() {
							ui.Text(c, truncatePreview(parsedErr.PrettyRaw, 1600)).
								Font(FontMono).FontSize(FontCaption).LineHeight(1.45).TextColor(p.TextSecondary).
								FillWidth().MinWidth(0).Selectable()
						})
					}
				})
			}

			// Quiet Hover-Revealed Turn Actions (Copy / Fork) — omitted on intermediate steps before tool cards
			if showTurnFooter && entry.Done && entry.Text != "" {
				ui.Row(c).Height(22).Gap(3).AlignItems(ui.Center).Opacity(actionOp).Children(func() {
					IconTipBtn(c, p, IconCopy, 22, 12.5, "复制回复", func() {
						c.WriteClipboard(entry.Text)
						c.Toast("已复制助手回复到剪贴板")
					})
					if canFork && onFork != nil && entry.EntryID != "" {
						forkTip := fmt.Sprintf("从此处分叉会话 (#%s)", shortEntryID(entry.EntryID))
						IconTipBtn(c, p, IconBranch, 22, 12.5, forkTip, func() {
							onFork(entry)
						})
					}
				})
			}
		})

	case EntryTool:
		toolIcon, toolLabel := toolVisualMeta(entry.ToolName)
		hasDiff := entry.DiffOld != "" || entry.DiffNew != ""
		hasBody := hasDiff || entry.ToolOutput != ""

		ui.Column(c).FillWidth().MaxWidth(760).MinWidth(0).Radius(RadiusMD).
			Background(p.BgSurface).Border(1, p.BorderSubtle).Clip().Children(func() {
			// Compact Semantic Tool Header (34px)
			hb := ui.ButtonBase(c).FillWidth().MinWidth(0).Height(34).Padding(0, 10).Gap(8).AlignItems(ui.Center)
			targetBg := ui.Transparent
			if hb.Hovered() || hb.Pressed() {
				targetBg = p.BgHover
			}
			hb.Background(targetBg).Children(func() {
				// Leading status / semantic icon
				if entry.ToolRunning {
					ui.Spinner(c).Size(13, 13)
				} else if entry.ToolError {
					ui.Icon(c, IconCross).Size(13.5, 13.5).TextColor(p.DangerColor).Shrink(0)
				} else {
					ui.Icon(c, toolIcon).Size(13.5, 13.5).TextColor(p.TextMuted).Shrink(0)
				}

				ui.Text(c, toolLabel).Font(FontMono).FontWeight(WeightMedium).FontSize(FontSmall).TextColor(p.TextPrimary).Shrink(0)

				if entry.ToolSummary != "" {
					ui.Text(c, singleLinePreview(entry.ToolSummary, 68)).
						Font(FontMono).FontSize(FontCaption + 0.5).TextColor(p.TextMuted).
						Grow(1).MinWidth(0).SingleLine().Ellipsis("...")
				} else {
					ui.Spacer(c)
				}

				// Remote SSH badge ONLY when remote (keeps local tool calls whisper-quiet)
				if isRemote && entry.TargetBadge != "" {
					ui.Row(c).Padding(1, 6).Radius(RadiusSM).Background(p.SSHSoft).Shrink(0).Gap(4).AlignItems(ui.Center).Children(func() {
						ui.Text(c, entry.TargetBadge).Font(FontMono).FontWeight(WeightMedium).FontSize(10).TextColor(p.SSHColor)
					})
				}

				if entry.ToolRunning {
					ui.Text(c, "执行中").FontSize(FontCaption).TextColor(p.Accent).Shrink(0)
				} else {
					if entry.DurationMs > 0 {
						ui.Text(c, fmt.Sprintf("%dms", entry.DurationMs)).
							FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextMuted).Shrink(0)
					}
					if !entry.ToolError {
						ui.Icon(c, IconCheck).Size(12, 12).TextColor(p.LocalColor).Shrink(0)
					}
				}

				if hasBody {
					chev := IconChevronRight
					if entry.CardOpen {
						chev = IconChevronDown
					}
					ui.Icon(c, chev).Size(12.5, 12.5).TextColor(p.TextMuted).Shrink(0)
				}
			})
			if hb.Clicked() && hasBody {
				entry.CardOpen = !entry.CardOpen
			}

			// Tool Card Body — Unified Single-Surface Code/Diff View (no multi-box nesting!)
			if entry.CardOpen && hasBody {
				ui.Column(c).FillWidth().MinWidth(0).Padding(10, 12).Gap(8).
					Background(p.CodeBg).
					BorderWidth(1, 0, 0, 0).BorderColor(p.BorderSubtle).Children(func() {
					if hasDiff {
						renderUnifiedDiffLines(c, p, entry.DiffOld, entry.DiffNew)
						if entry.ToolOutput != "" {
							outCol := p.TextMuted
							if entry.ToolError {
								outCol = p.DangerColor
							}
							ui.Text(c, truncatePreview(entry.ToolOutput, 400)).
								Font(FontMono).FontSize(FontCaption).LineHeight(1.45).TextColor(outCol).
								FillWidth().MinWidth(0).Selectable()
						}
					} else if entry.ToolOutput != "" {
						outCol := p.TextSecondary
						if entry.ToolError {
							outCol = p.DangerColor
						}
						ui.Text(c, truncatePreview(entry.ToolOutput, 1600)).
							Font(FontMono).FontSize(FontCaption + 0.5).LineHeight(1.48).TextColor(outCol).
							FillWidth().MinWidth(0).Selectable()
					}
				})
			}
		})
	}
}

// renderUnifiedDiffLines renders old/new edit hunks in a single cohesive IDE-style diff view.
func renderUnifiedDiffLines(c *ui.Context, p Palette, diffOld, diffNew string) {
	ui.Column(c).FillWidth().MinWidth(0).Radius(RadiusSM).
		Border(1, p.BorderSubtle).Clip().Children(func() {
		if diffOld != "" {
			oldText := truncatePreview(diffOld, 600)
			for _, line := range strings.Split(oldText, "\n") {
				ui.Row(c).FillWidth().MinWidth(0).Padding(2, 10).Background(p.DiffDelBg).Gap(8).Children(func() {
					ui.Text(c, "-").Font(FontMono).FontSize(FontCaption + 0.5).TextColor(p.DiffDelFg).Shrink(0).Unselectable()
					ui.Text(c, line).Font(FontMono).FontSize(FontCaption + 0.5).LineHeight(1.42).TextColor(p.DiffDelFg).
						Grow(1).MinWidth(0).PreserveSpaces().Selectable()
				})
			}
		}
		if diffNew != "" {
			newText := truncatePreview(diffNew, 600)
			for _, line := range strings.Split(newText, "\n") {
				ui.Row(c).FillWidth().MinWidth(0).Padding(2, 10).Background(p.DiffAddBg).Gap(8).Children(func() {
					ui.Text(c, "+").Font(FontMono).FontSize(FontCaption + 0.5).TextColor(p.DiffAddFg).Shrink(0).Unselectable()
					ui.Text(c, line).Font(FontMono).FontSize(FontCaption + 0.5).LineHeight(1.42).TextColor(p.DiffAddFg).
						Grow(1).MinWidth(0).PreserveSpaces().Selectable()
				})
			}
		}
	})
}

// ParsedAPIError holds a humanized breakdown of a raw LLM / gateway / Cloudflare error.
type ParsedAPIError struct {
	Title     string
	Detail    string
	PrettyRaw string
}

// FormatErrorHeadline returns a concise single-line summary suitable for top status alerts.
func FormatErrorHeadline(raw string) string {
	p := ParseAPIError(raw)
	if p.Detail != "" {
		return singleLinePreview(p.Title+" — "+p.Detail, 110)
	}
	return singleLinePreview(p.Title, 110)
}

// ParseAPIError extracts readable title and detail messages from raw HTTP/JSON/Cloudflare error strings.
func ParseAPIError(raw string) ParsedAPIError {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ParsedAPIError{Title: "请求失败"}
	}

	braceIdx := strings.Index(trimmed, "{")
	lastBrace := strings.LastIndex(trimmed, "}")
	if braceIdx != -1 && lastBrace > braceIdx {
		prefix := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(trimmed[:braceIdx]), ":"))
		jsonPart := trimmed[braceIdx : lastBrace+1]

		var obj map[string]any
		if err := json.Unmarshal([]byte(jsonPart), &obj); err == nil && len(obj) > 0 {
			prettyBytes, _ := json.MarshalIndent(obj, "", "  ")
			prettyStr := string(prettyBytes)

			// Unwrap nested {"error": {...}} if present
			targetMap := obj
			if nested, ok := obj["error"].(map[string]any); ok && len(nested) > 0 {
				targetMap = nested
			}

			msg := mapStr(targetMap, "message", "detail", "error_description")
			if msg == "" && targetMap != nil {
				msg = mapStr(obj, "message", "detail")
			}
			title := mapStr(targetMap, "title")
			if title == "" {
				title = mapStr(obj, "title")
			}
			code := mapStr(targetMap, "code", "error_name", "type")
			if code == "" {
				code = mapStr(obj, "code", "error_name")
			}
			zone := mapStr(obj, "zone")
			rayID := mapStr(obj, "ray_id")

			var headlineParts []string
			if prefix != "" {
				if len(prefix) == 3 && prefix[0] >= '1' && prefix[0] <= '5' {
					prefix = "HTTP " + prefix
				}
				if title == "" || !strings.Contains(strings.ToLower(title), strings.ToLower(prefix)) {
					headlineParts = append(headlineParts, prefix)
				}
			}
			if title != "" {
				headlineParts = append(headlineParts, title)
			}
			if zone != "" {
				headlineParts = append(headlineParts, zone)
			}
			if code != "" && !strings.HasPrefix(code, "http") {
				headlineParts = append(headlineParts, code)
			}

			headline := strings.Join(headlineParts, " · ")
			if headline == "" {
				headline = "接口请求返回错误"
			}

			detail := strings.TrimSpace(msg)
			if rayID != "" && !strings.Contains(detail, rayID) {
				if detail != "" {
					detail += fmt.Sprintf(" (Ray ID: %s)", rayID)
				} else {
					detail = "Ray ID: " + rayID
				}
			}
			if detail == headline {
				detail = ""
			}

			return ParsedAPIError{
				Title:     headline,
				Detail:    detail,
				PrettyRaw: prettyStr,
			}
		}
	}

	// Plain non-JSON error
	lines := strings.Split(trimmed, "\n")
	if len(lines) > 1 {
		return ParsedAPIError{
			Title:  singleLinePreview(lines[0], 96),
			Detail: strings.TrimSpace(strings.Join(lines[1:], "\n")),
		}
	}
	if len([]rune(trimmed)) > 120 {
		return ParsedAPIError{
			Title:  singleLinePreview(trimmed, 88),
			Detail: trimmed,
		}
	}
	return ParsedAPIError{Title: trimmed}
}

func mapStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch s := v.(type) {
			case string:
				if strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

func shortEntryID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

func singleLinePreview(s string, maxRunes int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
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
	case "ls":
		p, _ := args["path"].(string)
		if p == "" {
			p = "."
		}
		return p, "", ""
	case "find":
		pat, _ := args["pattern"].(string)
		p, _ := args["path"].(string)
		if p != "" {
			return fmt.Sprintf("%s (in %s)", pat, p), "", ""
		}
		return pat, "", ""
	case "grep":
		pat, _ := args["pattern"].(string)
		p, _ := args["path"].(string)
		if p != "" {
			return fmt.Sprintf("/%s/ in %s", pat, p), "", ""
		}
		return "/" + pat + "/", "", ""
	case "workspace_probe":
		sub, _ := args["subpath"].(string)
		if sub != "" {
			return "探针扫描: " + sub, "", ""
		}
		return "工作区系统与 Git 状态探针", "", ""
	default:
		for _, k := range []string{"path", "command", "pattern", "subpath"} {
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
	projected := make([]harness.ProjectedTurnMessage, 0, len(msgs))
	for _, m := range msgs {
		projected = append(projected, harness.ProjectedTurnMessage{Message: m})
	}
	return RebuildEntriesFromProjected(projected, nextID)
}

// RebuildEntriesFromProjected converts ProjectedTurnMessages (with JSONL tree EntryIDs) into UI ChatEntries.
func RebuildEntriesFromProjected(projected []harness.ProjectedTurnMessage, nextID *int) []*ChatEntry {
	var out []*ChatEntry
	toolMap := map[string]*ChatEntry{}

	allocID := func() int {
		*nextID++
		return *nextID
	}

	turnBadgeFor := func(pm harness.ProjectedTurnMessage, forToolCard bool) (string, bool) {
		if !pm.HasTargetHint || pm.TargetName == "" {
			return "", false
		}
		if pm.IsRemote {
			if forToolCard {
				return "SSH · " + pm.TargetName, true
			}
			return pm.TargetName + " (SSH)", true
		}
		if forToolCard {
			return "Local · " + pm.TargetName, false
		}
		return pm.TargetName + " (Local)", false
	}

	for _, pm := range projected {
		switch msg := pm.Message.(type) {
		case ai.UserMessage:
			txt := cleanUserMessageDisplay(ai.ContentText(msg.Content, "\n"))
			if txt != "" {
				badge, rem := turnBadgeFor(pm, false)
				out = append(out, &ChatEntry{
					ID:            allocID(),
					Kind:          EntryUser,
					Text:          txt,
					TargetBadge:   badge,
					IsRemote:      rem,
					Done:          true,
					EntryID:       pm.EntryID,
					ParentEntryID: pm.ParentEntryID,
				})
			}
		case *ai.UserMessage:
			if msg != nil {
				txt := cleanUserMessageDisplay(ai.ContentText(msg.Content, "\n"))
				if txt != "" {
					badge, rem := turnBadgeFor(pm, false)
					out = append(out, &ChatEntry{
						ID:            allocID(),
						Kind:          EntryUser,
						Text:          txt,
						TargetBadge:   badge,
						IsRemote:      rem,
						Done:          true,
						EntryID:       pm.EntryID,
						ParentEntryID: pm.ParentEntryID,
					})
				}
			}
		case ai.AssistantMessage:
			badge, rem := turnBadgeFor(pm, true)
			out = appendAssistantMsg(out, &msg, pm.EntryID, pm.ParentEntryID, badge, rem, toolMap, allocID)
		case *ai.AssistantMessage:
			if msg != nil {
				badge, rem := turnBadgeFor(pm, true)
				out = appendAssistantMsg(out, msg, pm.EntryID, pm.ParentEntryID, badge, rem, toolMap, allocID)
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

func cleanUserMessageDisplay(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[System Notice: Active execution environment switched") {
		if idx := strings.Index(s, "]\n\n"); idx != -1 {
			return strings.TrimSpace(s[idx+3:])
		}
	}
	return s
}

func appendAssistantMsg(out []*ChatEntry, am *ai.AssistantMessage, entryID, parentEntryID, toolBadge string, isRemote bool, toolMap map[string]*ChatEntry, allocID func() int) []*ChatEntry {
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
			ID:            allocID(),
			Kind:          EntryAssistant,
			Text:          strings.Join(textParts, "\n\n"),
			Thinking:      strings.Join(thinkParts, "\n"),
			Done:          true,
			ErrorText:     am.ErrorMessage,
			EntryID:       entryID,
			ParentEntryID: parentEntryID,
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
			TargetBadge: toolBadge,
			IsRemote:    isRemote,
			Done:        true,
			EntryID:     entryID,
			CardOpen:    tc.Name == "edit",
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
