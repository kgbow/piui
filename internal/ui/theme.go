package ui

import "github.com/egoist/mygo/ui"

// Palette holds the complete Antigravity Dual-Theme (Obsidian Dark & Daylight Light)
// design tokens.
type Palette struct {
	IsLight bool

	BgSidebar    ui.Color
	BgCanvas     ui.Color
	BgSurface    ui.Color
	BgElevated   ui.Color
	BgHover      ui.Color
	BgActive     ui.Color
	BgTerm       ui.Color
	BgTermHeader ui.Color
	TermText     ui.Color
	CodeBg       ui.Color
	ModalBg      ui.Color
	InputBg      ui.Color

	BorderSubtle ui.Color
	BorderStrong ui.Color

	TextPrimary   ui.Color
	TextSecondary ui.Color
	TextMuted     ui.Color

	Accent      ui.Color
	AccentSoft  ui.Color
	SSHColor    ui.Color
	SSHSoft     ui.Color
	LocalColor  ui.Color
	LocalSoft   ui.Color
	WarnColor   ui.Color
	WarnSoft    ui.Color
	DangerColor ui.Color
	DangerSoft  ui.Color

	DiffDelBg ui.Color
	DiffDelFg ui.Color
	DiffAddBg ui.Color
	DiffAddFg ui.Color

	BtnPrimaryBg ui.Color
	BtnPrimaryFg ui.Color
}

// DarkPalette returns the Antigravity Obsidian (Dark) color tokens.
func DarkPalette() Palette {
	return Palette{
		IsLight: false,

		BgSidebar:    ui.Hex("#09090B"),
		BgCanvas:     ui.Hex("#0D0D10"),
		BgSurface:    ui.Hex("#131317"),
		BgElevated:   ui.Hex("#18181D"),
		BgHover:      ui.RGBA(255, 255, 255, 0.045),
		BgActive:     ui.RGBA(255, 255, 255, 0.075),
		BgTerm:       ui.Hex("#08090C"),
		BgTermHeader: ui.Hex("#0C0D12"),
		TermText:     ui.Hex("#D4D4D8"),
		CodeBg:       ui.Hex("#090A0D"),
		ModalBg:      ui.Hex("#111116"),
		InputBg:      ui.Hex("#0B0B0E"),

		BorderSubtle: ui.RGBA(255, 255, 255, 0.07),
		BorderStrong: ui.RGBA(255, 255, 255, 0.13),

		TextPrimary:   ui.Hex("#F4F4F6"),
		TextSecondary: ui.Hex("#A1A1AA"),
		TextMuted:     ui.Hex("#6E6E77"),

		Accent:      ui.Hex("#38BDF8"),
		AccentSoft:  ui.RGBA(56, 189, 248, 0.12),
		SSHColor:    ui.Hex("#A78BFA"),
		SSHSoft:     ui.RGBA(167, 139, 250, 0.14),
		LocalColor:  ui.Hex("#34D399"),
		LocalSoft:   ui.RGBA(52, 211, 153, 0.13),
		WarnColor:   ui.Hex("#FBBF24"),
		WarnSoft:    ui.RGBA(251, 191, 36, 0.14),
		DangerColor: ui.Hex("#F87171"),
		DangerSoft:  ui.RGBA(239, 68, 68, 0.13),

		DiffDelBg: ui.RGBA(239, 68, 68, 0.12),
		DiffDelFg: ui.Hex("#F87171"),
		DiffAddBg: ui.RGBA(34, 197, 94, 0.12),
		DiffAddFg: ui.Hex("#4ADE80"),

		BtnPrimaryBg: ui.Hex("#F4F4F6"),
		BtnPrimaryFg: ui.Hex("#09090B"),
	}
}

// LightPalette returns the Antigravity Daylight (Light) color tokens.
func LightPalette() Palette {
	return Palette{
		IsLight: true,

		BgSidebar:    ui.Hex("#F3F4F6"),
		BgCanvas:     ui.Hex("#FAFAFC"),
		BgSurface:    ui.Hex("#FFFFFF"),
		BgElevated:   ui.Hex("#F1F2F6"),
		BgHover:      ui.RGBA(15, 23, 42, 0.045),
		BgActive:     ui.RGBA(15, 23, 42, 0.075),
		BgTerm:       ui.Hex("#F3F4F6"),
		BgTermHeader: ui.Hex("#F8FAFC"),
		TermText:     ui.Hex("#1F2937"),
		CodeBg:       ui.Hex("#F3F4F6"),
		ModalBg:      ui.Hex("#FFFFFF"),
		InputBg:      ui.Hex("#F8FAFC"),

		BorderSubtle: ui.RGBA(15, 23, 42, 0.08),
		BorderStrong: ui.RGBA(15, 23, 42, 0.15),

		TextPrimary:   ui.Hex("#111827"),
		TextSecondary: ui.Hex("#4B5563"),
		TextMuted:     ui.Hex("#6B7280"),

		Accent:      ui.Hex("#0284C7"),
		AccentSoft:  ui.RGBA(2, 132, 199, 0.10),
		SSHColor:    ui.Hex("#6D28D9"),
		SSHSoft:     ui.RGBA(109, 40, 217, 0.10),
		LocalColor:  ui.Hex("#059669"),
		LocalSoft:   ui.RGBA(5, 150, 105, 0.10),
		WarnColor:   ui.Hex("#D97706"),
		WarnSoft:    ui.RGBA(217, 119, 6, 0.11),
		DangerColor: ui.Hex("#DC2626"),
		DangerSoft:  ui.RGBA(239, 68, 68, 0.10),

		DiffDelBg: ui.RGBA(239, 68, 68, 0.09),
		DiffDelFg: ui.Hex("#DC2626"),
		DiffAddBg: ui.RGBA(22, 163, 74, 0.09),
		DiffAddFg: ui.Hex("#15803D"),

		BtnPrimaryBg: ui.Hex("#111827"),
		BtnPrimaryFg: ui.Hex("#FFFFFF"),
	}
}

// PaletteFor returns LightPalette when isLight is true, otherwise DarkPalette.
func PaletteFor(isLight bool) Palette {
	if isLight {
		return LightPalette()
	}
	return DarkPalette()
}

// BuildMyGoTheme builds a *ui.Theme synchronized with our Antigravity Palette.
func BuildMyGoTheme(isLight bool) *ui.Theme {
	p := PaletteFor(isLight)
	var base *ui.Theme
	if isLight {
		base = ui.LightTheme()
	} else {
		base = ui.DarkTheme()
	}
	t := *base
	t.Dark = !isLight
	t.Background = p.BgCanvas
	t.Surface = p.BgSurface
	t.SurfaceHover = p.BgHover
	t.SurfacePressed = p.BgActive
	t.Border = p.BorderSubtle
	t.Text = p.TextPrimary
	t.TextMuted = p.TextMuted
	t.Accent = p.Accent
	t.AccentText = ui.Hex("#FFFFFF")
	t.Danger = p.DangerColor
	t.Warning = p.WarnColor
	t.Success = p.LocalColor
	t.Radius = 8
	return &t
}

// IconBtn renders a compact square icon button matching Antigravity's .icon-btn.
func IconBtn(c *ui.Context, p Palette, icon *ui.SVG, boxSize, iconSize float32, onClick func()) ui.Element {
	b := ui.ButtonBase(c).Size(boxSize, boxSize).Radius(6).
		AlignItems(ui.Center).Justify(ui.Center)
	fg := p.TextMuted
	bg := ui.Transparent
	if b.Hovered() || b.Pressed() {
		fg = p.TextPrimary
		bg = p.BgHover
	}
	b.Background(bg).Children(func() {
		ui.Icon(c, icon).Size(iconSize, iconSize).TextColor(fg)
	})
	if b.Clicked() && onClick != nil {
		onClick()
	}
	return b
}

// IconTipBtn renders a quiet square icon button with a hover tooltip and accessibility label.
func IconTipBtn(c *ui.Context, p Palette, icon *ui.SVG, boxSize, iconSize float32, tooltip string, onClick func()) ui.Element {
	b := IconBtn(c, p, icon, boxSize, iconSize, onClick)
	if tooltip != "" {
		b.Tooltip(tooltip).Label(tooltip)
	}
	return b
}

// DangerIconBtn renders a quiet square icon button that highlights with danger styling on hover.
func DangerIconBtn(c *ui.Context, p Palette, icon *ui.SVG, boxSize, iconSize float32, tooltip string, onClick func()) ui.Element {
	b := ui.ButtonBase(c).Size(boxSize, boxSize).Radius(6).
		AlignItems(ui.Center).Justify(ui.Center)
	fg := p.TextMuted
	bg := ui.Transparent
	if b.Hovered() || b.Pressed() {
		fg = p.DangerColor
		bg = p.DangerSoft
	}
	b.Background(bg).Children(func() {
		ui.Icon(c, icon).Size(iconSize, iconSize).TextColor(fg)
	})
	if tooltip != "" {
		b.Tooltip(tooltip).Label(tooltip)
	}
	if b.Clicked() && onClick != nil {
		onClick()
	}
	return b
}

// ToolbarIconBtn renders a compact bordered toolbar icon button supporting active state and custom icon tint.
func ToolbarIconBtn(c *ui.Context, p Palette, icon *ui.SVG, boxSize, iconSize float32, active bool, tint ui.Color, tooltip string, onClick func()) ui.Element {
	b := ui.ButtonBase(c).Size(boxSize, boxSize).Radius(7).
		AlignItems(ui.Center).Justify(ui.Center)
	bg := ui.Transparent
	borderCol := p.BorderSubtle
	fg := p.TextSecondary
	if tint.A > 0 {
		fg = tint
	}
	if active {
		bg = p.BgActive
		borderCol = p.BorderStrong
		if tint.A == 0 {
			fg = p.TextPrimary
		}
	}
	if b.Hovered() || b.Pressed() {
		if !active {
			bg = p.BgHover
		}
		borderCol = p.BorderStrong
		if tint.A == 0 {
			fg = p.TextPrimary
		}
	}
	b.Background(bg).Border(1, borderCol).Children(func() {
		ui.Icon(c, icon).Size(iconSize, iconSize).TextColor(fg)
	})
	if tooltip != "" {
		b.Tooltip(tooltip).Label(tooltip)
	}
	if b.Clicked() && onClick != nil {
		onClick()
	}
	return b
}

// MetaActionBtn renders a quiet inline action link.
func MetaActionBtn(c *ui.Context, p Palette, label string, onClick func()) ui.Element {
	b := ui.ButtonBase(c).Padding(1, 3).Radius(4)
	fg := p.TextMuted
	if b.Hovered() || b.Pressed() {
		fg = p.TextPrimary
	}
	b.Children(func() {
		ui.Text(c, label).FontSize(11).TextColor(fg)
	})
	if b.Clicked() && onClick != nil {
		onClick()
	}
	return b
}
