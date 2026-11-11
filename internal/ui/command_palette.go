package ui

import (
	"runtime"
	"strings"

	"github.com/egoist/mygo/ui"
)

// PaletteCommand represents a single searchable action inside the Cmd+K Command Palette.
type PaletteCommand struct {
	ID       string
	Category string
	Title    string
	Subtitle string
	Shortcut string
	Icon     *ui.SVG
	Run      func(c *ui.Context)
}

// CommandPaletteModal renders a centered spotlight-style command palette (Cmd/Ctrl+K).
type CommandPaletteModal struct {
	Open        bool
	Query       string
	SelectedIdx int
	listState   ui.ListState
}

// OpenPalette resets search state and opens the Command Palette.
func (cp *CommandPaletteModal) OpenPalette() {
	cp.Open = true
	cp.Query = ""
	cp.SelectedIdx = 0
}

// Toggle flips the Command Palette open/closed state.
func (cp *CommandPaletteModal) Toggle() {
	if cp.Open {
		cp.Open = false
		return
	}
	cp.OpenPalette()
}

func platformModLabel() string {
	if runtime.GOOS == "darwin" {
		return "⌘"
	}
	return "Ctrl+"
}

// FilterCommands filters and ranks PaletteCommands against query.
func FilterCommands(all []PaletteCommand, query string) []PaletteCommand {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return all
	}
	var exact, partial []PaletteCommand
	for _, cmd := range all {
		tLower := strings.ToLower(cmd.Title)
		cLower := strings.ToLower(cmd.Category)
		sLower := strings.ToLower(cmd.Subtitle)
		switch {
		case strings.Contains(tLower, q):
			exact = append(exact, cmd)
		case strings.Contains(cLower, q) || strings.Contains(sLower, q):
			partial = append(partial, cmd)
		}
	}
	return append(exact, partial...)
}

// Render draws the spotlight command palette dialog when Open is true.
func (cp *CommandPaletteModal) Render(c *ui.Context, p Palette, commands []PaletteCommand) {
	if !cp.Open {
		return
	}
	filtered := FilterCommands(commands, cp.Query)
	if cp.SelectedIdx >= len(filtered) {
		cp.SelectedIdx = max(0, len(filtered)-1)
	}
	if cp.SelectedIdx < 0 {
		cp.SelectedIdx = 0
	}

	ui.DialogBase(c, &cp.Open, func(backdrop, panel ui.Element) {
		backdrop.Background(p.Backdrop)

		panel.Width(540).MaxHeight(420).
			Radius(RadiusLG).
			Background(p.ModalBg).
			Border(1, p.BorderStrong).
			Shadow(0, 20, 48, -10, p.ShadowModal).
			Clip().
			Children(func() {
				// Top Search Input Row
				ui.Row(c).FillWidth().Height(46).Padding(0, 14).Gap(10).
					AlignItems(ui.Center).
					BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).
					Children(func() {
						ui.Icon(c, IconSearch).Size(15, 15).TextColor(p.Accent).Shrink(0)
						prevQuery := cp.Query
						inp := ui.TextAreaBase(c, &cp.Query).
							Lines(1, 1).
							Grow(1).MinWidth(0).
							Padding(2, 0).
							FontSize(FontBody).
							TextColor(p.TextPrimary).
							Placeholder("输入命令、工作区名称或模型关键字...").
							PlaceholderColor(p.TextMuted).
							FocusRing(false).
							AutoFocus().
							OnTextKey(func(k ui.TextKey) bool {
								if k.Mods != 0 {
									return false
								}
								switch k.Key {
								case ui.KeyDown:
									if len(filtered) > 0 {
										cp.SelectedIdx = (cp.SelectedIdx + 1) % len(filtered)
									}
									return true
								case ui.KeyUp:
									if len(filtered) > 0 {
										cp.SelectedIdx = (cp.SelectedIdx - 1 + len(filtered)) % len(filtered)
									}
									return true
								case ui.KeyEscape:
									cp.Open = false
									return true
								case ui.KeyEnter:
									if len(filtered) > 0 && cp.SelectedIdx >= 0 && cp.SelectedIdx < len(filtered) {
										chosen := filtered[cp.SelectedIdx]
										cp.Open = false
										if chosen.Run != nil {
											chosen.Run(c)
										}
									}
									return true
								}
								return false
							})
						_ = inp
						if cp.Query != prevQuery {
							cp.SelectedIdx = 0
						}

						ui.Row(c).Padding(2, 6).Radius(RadiusSM).Background(p.BgElevated).Border(1, p.BorderSubtle).Shrink(0).Children(func() {
							ui.Text(c, "ESC").Font(FontMono).FontSize(10).TextColor(p.TextMuted)
						})
					})

				// Command List Body
				if len(filtered) == 0 {
					ui.Column(c).FillWidth().Padding(28, 16).AlignItems(ui.Center).Gap(6).Children(func() {
						ui.Text(c, "未找到匹配的快捷命令").FontSize(FontSmall).TextColor(p.TextMuted)
					})
					return
				}

				cp.listState.Key = func(i int) any {
					if i < len(filtered) {
						return filtered[i].ID
					}
					return i
				}
				ui.List(c, &cp.listState, len(filtered), func(i int) {
					item := filtered[i]
					selected := i == cp.SelectedIdx
					rowBtn := ui.ButtonBase(c).FillWidth().MinWidth(0).Height(38).Padding(0, 12).
						Radius(RadiusSM).Gap(10).AlignItems(ui.Center).Label(item.Title)
					bg := ui.Transparent
					if selected {
						bg = p.AccentSoft
					} else if rowBtn.Hovered() {
						bg = p.BgHover
					}
					icon := item.Icon
					if icon == nil {
						icon = IconCommand
					}
					iconCol := p.TextMuted
					titleCol := p.TextPrimary
					if selected {
						iconCol = p.Accent
					}
					rowBtn.Background(bg).Children(func() {
						ui.Icon(c, icon).Size(14, 14).TextColor(iconCol).Shrink(0)
						ui.Text(c, item.Category).FontSize(FontCaption).TextColor(p.TextMuted).Shrink(0).SingleLine()
						ui.Text(c, "·").FontSize(FontCaption).TextColor(p.TextMuted).Shrink(0)
						ui.Text(c, item.Title).FontSize(FontSmall).FontWeight(WeightMedium).TextColor(titleCol).
							Shrink(0).SingleLine()
						if item.Subtitle != "" {
							ui.Text(c, item.Subtitle).Font(FontMono).FontSize(FontCaption).TextColor(p.TextMuted).
								Grow(1).MinWidth(0).SingleLine().Ellipsis("...")
						} else {
							ui.Spacer(c)
						}
						if item.Shortcut != "" {
							ui.Row(c).Padding(1, 6).Radius(RadiusSM).Background(p.BgElevated).Border(1, p.BorderSubtle).Shrink(0).Children(func() {
								ui.Text(c, item.Shortcut).Font(FontMono).FontSize(10.5).TextColor(p.TextSecondary).SingleLine()
							})
						}
					})
					if rowBtn.Hovered() {
						cp.SelectedIdx = i
					}
					if rowBtn.Clicked() {
						cp.Open = false
						if item.Run != nil {
							item.Run(c)
						}
					}
				}).MaxHeight(340).Padding(6, 8).Gap(2)
			})
	})
}
