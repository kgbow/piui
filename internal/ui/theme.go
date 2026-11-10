package ui

import (
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
)

// Unified Design System Tokens — Typography, Weights, Radii, and Layout Metrics.
const (
	// FontMono is the cross-platform monospace font stack for paths, IDs, code, and terminal output.
	FontMono = "SF Mono, JetBrains Mono, Menlo, Consolas, monospace"

	// Strict 5-step Typography Scale (DIPs)
	FontCaption float32 = 11.0
	FontSmall   float32 = 12.0
	FontBody    float32 = 13.5
	FontTitle   float32 = 15.0
	FontHero    float32 = 20.0

	// Font Weights
	WeightRegular  = 400
	WeightMedium   = 500
	WeightSemibold = 600

	// Strict 4-step Corner Radius Scale (DIPs)
	RadiusSM   float32 = 6.0
	RadiusMD   float32 = 8.0
	RadiusLG   float32 = 12.0
	RadiusPill float32 = 999.0

	// Unified Chrome & Pane Metrics
	TopBarHeight        float32 = 44.0
	DefaultSidebarWidth float32 = 256.0
	DefaultTermWidth    float32 = 360.0
)

// Palette holds the complete Antigravity Dual-Theme (Obsidian Dark & Daylight Light)
// color and elevation tokens.
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
	BorderFocus  ui.Color

	Backdrop    ui.Color
	ShadowSoft  ui.Color
	ShadowModal ui.Color

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
	SuccessBg    ui.Color
	SuccessFg    ui.Color
}

// DarkPalette returns the Antigravity Obsidian (Dark) color tokens.
func DarkPalette() Palette {
	return Palette{
		IsLight: false,

		BgSidebar:    ui.Hex("#0A0A0D"),
		BgCanvas:     ui.Hex("#0E0E12"),
		BgSurface:    ui.Hex("#141419"),
		BgElevated:   ui.Hex("#1C1C23"),
		BgHover:      ui.RGBA(255, 255, 255, 0.048),
		BgActive:     ui.RGBA(255, 255, 255, 0.082),
		BgTerm:       ui.Hex("#090A0E"),
		BgTermHeader: ui.Hex("#0E0F14"),
		TermText:     ui.Hex("#D4D4D8"),
		CodeBg:       ui.Hex("#0A0B0F"),
		ModalBg:      ui.Hex("#15151B"),
		InputBg:      ui.Hex("#101015"),

		BorderSubtle: ui.RGBA(255, 255, 255, 0.068),
		BorderStrong: ui.RGBA(255, 255, 255, 0.135),
		BorderFocus:  ui.RGBA(56, 189, 248, 0.36),

		Backdrop:    ui.RGBA(0, 0, 0, 0.62),
		ShadowSoft:  ui.RGBA(0, 0, 0, 0.36),
		ShadowModal: ui.RGBA(0, 0, 0, 0.62),

		TextPrimary:   ui.Hex("#F4F4F6"),
		TextSecondary: ui.Hex("#A1A1AA"),
		TextMuted:     ui.Hex("#6E6E78"),

		Accent:      ui.Hex("#38BDF8"),
		AccentSoft:  ui.RGBA(56, 189, 248, 0.12),
		SSHColor:    ui.Hex("#A78BFA"),
		SSHSoft:     ui.RGBA(167, 139, 250, 0.13),
		LocalColor:  ui.Hex("#34D399"),
		LocalSoft:   ui.RGBA(52, 211, 153, 0.12),
		WarnColor:   ui.Hex("#FBBF24"),
		WarnSoft:    ui.RGBA(251, 191, 36, 0.13),
		DangerColor: ui.Hex("#F87171"),
		DangerSoft:  ui.RGBA(239, 68, 68, 0.12),

		DiffDelBg: ui.RGBA(239, 68, 68, 0.12),
		DiffDelFg: ui.Hex("#FCA5A5"),
		DiffAddBg: ui.RGBA(34, 197, 94, 0.12),
		DiffAddFg: ui.Hex("#86EFAC"),

		BtnPrimaryBg: ui.Hex("#F4F4F6"),
		BtnPrimaryFg: ui.Hex("#09090B"),
		SuccessBg:    ui.Hex("#10B981"),
		SuccessFg:    ui.Hex("#042F1E"),
	}
}

// LightPalette returns the Antigravity Daylight (Light) color tokens.
func LightPalette() Palette {
	return Palette{
		IsLight: true,

		BgSidebar:    ui.Hex("#F3F4F7"),
		BgCanvas:     ui.Hex("#FAFAFC"),
		BgSurface:    ui.Hex("#FFFFFF"),
		BgElevated:   ui.Hex("#ECEEF3"),
		BgHover:      ui.RGBA(15, 23, 42, 0.045),
		BgActive:     ui.RGBA(15, 23, 42, 0.075),
		BgTerm:       ui.Hex("#F4F5F8"),
		BgTermHeader: ui.Hex("#FAFAFC"),
		TermText:     ui.Hex("#1F2937"),
		CodeBg:       ui.Hex("#F4F5F8"),
		ModalBg:      ui.Hex("#FFFFFF"),
		InputBg:      ui.Hex("#F5F6F9"),

		BorderSubtle: ui.RGBA(15, 23, 42, 0.075),
		BorderStrong: ui.RGBA(15, 23, 42, 0.14),
		BorderFocus:  ui.RGBA(2, 132, 199, 0.38),

		Backdrop:    ui.RGBA(15, 23, 42, 0.30),
		ShadowSoft:  ui.RGBA(15, 23, 42, 0.06),
		ShadowModal: ui.RGBA(15, 23, 42, 0.16),

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
		WarnSoft:    ui.RGBA(217, 119, 6, 0.10),
		DangerColor: ui.Hex("#DC2626"),
		DangerSoft:  ui.RGBA(239, 68, 68, 0.09),

		DiffDelBg: ui.RGBA(239, 68, 68, 0.09),
		DiffDelFg: ui.Hex("#B91C1C"),
		DiffAddBg: ui.RGBA(22, 163, 74, 0.09),
		DiffAddFg: ui.Hex("#15803D"),

		BtnPrimaryBg: ui.Hex("#111827"),
		BtnPrimaryFg: ui.Hex("#FFFFFF"),
		SuccessBg:    ui.Hex("#059669"),
		SuccessFg:    ui.Hex("#FFFFFF"),
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
	t.Surface = p.InputBg
	t.SurfaceHover = p.BgHover
	t.SurfacePressed = p.BgActive
	t.Border = p.BorderSubtle
	t.Text = p.TextPrimary
	t.TextMuted = p.TextMuted
	t.Accent = p.Accent
	t.AccentText = ui.Hex("#FFFFFF")
	t.Focus = p.BorderFocus
	t.FocusRingWidth = 1.5
	t.FocusRingOffset = 0
	t.Danger = p.DangerColor
	t.Warning = p.WarnColor
	t.Success = p.LocalColor
	t.Radius = RadiusMD
	t.FontSize = FontSmall
	return &t
}

// BuildMarkdownTheme creates a refined Markdown typography and code block theme synced to Palette.
func BuildMarkdownTheme(p Palette) *markdown.Theme {
	return &markdown.Theme{
		FontSize:            FontBody,
		LineHeight:          1.62,
		CodeFont:            FontMono,
		CodeSize:            0.88,
		CodeLineHeight:      1.5,
		Text:                p.TextPrimary,
		Muted:               p.TextMuted,
		Link:                p.Accent,
		Strong:              p.TextPrimary,
		StrongWeight:        WeightSemibold,
		Code:                p.TextPrimary,
		CodeBackground:      p.BgElevated,
		BlockSpacing:        0.72,
		ItemSpacing:         0.28,
		QuoteBar:            p.BorderStrong,
		QuoteText:           p.TextSecondary,
		Rule:                p.BorderSubtle,
		CodeBlockBackground: p.CodeBg,
		CodeBlockBorder:     p.BorderSubtle,
		CodeBlockRadius:     RadiusMD,
		CodeBlockPadding:    0.9,
		TableBorder:         p.BorderSubtle,
		TableHeader:         p.BgSurface,
	}
}

// RenderMarkdownCodeBlock renders a refined fenced code block with a clean header bar and copy button.
func RenderMarkdownCodeBlock(c *ui.Context, p Palette, b *markdown.CodeBlockView) {
	lang := strings.TrimSpace(b.Language)
	if lang == "" {
		lang = "code"
	}
	ui.Column(c).Direction(ui.LeftToRight).AlignSelf(ui.Stretch).
		Radius(RadiusMD).Background(p.CodeBg).Border(1, p.BorderSubtle).Clip().
		Children(func() {
			ui.Row(c).FillWidth().Height(30).Padding(0, 8, 0, 12).
				AlignItems(ui.Center).Background(p.BgSurface).
				BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).Children(func() {
				ui.Text(c, lang).Font(FontMono).FontSize(FontCaption).TextColor(p.TextMuted).Unselectable()
				ui.Spacer(c)
				b.CopyButton(c)
			})
			b.Body(c)
		})
}

// IconBtn renders a quiet square icon button with smooth hover feedback.
func IconBtn(c *ui.Context, p Palette, icon *ui.SVG, boxSize, iconSize float32, onClick func()) ui.Element {
	b := ui.ButtonBase(c).Size(boxSize, boxSize).Radius(RadiusSM).
		AlignItems(ui.Center).Justify(ui.Center)
	fg := p.TextMuted
	targetBg := ui.Transparent
	if b.Pressed() {
		fg = p.TextPrimary
		targetBg = p.BgActive
	} else if b.Hovered() {
		fg = p.TextPrimary
		targetBg = p.BgHover
	}
	b.Background(targetBg).Children(func() {
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
	b := ui.ButtonBase(c).Size(boxSize, boxSize).Radius(RadiusSM).
		AlignItems(ui.Center).Justify(ui.Center)
	fg := p.TextMuted
	targetBg := ui.Transparent
	if b.Hovered() || b.Pressed() {
		fg = p.DangerColor
		targetBg = p.DangerSoft
	}
	b.Background(targetBg).Children(func() {
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

// ToolbarIconBtn renders a compact toolbar icon button supporting active state, subtle border, and custom tint.
func ToolbarIconBtn(c *ui.Context, p Palette, icon *ui.SVG, boxSize, iconSize float32, active bool, tint ui.Color, tooltip string, onClick func()) ui.Element {
	b := ui.ButtonBase(c).Size(boxSize, boxSize).Radius(RadiusSM).
		AlignItems(ui.Center).Justify(ui.Center)
	targetBg := ui.Transparent
	borderCol := ui.Transparent
	fg := p.TextSecondary
	if tint.A > 0 {
		fg = tint
	}
	if active {
		targetBg = p.BgActive
		borderCol = p.BorderSubtle
		if tint.A == 0 {
			fg = p.TextPrimary
		}
	}
	if b.Hovered() || b.Pressed() {
		if !active {
			targetBg = p.BgHover
		}
		borderCol = p.BorderSubtle
		if tint.A == 0 {
			fg = p.TextPrimary
		}
	}
	b.Background(targetBg)
	if borderCol.A > 0 {
		b.Border(1, borderCol)
	}
	b.Children(func() {
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
	b := ui.ButtonBase(c).Padding(2, 5).Radius(RadiusSM)
	fg := p.TextMuted
	if b.Hovered() || b.Pressed() {
		fg = p.TextPrimary
	}
	b.Children(func() {
		ui.Text(c, label).FontSize(FontCaption).TextColor(fg)
	})
	if b.Clicked() && onClick != nil {
		onClick()
	}
	return b
}

// SegmentedBar renders a unified, borderless-pill segmented tab switcher with subtle active elevation.
func SegmentedBar(c *ui.Context, p Palette, activeIdx *int, labels []string) {
	ui.Row(c).FillWidth().Padding(3).Radius(RadiusMD).
		Background(p.InputBg).Border(1, p.BorderSubtle).Gap(3).Children(func() {
		for i, lbl := range labels {
			idx := i
			text := lbl
			active := *activeIdx == idx
			b := ui.ButtonBase(c).Grow(1).Height(28).Padding(0, 10).Radius(RadiusSM).
				AlignItems(ui.Center).Justify(ui.Center)
			targetBg := ui.Transparent
			fg := p.TextMuted
			if active {
				targetBg = p.BgElevated
				if p.IsLight {
					targetBg = p.BgSurface
				}
				fg = p.TextPrimary
			} else if b.Hovered() {
				fg = p.TextSecondary
			}
			b.Background(targetBg)
			if active {
				b.Border(1, p.BorderSubtle).Shadow(0, 1, 3, 0, p.ShadowSoft)
			}
			b.Children(func() {
				el := ui.Text(c, text).FontSize(FontSmall).TextColor(fg).SingleLine()
				if active {
					el.FontWeight(WeightMedium)
				}
			})
			if b.Clicked() {
				*activeIdx = idx
			}
		}
	})
}

// AppCheckbox renders a unified, crisp vector checkbox across all panels and modals.
func AppCheckbox(c *ui.Context, p Palette, checked *bool, label string, mono bool) ui.Element {
	b := ui.ButtonBase(c).Gap(8).AlignItems(ui.Center).Role(ui.RoleCheckBox).Checked(*checked)
	if label != "" {
		b.Label(label)
	}
	isOn := *checked
	boxBg := ui.Transparent
	boxBorder := p.BorderStrong
	if isOn {
		boxBg = p.Accent
		boxBorder = p.Accent
	} else if b.Hovered() {
		boxBg = p.BgHover
	}
	b.Children(func() {
		ui.Row(c).Size(15, 15).Shrink(0).Radius(4).Background(boxBg).Border(1, boxBorder).
			AlignItems(ui.Center).Justify(ui.Center).Children(func() {
			if isOn {
				ui.Icon(c, IconCheck).Size(10.5, 10.5).TextColor(ui.Hex("#FFFFFF"))
			}
		})
		if label != "" {
			el := ui.Text(c, label).FontSize(FontSmall).TextColor(p.TextPrimary).SingleLine()
			if mono {
				el.Font(FontMono)
			}
		}
	})
	if b.Clicked() {
		*checked = !*checked
	}
	return b
}

// AppDialog renders a refined, bordered, elevated modal surface with edge-to-edge layout support.
func AppDialog(c *ui.Context, p Palette, open *bool, width, fixedHeight float32, fn func()) ui.Element {
	if !*open {
		return ui.Element{}
	}
	winW, winH := c.Size()
	return ui.DialogBase(c, open, func(backdrop, panel ui.Element) {
		backdrop.Background(p.Backdrop)
		panel.Width(width).
			MaxWidth(max(winW-40, 320)).
			MaxHeight(max(winH-40, 280)).
			Radius(RadiusLG).
			Background(p.ModalBg).
			Border(1, p.BorderStrong).
			Shadow(0, 20, 50, -8, p.ShadowModal).
			Clip()
		if fixedHeight > 0 {
			panel.Height(fixedHeight)
		}
		fn()
	})
}

// GhostBtn renders a secondary bordered button with unified metrics.
func GhostBtn(c *ui.Context, p Palette, icon *ui.SVG, label, tooltip string, onClick func()) ui.Element {
	b := ui.ButtonBase(c).Height(30).Padding(0, 12).Radius(RadiusSM).
		Border(1, p.BorderStrong).Gap(5).AlignItems(ui.Center).Justify(ui.Center)
	targetBg := ui.Transparent
	fg := p.TextSecondary
	if b.Hovered() || b.Pressed() {
		targetBg = p.BgHover
		fg = p.TextPrimary
	}
	b.Background(targetBg).Children(func() {
		if icon != nil {
			ui.Icon(c, icon).Size(13.5, 13.5).TextColor(fg)
		}
		ui.Text(c, label).FontSize(FontSmall).FontWeight(WeightMedium).TextColor(fg).SingleLine()
	})
	if tooltip != "" {
		b.Tooltip(tooltip).Label(tooltip)
	}
	if b.Clicked() && onClick != nil {
		onClick()
	}
	return b
}

// PrimaryBtn renders a high-contrast primary action button with unified metrics.
func PrimaryBtn(c *ui.Context, p Palette, icon *ui.SVG, label, tooltip string, onClick func()) ui.Element {
	b := ui.ButtonBase(c).Height(30).Padding(0, 14).Radius(RadiusSM).
		Gap(5).AlignItems(ui.Center).Justify(ui.Center)
	bg := p.BtnPrimaryBg
	if b.Hovered() || b.Pressed() {
		bg = bg.Alpha(0.88)
	}
	b.Background(bg).Children(func() {
		if icon != nil {
			ui.Icon(c, icon).Size(13.5, 13.5).TextColor(p.BtnPrimaryFg)
		}
		ui.Text(c, label).FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.BtnPrimaryFg).SingleLine()
	})
	if tooltip != "" {
		b.Tooltip(tooltip).Label(tooltip)
	}
	if b.Clicked() && onClick != nil {
		onClick()
	}
	return b
}
