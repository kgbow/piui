package ui

import (
	"regexp"
	"strings"

	"github.com/egoist/mygo/ui"
)

var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// TermLine represents a line in the live bash output drawer.
type TermLine struct {
	ID   int
	Text string
	Kind string // "cmd", "meta", "warn", "err", "out"
}

// TermDrawer renders a pure-Go GPU-accelerated live terminal output stream
// without requiring external C libraries like libghostty-vt.
type TermDrawer struct {
	Lines     []TermLine
	NextID    int
	ListState ui.ListState
	Open      bool
	Unread    bool
	Width     float32
	partial   string
}

// NewTermDrawer creates a default live output drawer (collapsed initially to keep the main stage calm).
func NewTermDrawer() *TermDrawer {
	return &TermDrawer{
		Open:  false,
		Width: DefaultTermWidth,
	}
}

// AppendChunk splits incoming raw stdout/stderr bytes into lines and classifies them for styling.
func (d *TermDrawer) AppendChunk(chunk string) {
	if chunk == "" {
		return
	}
	if !d.Open {
		d.Unread = true
	}
	chunk = strings.ReplaceAll(chunk, "\r\n", "\n")
	chunk = strings.ReplaceAll(chunk, "\r", "\n")
	combined := d.partial + chunk
	parts := strings.Split(combined, "\n")
	d.partial = stripANSI(parts[len(parts)-1])

	for _, line := range parts[:len(parts)-1] {
		d.pushLine(stripANSI(line))
	}
}

func stripANSI(s string) string {
	if !strings.ContainsRune(s, '\x1b') {
		return s
	}
	return ansiEscapeRe.ReplaceAllString(s, "")
}

func (d *TermDrawer) pushLine(line string) {
	d.NextID++
	kind := "out"
	trimmed := strings.TrimSpace(line)
	switch {
	case strings.Contains(line, " $ "):
		kind = "cmd"
	case strings.HasPrefix(trimmed, "───") || strings.HasPrefix(trimmed, "[exit code:"):
		kind = "meta"
	case strings.HasPrefix(trimmed, "[Safety Gate]") || strings.Contains(trimmed, "[安全拦截]"):
		kind = "warn"
	case strings.HasPrefix(trimmed, "[error:"):
		kind = "err"
	}
	d.Lines = append(d.Lines, TermLine{
		ID:   d.NextID,
		Text: line,
		Kind: kind,
	})
	// Cap scrollback at 2000 lines to keep GPU layout ultra-fast.
	if len(d.Lines) > 2000 {
		d.Lines = d.Lines[len(d.Lines)-2000:]
	}
}

// Clear empties the terminal output buffer.
func (d *TermDrawer) Clear() {
	d.Lines = nil
	d.partial = ""
	d.Unread = false
}

// AllText returns the complete text buffer for clipboard copying.
func (d *TermDrawer) AllText() string {
	var sb strings.Builder
	for _, l := range d.Lines {
		sb.WriteString(l.Text)
		sb.WriteByte('\n')
	}
	if d.partial != "" {
		sb.WriteString(d.partial)
	}
	return sb.String()
}

// View renders the right-hand terminal output drawer inside its split pane.
func (d *TermDrawer) View(c *ui.Context, p Palette) {
	if !d.Open {
		return
	}
	d.Unread = false

	d.ListState.Key = func(i int) any {
		if i < len(d.Lines) {
			return d.Lines[i].ID
		}
		return -1
	}
	d.ListState.FollowEnd = true
	d.ListState.FollowAnimation = &ui.Smooth

	totalRows := len(d.Lines)
	if d.partial != "" {
		totalRows++
	}

	bar := c.TitleBar()
	rightPad := max(bar.Right, 10)

	ui.Column(c).Direction(ui.LeftToRight).Fill().Clip().Background(p.BgTerm).Children(func() {
		// Unified 44px Top Header Bar aligned with CenterMain and LeftSidebar
		ui.Row(c).FillWidth().MinWidth(0).Height(TopBarHeight).Padding(0, rightPad, 0, 14).Gap(6).
			AlignItems(ui.Center).Background(p.BgTermHeader).
			BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).DragWindow().Children(func() {
			ui.Icon(c, IconTerminal).Size(14, 14).TextColor(p.TextSecondary)
			ui.Text(c, "实时输出").FontWeight(WeightMedium).FontSize(FontSmall).TextColor(p.TextPrimary).SingleLine()
			if totalRows > 0 {
				ui.Textf(c, "%d", totalRows).FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextMuted).Padding(0, 2)
			}
			ui.Spacer(c)
			IconTipBtn(c, p, IconCopy, 26, 13.5, "复制全部终端输出", func() {
				c.WriteClipboard(d.AllText())
				c.Toast("已复制终端输出到剪贴板")
			})
			DangerIconBtn(c, p, IconTrash, 26, 13.5, "清空终端输出", func() {
				d.Clear()
			})
			IconTipBtn(c, p, IconCross, 26, 14, "收起终端面板 (Ctrl/Cmd+J)", func() {
				d.Open = false
			})
		})

		// Scrollable log lines or quiet empty state
		ui.List(c, &d.ListState, totalRows, func(i int) {
			var line TermLine
			if i < len(d.Lines) {
				line = d.Lines[i]
			} else {
				line = TermLine{ID: -1, Text: d.partial, Kind: "out"}
			}

			color := p.TermText
			switch line.Kind {
			case "cmd":
				color = p.LocalColor
			case "meta":
				color = p.TextMuted
			case "warn":
				color = p.WarnColor
			case "err":
				color = p.DangerColor
			}

			display := line.Text
			if display == "" {
				display = " "
			}
			el := ui.Text(c, display).
				Font(FontMono).
				FontSize(FontCaption + 0.5).
				LineHeight(1.45).
				TextColor(color).
				FillWidth().
				MinWidth(0).
				Selectable()
			if line.Kind == "cmd" {
				el.FontWeight(WeightMedium)
			}
		}).Grow(1).MinWidth(0).Padding(12, 14).Gap(3).Children(func() {
			if totalRows == 0 {
				ui.Column(c).Margin(ui.Auto).AlignItems(ui.Center).Gap(6).Padding(24).Children(func() {
					ui.Icon(c, IconTerminal).Size(20, 20).TextColor(p.TextMuted)
					ui.Text(c, "暂无实时命令输出").FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextSecondary)
					ui.Text(c, "当 Agent 执行 bash 命令时，标准输出与错误流将在此实时滚动显示。").
						FontSize(FontCaption).LineHeight(1.5).TextColor(p.TextMuted).TextAlign(ui.Center)
				})
			}
		})
	})
}
