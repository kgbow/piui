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
	case strings.HasPrefix(trimmed, "[Safety Gate]"):
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

// View renders the right-hand terminal output drawer matching the Doop Antigravity specification.
func (d *TermDrawer) View(c *ui.Context, p Palette) {
	if !d.Open {
		return
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

	termActionBtn := func(label string, onClick func()) {
		b := ui.ButtonBase(c).Padding(3, 8).Radius(6).
			Border(1, ui.RGBA(255, 255, 255, 0.10)).AlignItems(ui.Center)
		bg := ui.RGBA(255, 255, 255, 0.06)
		fg := ui.Hex("#A1A1AA")
		if b.Hovered() || b.Pressed() {
			bg = ui.RGBA(255, 255, 255, 0.12)
			fg = ui.Hex("#FFFFFF")
		}
		b.Background(bg).Children(func() {
			ui.Text(c, label).FontSize(11).TextColor(fg)
		})
		if b.Clicked() && onClick != nil {
			onClick()
		}
	}

	ui.Column(c).Width(d.Width).FillHeight().Background(p.BgTerm).
		BorderWidth(0, 0, 0, 1).BorderColor(p.BorderSubtle).Children(func() {
		// 44px Header bar
		ui.Row(c).FillWidth().Height(44).Padding(0, 14).Gap(7).
			AlignItems(ui.Center).Background(p.BgTermHeader).
			BorderWidth(0, 0, 1, 0).BorderColor(ui.RGBA(255, 255, 255, 0.07)).Children(func() {
			ui.Icon(c, IconTerminal).Size(14, 14).TextColor(ui.Hex("#4ADE80"))
			ui.Text(c, "实时命令输出流").Bold().FontSize(12).TextColor(ui.Hex("#E4E4E7"))
			ui.Spacer(c)
			termActionBtn("复制", func() {
				c.WriteClipboard(d.AllText())
				c.Toast("已复制终端输出到剪贴板")
			})
			termActionBtn("清空", func() {
				d.Clear()
			})
		})

		// Scrollable log lines
		ui.List(c, &d.ListState, totalRows, func(i int) {
			var line TermLine
			if i < len(d.Lines) {
				line = d.Lines[i]
			} else {
				line = TermLine{ID: -1, Text: d.partial, Kind: "out"}
			}

			color := ui.Hex("#D4D4D8")
			switch line.Kind {
			case "cmd":
				color = ui.Hex("#4ADE80") // green prompt
			case "meta":
				color = ui.Hex("#94A3B8") // muted slate
			case "warn":
				color = ui.Hex("#FBBF24") // amber safety gate notice
			case "err":
				color = ui.Hex("#F87171") // soft red
			}

			display := line.Text
			if display == "" {
				display = " "
			}
			el := ui.Text(c, display).
				Font("Menlo").
				FontSize(11.5).
				TextColor(color).
				Selectable()
			if line.Kind == "cmd" {
				el.Bold()
			}
		}).Grow(1).Padding(12, 14).Gap(3)
	})
}
