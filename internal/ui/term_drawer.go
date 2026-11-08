package ui

import (
	"strings"

	"github.com/egoist/mygo/ui"
)

// TermLine represents a line in the live bash output drawer.
type TermLine struct {
	ID   int
	Text string
	Kind string // "cmd", "meta", "err", "out"
}

// TermDrawer renders a pure-Go GPU-accelerated live terminal output stream
// without requiring external C libraries like libghostty-vt.
type TermDrawer struct {
	Lines     []TermLine
	NextID    int
	ListState ui.ListState
	Open      bool
	Width     float32
	partial   string
}

// NewTermDrawer creates a default live output drawer.
func NewTermDrawer() *TermDrawer {
	td := &TermDrawer{
		Open:  true,
		Width: 360,
	}
	td.AppendChunk("─── PiUI Live Bash Stream Ready ───\n")
	return td
}

// AppendChunk splits incoming raw stdout/stderr bytes into lines and classifies them for styling.
func (d *TermDrawer) AppendChunk(chunk string) {
	if chunk == "" {
		return
	}
	chunk = strings.ReplaceAll(chunk, "\r\n", "\n")
	chunk = strings.ReplaceAll(chunk, "\r", "\n")
	combined := d.partial + chunk
	parts := strings.Split(combined, "\n")
	d.partial = parts[len(parts)-1]

	for _, line := range parts[:len(parts)-1] {
		d.pushLine(line)
	}
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
	d.AppendChunk("─── Output Cleared ───\n")
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

// View renders the right-hand terminal output drawer.
func (d *TermDrawer) View(c *ui.Context) {
	if !d.Open {
		return
	}
	t := c.Theme()
	bg := ui.RGB(16, 18, 22)
	headerBg := ui.RGB(24, 26, 32)
	if !t.Dark {
		bg = ui.RGB(23, 25, 30)
		headerBg = ui.RGB(32, 35, 42)
	}

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

	ui.Column(c).Width(d.Width).FillHeight().Background(bg).BorderWidth(0, 0, 0, 1).BorderColor(t.Border).Children(func() {
		// Header bar
		ui.Row(c).FillWidth().Padding(8, 12).Gap(8).AlignItems(ui.Center).Background(headerBg).BorderWidth(0, 0, 1, 0).BorderColor(ui.RGB(45, 48, 58)).Children(func() {
			ui.Text(c, "🐚 实时命令输出流").Bold().FontSize(13).TextColor(ui.RGB(228, 228, 231))
			ui.Spacer(c)
			if ui.Button(c, "复制").Clicked() {
				c.WriteClipboard(d.AllText())
				c.Toast("已复制终端输出到剪贴板")
			}
			if ui.Button(c, "清空").Clicked() {
				d.Clear()
			}
		})

		// Scrollable log lines
		ui.List(c, &d.ListState, totalRows, func(i int) {
			var line TermLine
			if i < len(d.Lines) {
				line = d.Lines[i]
			} else {
				line = TermLine{ID: -1, Text: d.partial, Kind: "out"}
			}

			color := ui.RGB(212, 212, 216)
			switch line.Kind {
			case "cmd":
				color = ui.RGB(74, 222, 128) // green prompt
			case "meta":
				color = ui.RGB(148, 163, 184) // muted slate
			case "err":
				color = ui.RGB(248, 113, 113) // soft red
			}

			display := line.Text
			if display == "" {
				display = " "
			}
			el := ui.Text(c, display).
				Font("Menlo").
				FontSize(12).
				TextColor(color).
				Selectable()
			if line.Kind == "cmd" {
				el.Bold()
			}
		}).Grow(1).Padding(10, 12).Gap(3)
	})
}
