package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
	"github.com/sky-valley/pi/coding"

	"piui/internal/harness"
)

// PendingAttachment wraps a harness.PromptAttachment with an optional decoded UI bitmap thumbnail.
type PendingAttachment struct {
	Attachment harness.PromptAttachment
	Bitmap     *ui.Bitmap
}

// AddAttachmentFromPath loads an image or text/code file from disk and appends it to the composer.
func (a *DeskApp) AddAttachmentFromPath(c *ui.Context, rawPath string) error {
	att, err := harness.LoadAttachmentFromPath(rawPath)
	if err != nil {
		if c != nil {
			c.Toast(err.Error())
		}
		return err
	}
	a.nextAttachID++
	att.ID = fmt.Sprintf("att-%d", a.nextAttachID)

	var bmp *ui.Bitmap
	if att.Kind == harness.AttachmentImage && len(att.PreviewBytes) > 0 {
		bmp, _ = ui.DecodeBitmap(att.PreviewBytes)
	}
	a.pendingAttachments = append(a.pendingAttachments, PendingAttachment{
		Attachment: att,
		Bitmap:     bmp,
	})
	if c != nil {
		c.Toast("已添加附件: " + att.Name)
	}
	return nil
}

// AddImageAttachmentBytes attaches raw image bytes (e.g. from clipboard screenshot paste or PNG drop).
func (a *DeskApp) AddImageAttachmentBytes(c *ui.Context, name, mimeType string, raw []byte) error {
	att, err := harness.NewImageAttachmentFromBytes(name, mimeType, raw)
	if err != nil {
		if c != nil {
			c.Toast(err.Error())
		}
		return err
	}
	a.nextAttachID++
	att.ID = fmt.Sprintf("att-%d", a.nextAttachID)
	bmp, _ := ui.DecodeBitmap(att.PreviewBytes)
	a.pendingAttachments = append(a.pendingAttachments, PendingAttachment{
		Attachment: att,
		Bitmap:     bmp,
	})
	if c != nil {
		c.Toast("已粘贴图片附件: " + att.Name)
	}
	return nil
}

// HandleDroppedTransfer processes files or images dropped onto the application canvas/composer.
func (a *DeskApp) HandleDroppedTransfer(c *ui.Context, data transfer.Data) {
	if paths, err := data.Files(); err == nil && len(paths) > 0 {
		for _, p := range paths {
			_ = a.AddAttachmentFromPath(c, p)
		}
		return
	}
	if pngBytes, err := data.Read(transfer.PNG); err == nil && len(pngBytes) > 0 {
		name := fmt.Sprintf("drop-%s.png", time.Now().Format("150405"))
		_ = a.AddImageAttachmentBytes(c, name, "image/png", pngBytes)
	}
}

// TryPasteClipboardAttachment checks if the clipboard holds an image screenshot or file list
// when Cmd/Ctrl+V is pressed inside the composer. Returns true if consumed as an attachment.
func (a *DeskApp) TryPasteClipboardAttachment(c *ui.Context) bool {
	if c == nil {
		return false
	}
	data := c.Services().ReadClipboardData(transfer.PNG, transfer.FileList, transfer.Text)
	if files, err := data.Files(); err == nil && len(files) > 0 {
		for _, f := range files {
			_ = a.AddAttachmentFromPath(c, f)
		}
		return true
	}
	// Only intercept as PNG image when there is PNG data and no plain text on the clipboard
	// (so normal code/text copy-paste continues to paste text into the editor!).
	txtBytes, txtErr := data.Read(transfer.Text)
	hasPlainText := txtErr == nil && len(strings.TrimSpace(string(txtBytes))) > 0
	if !hasPlainText {
		if pngBytes, err := data.Read(transfer.PNG); err == nil && len(pngBytes) > 0 {
			name := fmt.Sprintf("paste-%s.png", time.Now().Format("150405"))
			_ = a.AddImageAttachmentBytes(c, name, "image/png", pngBytes)
			return true
		}
		if a.win != nil {
			if rawImg := mygo.Clipboard.ReadImage(); len(rawImg) > 0 {
				name := fmt.Sprintf("paste-%s.png", time.Now().Format("150405"))
				_ = a.AddImageAttachmentBytes(c, name, "image/png", rawImg)
				return true
			}
		}
	}
	return false
}

// TriggerPickAttachments opens the native OS file picker to select images or code/text files.
func (a *DeskApp) TriggerPickAttachments() {
	if a.win == nil || a.pickingAttachment {
		return
	}
	a.pickingAttachment = true
	defaultDir := ""
	if a.mgr != nil {
		t := a.mgr.Router().Target()
		if !t.IsRemote {
			defaultDir = t.WorkDir
		}
	}
	win := a.win
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent:      win,
			Title:       "选择图片或代码文件作为附件",
			DefaultPath: defaultDir,
			Multiple:    true,
			ButtonLabel: "添加附件",
		})
		a.updateUI(func() {
			a.pickingAttachment = false
			if err != nil || len(paths) == 0 {
				return
			}
			for _, p := range paths {
				_ = a.AddAttachmentFromPath(nil, p)
			}
		})
	}()
}

// RemovePendingAttachment deletes a pending composer attachment at index idx.
func (a *DeskApp) RemovePendingAttachment(idx int) {
	if idx < 0 || idx >= len(a.pendingAttachments) {
		return
	}
	a.pendingAttachments = append(a.pendingAttachments[:idx], a.pendingAttachments[idx+1:]...)
}

func (a *DeskApp) consumePendingAttachments() ([]harness.PromptAttachment, []ChatAttachmentBadge) {
	if len(a.pendingAttachments) == 0 {
		return nil, nil
	}
	atts := make([]harness.PromptAttachment, len(a.pendingAttachments))
	badges := make([]ChatAttachmentBadge, len(a.pendingAttachments))
	for i, pa := range a.pendingAttachments {
		atts[i] = pa.Attachment
		badges[i] = ChatAttachmentBadge{
			Name:    pa.Attachment.Name,
			IsImage: pa.Attachment.Kind == harness.AttachmentImage,
			Bitmap:  pa.Bitmap,
		}
	}
	a.pendingAttachments = nil
	return atts, badges
}

func (a *DeskApp) renderComposerAttachmentStrip(c *ui.Context, p Palette) {
	if len(a.pendingAttachments) == 0 {
		return
	}
	ui.Row(c).FillWidth().MinWidth(0).Gap(6).Wrap().Padding(0, 0, 2, 0).Children(func() {
		for idx := range a.pendingAttachments {
			i := idx
			pa := a.pendingAttachments[i]
			ui.Row(c).Height(28).Padding(0, 6, 0, 8).Radius(RadiusSM).
				Background(p.BgElevated).Border(1, p.BorderSubtle).Gap(6).AlignItems(ui.Center).Children(func() {
				if pa.Bitmap != nil {
					ui.Image(c, pa.Bitmap).Size(18, 18)
				} else if pa.Attachment.Kind == harness.AttachmentImage {
					ui.Icon(c, IconImage).Size(13, 13).TextColor(p.Accent)
				} else {
					ui.Icon(c, IconFileText).Size(13, 13).TextColor(p.TextMuted)
				}
				ui.Text(c, pa.Attachment.Name).Font(FontMono).FontSize(FontCaption).TextColor(p.TextPrimary).
					MaxWidth(180).SingleLine().Ellipsis("...")
				ui.Text(c, coding.FormatSize(int(pa.Attachment.SizeBytes))).Font(FontMono).FontSize(10).TextColor(p.TextMuted).SingleLine()

				rmBtn := ui.ButtonBase(c).Size(18, 18).Radius(RadiusSM).AlignItems(ui.Center).Justify(ui.Center).
					Tooltip("移除附件 " + pa.Attachment.Name).Label("移除附件 " + pa.Attachment.Name)
				if rmBtn.Hovered() {
					rmBtn.Background(p.DangerSoft)
				}
				rmBtn.Children(func() {
					ui.Icon(c, IconCross).Size(10.5, 10.5).TextColor(p.TextMuted)
				})
				if rmBtn.Clicked() {
					a.RemovePendingAttachment(i)
				}
			})
		}
	})
}

// ensureWorkspaceFilesIndexed ensures the active workspace's file list is indexed for @file autocomplete.
// For Local workspaces when running headlessly (win == nil), it indexes synchronously once so unit tests
// get deterministic immediate autocomplete; with a live window (or SSH), it indexes asynchronously.
func (a *DeskApp) ensureWorkspaceFilesIndexed() {
	if a.mgr == nil || a.mgr.WorkspaceFilesFresh(25*time.Second) || a.indexingFiles {
		return
	}
	t := a.mgr.Router().Target()
	if !t.IsRemote && a.win == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = a.mgr.RefreshWorkspaceFiles(ctx)
		cancel()
		return
	}
	a.indexingFiles = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, _ = a.mgr.RefreshWorkspaceFiles(ctx)
		a.updateUI(func() {
			a.indexingFiles = false
		})
	}()
}

func (a *DeskApp) activeAtAutocompleteMatches() (query string, matches []string, active bool) {
	q, _, _, ok := harness.ParseActiveAtMention(a.input)
	if !ok {
		a.atDismissedToken = ""
		return "", nil, false
	}
	tokenSig := "@" + q
	if a.atDismissedToken != "" && a.atDismissedToken == tokenSig {
		return q, nil, false
	}
	a.ensureWorkspaceFilesIndexed()
	files := a.mgr.WorkspaceFiles()
	matches = harness.FuzzyMatchFiles(files, q, 8)
	if a.atSelectedIdx >= len(matches) {
		a.atSelectedIdx = max(0, len(matches)-1)
	}
	if a.atSelectedIdx < 0 {
		a.atSelectedIdx = 0
	}
	return q, matches, true
}

func (a *DeskApp) renderAtAutocompletePanel(c *ui.Context, p Palette, query string, matches []string) {
	ui.Column(c).FillWidth().MaxWidth(760).MinWidth(0).Margin(0, 0, 6, 0).
		Radius(RadiusMD).Background(p.BgSurface).Border(1, p.BorderStrong).
		Shadow(0, 8, 24, -6, p.ShadowSoft).Clip().Children(func() {
		ui.Row(c).FillWidth().Height(26).Padding(0, 10).Gap(6).AlignItems(ui.Center).
			Background(p.BgElevated).BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).Children(func() {
			ui.Icon(c, IconAtSign).Size(11.5, 11.5).TextColor(p.Accent)
			title := "引用工作区文件"
			if query != "" {
				title = "匹配文件: " + query
			}
			ui.Text(c, title).FontSize(FontCaption).FontWeight(WeightMedium).TextColor(p.TextSecondary).SingleLine()
			ui.Spacer(c)
			if a.indexingFiles {
				ui.Spinner(c).Size(10, 10)
			}
			ui.Text(c, "↑↓ 选择 · Tab/Enter 插入 · Esc 关闭").FontSize(10).TextColor(p.TextMuted).SingleLine()
		})

		if len(matches) == 0 {
			msg := "未找到匹配文件"
			if a.indexingFiles {
				msg = "正在扫描工作区文件索引..."
			}
			ui.Row(c).FillWidth().Height(30).Padding(0, 12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, msg).FontSize(FontCaption).TextColor(p.TextMuted)
			})
			return
		}

		ui.Column(c).FillWidth().Padding(4).Gap(1).Children(func() {
			for idx, relPath := range matches {
				i := idx
				pathStr := relPath
				selected := i == a.atSelectedIdx
				row := ui.ButtonBase(c).FillWidth().MinWidth(0).Height(28).Padding(0, 8).
					Radius(RadiusSM).Gap(8).AlignItems(ui.Center).Label("引用文件 " + pathStr)
				bg := ui.Transparent
				if selected {
					bg = p.AccentSoft
				} else if row.Hovered() {
					bg = p.BgHover
				}
				base := filepath.Base(pathStr)
				dir := filepath.ToSlash(filepath.Dir(pathStr))
				if dir == "." {
					dir = ""
				}
				row.Background(bg).Children(func() {
					ui.Icon(c, IconFileText).Size(12.5, 12.5).TextColor(map[bool]ui.Color{true: p.Accent, false: p.TextMuted}[selected]).Shrink(0)
					ui.Text(c, base).Font(FontMono).FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextPrimary).Shrink(0).SingleLine()
					if dir != "" {
						ui.Text(c, dir).Font(FontMono).FontSize(FontCaption).TextColor(p.TextMuted).
							Grow(1).MinWidth(0).SingleLine().Ellipsis("...")
					} else {
						ui.Spacer(c)
					}
				})
				if row.Hovered() {
					a.atSelectedIdx = i
				}
				if row.Clicked() {
					a.input = harness.ReplaceActiveAtMention(a.input, pathStr)
					a.atSelectedIdx = 0
				}
			}
		})
	})
}

func (a *DeskApp) syncToolCardMutationSnapshot(card *ChatEntry) {
	if card == nil || a.mgr == nil || card.ToolCallID == "" {
		return
	}
	if snap, ok := a.mgr.GetFileMutation(card.ToolCallID); ok && snap != nil {
		card.DiffLines = snap.DiffLines
		card.DiffAdded = snap.AddedLines
		card.DiffRemoved = snap.RemovedLines
		card.CanRevert = true
		card.Reverted = snap.Reverted
		if snap.ToolName == "write" && len(snap.DiffLines) > 0 {
			card.CardOpen = true
		}
	}
}

// RevertToolEntry rolls back the file mutation recorded for a single edit/write tool card.
func (a *DeskApp) RevertToolEntry(c *ui.Context, entry *ChatEntry) {
	if entry == nil || a.mgr == nil || a.isRunning || entry.ToolCallID == "" {
		return
	}
	if a.win != nil && entry.IsRemote {
		callID := entry.ToolCallID
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			snap, err := a.mgr.RevertFileMutation(ctx, callID)
			a.updateUI(func() {
				if err != nil {
					a.lastError = err.Error()
					return
				}
				entry.Reverted = true
				if snap != nil {
					a.refreshGitStatusAsync()
				}
			})
		}()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	snap, err := a.mgr.RevertFileMutation(ctx, entry.ToolCallID)
	if err != nil {
		a.lastError = err.Error()
		if c != nil {
			c.Toast(err.Error())
		}
		return
	}
	entry.Reverted = true
	if a.win != nil {
		a.refreshGitStatusAsync()
	} else {
		_ = a.mgr.RefreshGitStatus(ctx)
	}
	if c != nil && snap != nil {
		c.Toast("已回滚文件修改: " + snap.DisplayPath)
	}
}

// RefreshGitStatusSync synchronously refreshes the active workspace's Git status.
func (a *DeskApp) RefreshGitStatusSync() harness.GitWorkspaceStatus {
	if a.mgr == nil {
		return harness.GitWorkspaceStatus{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	return a.mgr.RefreshGitStatus(ctx)
}

func (a *DeskApp) refreshGitStatusAsync() {
	if a.mgr == nil || a.probingGit {
		return
	}
	a.probingGit = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = a.mgr.RefreshGitStatus(ctx)
		a.updateUI(func() {
			a.probingGit = false
		})
	}()
}

func (a *DeskApp) renderGitStatusPill(c *ui.Context, p Palette) {
	gs := a.mgr.GitStatus()
	if !gs.IsRepo {
		return
	}
	dirty := gs.DirtyCount()
	pill := ui.ButtonBase(c).MaxWidth(168).MinWidth(0).Height(26).Padding(0, 8).Radius(RadiusSM).
		Gap(5).AlignItems(ui.Center).Shrink(0).
		Tooltip("查看当前 Git 分支与未提交变更")

	bg := p.InputBg
	if pill.Hovered() || a.gitPopoverOpen {
		bg = p.BgHover
	}
	pill.Background(bg).Border(1, p.BorderSubtle).Children(func() {
		ui.Icon(c, IconBranch).Size(11.5, 11.5).TextColor(p.TextSecondary).Shrink(0)
		ui.Text(c, gs.Branch).Font(FontMono).FontSize(FontCaption).FontWeight(WeightMedium).
			TextColor(p.TextPrimary).MinWidth(0).SingleLine().Ellipsis("...")
		if dirty > 0 {
			ui.Row(c).Padding(0, 5).Radius(RadiusPill).Background(p.AccentSoft).Shrink(0).Children(func() {
				ui.Text(c, fmt.Sprintf("%d", dirty)).Font(FontMono).FontFeatures("tnum").
					FontSize(10).FontWeight(WeightSemibold).TextColor(p.Accent)
			})
		}
	})
	if pill.Clicked() {
		a.gitPopoverOpen = !a.gitPopoverOpen
		if a.gitPopoverOpen && a.win != nil {
			a.refreshGitStatusAsync()
		}
	}

	ui.PopoverBase(c, pill, &a.gitPopoverOpen, func(panel ui.Element) {
		panel.Width(340).MaxHeight(380).Margin(4, 0, 0, 0).Radius(RadiusMD).
			Background(p.ModalBg).Border(1, p.BorderStrong).
			Shadow(0, 12, 32, -6, p.ShadowModal).Clip().Children(func() {
			// Header
			ui.Row(c).FillWidth().Height(36).Padding(0, 12).Gap(6).AlignItems(ui.Center).
				BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).Children(func() {
				ui.Icon(c, IconBranch).Size(13, 13).TextColor(p.Accent)
				ui.Text(c, gs.Branch).Font(FontMono).FontWeight(WeightSemibold).FontSize(FontSmall).TextColor(p.TextPrimary).
					Grow(1).MinWidth(0).SingleLine().Ellipsis("...")
				if gs.TotalAdded > 0 || gs.TotalRemoved > 0 {
					ui.Text(c, fmt.Sprintf("+%d / -%d", gs.TotalAdded, gs.TotalRemoved)).
						Font(FontMono).FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextMuted)
				}
				IconTipBtn(c, p, IconRefresh, 24, 12.5, "刷新 Git 状态", func() {
					if a.win != nil {
						a.refreshGitStatusAsync()
					} else {
						a.RefreshGitStatusSync()
					}
				})
			})

			if dirty == 0 {
				ui.Column(c).FillWidth().Padding(20, 14).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "工作区干净，无未提交变更").FontSize(FontSmall).TextColor(p.TextMuted)
				})
				return
			}

			// Changed files list
			ui.Scroll(c).MaxHeight(240).Children(func() {
				ui.Column(c).FillWidth().Padding(6).Gap(2).Children(func() {
					for _, ch := range gs.Changes {
						changeItem := ch
						fb := ui.ButtonBase(c).FillWidth().MinWidth(0).Height(28).Padding(0, 8).Radius(RadiusSM).
							Gap(7).AlignItems(ui.Center).Tooltip("点击插入 @" + changeItem.Path + " 到输入框")
						if fb.Hovered() {
							fb.Background(p.BgHover)
						}
						stCol := p.WarnColor
						switch changeItem.Status {
						case "A":
							stCol = p.DiffAddFg
						case "D":
							stCol = p.DiffDelFg
						case "?":
							stCol = p.TextMuted
						}
						fb.Children(func() {
							ui.Text(c, changeItem.Status).Font(FontMono).FontWeight(WeightSemibold).FontSize(FontCaption).
								TextColor(stCol).Width(14).Shrink(0)
							ui.Text(c, changeItem.Path).Font(FontMono).FontSize(FontCaption).TextColor(p.TextPrimary).
								Grow(1).MinWidth(0).SingleLine().Ellipsis("...")
							if changeItem.Added > 0 || changeItem.Removed > 0 {
								ui.Text(c, fmt.Sprintf("+%d -%d", changeItem.Added, changeItem.Removed)).
									Font(FontMono).FontFeatures("tnum").FontSize(10).TextColor(p.TextMuted).Shrink(0)
							}
						})
						if fb.Clicked() {
							a.input = harness.ReplaceActiveAtMention(a.input, changeItem.Path)
							a.gitPopoverOpen = false
						}
					}
				})
			})

			// Footer quick action: Ask Agent to review git diff
			ui.Row(c).FillWidth().Padding(8, 10).BorderWidth(1, 0, 0, 0).BorderColor(p.BorderSubtle).Justify(ui.End).Children(func() {
				revBtn := ui.ButtonBase(c).Height(26).Padding(0, 10).Radius(RadiusSM).
					Background(p.AccentSoft).Gap(5).AlignItems(ui.Center).Label("让 Agent 审查当前 Git 变更")
				revBtn.Children(func() {
					ui.Icon(c, IconSparkles).Size(11.5, 11.5).TextColor(p.Accent)
					ui.Text(c, "审查未提交变更并建议 Commit").FontSize(FontCaption).FontWeight(WeightMedium).TextColor(p.Accent)
				})
				if revBtn.Clicked() {
					a.input = "请使用 git status / git diff 审查当前工作区的未提交代码变更，检查潜在问题并给出规范的 Commit Message 建议。"
					a.gitPopoverOpen = false
				}
			})
		})
	})
}

func (a *DeskApp) renderContextBudgetPill(c *ui.Context, p Palette) {
	usage := a.mgr.ContextUsageSnapshot()
	if usage.ContextTokens <= 0 && !a.isCompacting {
		return
	}

	pill := ui.ButtonBase(c).Height(26).Padding(0, 8).Radius(RadiusSM).Gap(5).AlignItems(ui.Center).Shrink(0).
		Tooltip("查看上下文窗口用量或主动压缩上下文 (/compact)")
	bg := ui.Transparent
	fg := p.TextMuted
	if usage.Percent >= 75 {
		bg = p.WarnSoft
		fg = p.WarnColor
	} else if pill.Hovered() || a.ctxPopoverOpen {
		bg = p.BgHover
		fg = p.TextSecondary
	}
	pill.Background(bg).Children(func() {
		if a.isCompacting {
			ui.Spinner(c).Size(11, 11)
			ui.Text(c, "压缩中...").FontSize(FontCaption).TextColor(p.Accent).SingleLine()
		} else {
			ui.Text(c, fmt.Sprintf("~%s", formatTokenCount(usage.ContextTokens))).
				Font(FontMono).FontFeatures("tnum").FontSize(FontCaption).TextColor(fg).SingleLine()
			if usage.Percent >= 1.0 {
				ui.Text(c, fmt.Sprintf("(%.0f%%)", usage.Percent)).
					Font(FontMono).FontFeatures("tnum").FontSize(10).TextColor(fg).SingleLine()
			}
		}
	})
	if pill.Clicked() {
		a.ctxPopoverOpen = !a.ctxPopoverOpen
	}

	ui.PopoverBase(c, pill, &a.ctxPopoverOpen, func(panel ui.Element) {
		panel.Width(300).Margin(4, 0, 0, 0).Padding(12).Radius(RadiusMD).
			Background(p.ModalBg).Border(1, p.BorderStrong).
			Shadow(0, 12, 32, -6, p.ShadowModal).Gap(10).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
				ui.Text(c, "上下文窗口与 Token 用量").FontWeight(WeightSemibold).FontSize(FontSmall).TextColor(p.TextPrimary)
				ui.Spacer(c)
				ui.Text(c, fmt.Sprintf("%.1f%%", usage.Percent)).Font(FontMono).FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextSecondary)
			})

			// Progress bar
			ratio := min(float64(1.0), max(float64(0.0), usage.Percent/100.0))
			ui.Progress(c, ratio).FillWidth()

			ui.Text(c, fmt.Sprintf("当前上下文约 %s / 窗口上限 %s tokens", formatTokenCount(usage.ContextTokens), formatTokenCount(usage.ContextWindow))).
				Font(FontMono).FontSize(FontCaption).TextColor(p.TextMuted)

			if usage.SessionInputTokens > 0 || usage.SessionOutputTokens > 0 {
				ui.Column(c).FillWidth().Padding(8, 10).Radius(RadiusSM).Background(p.BgElevated).Gap(4).Children(func() {
					ui.Row(c).FillWidth().Children(func() {
						ui.Text(c, "本会话累计输入").FontSize(FontCaption).TextColor(p.TextMuted)
						ui.Spacer(c)
						ui.Text(c, formatTokenCount(usage.SessionInputTokens)).Font(FontMono).FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextPrimary)
					})
					ui.Row(c).FillWidth().Children(func() {
						ui.Text(c, "本会话累计输出").FontSize(FontCaption).TextColor(p.TextMuted)
						ui.Spacer(c)
						ui.Text(c, formatTokenCount(usage.SessionOutputTokens)).Font(FontMono).FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextPrimary)
					})
					if usage.SessionCacheReadTokens > 0 {
						ui.Row(c).FillWidth().Children(func() {
							ui.Text(c, "缓存命中读取").FontSize(FontCaption).TextColor(p.TextMuted)
							ui.Spacer(c)
							ui.Text(c, formatTokenCount(usage.SessionCacheReadTokens)).Font(FontMono).FontFeatures("tnum").FontSize(FontCaption).TextColor(p.LocalColor)
						})
					}
				})
			}

			canCompact := !a.isRunning && !a.isCompacting && len(a.entries) > 0
			compBtn := ui.ButtonBase(c).FillWidth().Height(30).Radius(RadiusSM).
				Background(map[bool]ui.Color{true: p.BtnPrimaryBg, false: p.BgElevated}[canCompact]).
				Gap(6).AlignItems(ui.Center).Justify(ui.Center).Disabled(!canCompact).
				Label("压缩上下文 (/compact)")
			compBtn.Children(func() {
				fgCol := p.BtnPrimaryFg
				if !canCompact {
					fgCol = p.TextMuted
				}
				ui.Icon(c, IconCompress).Size(12.5, 12.5).TextColor(fgCol)
				ui.Text(c, "主动压缩上下文 (/compact)").FontSize(FontSmall).FontWeight(WeightMedium).TextColor(fgCol)
			})
			if compBtn.Clicked() {
				a.ctxPopoverOpen = false
				a.CompactActiveSession(c, "")
			}
		})
	})
}

// CompactActiveSession summarizes the active conversation into a structured checkpoint and preserves
// the uncompacted transcript as a recoverable branch in the JSONL session tree.
func (a *DeskApp) CompactActiveSession(c *ui.Context, focusHint string) {
	if a.isRunning || a.isCompacting || a.mgr == nil {
		return
	}
	a.isCompacting = true
	a.lastError = ""
	prof := a.cfg.ActiveProfile()

	runCompact := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
		defer cancel()
		res, err := a.mgr.CompactSession(ctx, focusHint, prof)
		a.updateUI(func() {
			a.isCompacting = false
			if err != nil {
				a.lastError = "压缩上下文失败: " + err.Error()
				return
			}
			a.toolCards = map[string]*ChatEntry{}
			a.currentAssistant = nil
			a.entries = RebuildEntriesFromProjected(res.Projected, &a.nextEntryID)
			a.treeSnap = res.TreeSnap
			a.refreshSessionsList()
		})
	}

	if a.win != nil {
		go runCompact()
		if c != nil {
			c.Toast("正在提炼并压缩当前会话上下文...")
		}
	} else {
		runCompact()
	}
}

// ExecBangCommand runs an explicit "! <command>" shell instruction directly on the active
// Local or Remote SSH workspace and renders the result inline in the conversation timeline.
func (a *DeskApp) ExecBangCommand(c *ui.Context, rawCmd string) bool {
	if a.isRunning || a.mgr == nil {
		return false
	}
	cmdStr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rawCmd), "!"))
	if cmdStr == "" {
		a.input = ""
		return true
	}

	a.input = ""
	a.lastError = ""
	badge, isRem := a.currentTurnBadge()
	toolBadge, _ := a.toolCardBadgeFor("")

	userEntry := &ChatEntry{
		ID:          a.allocID(),
		Kind:        EntryUser,
		Text:        "! " + cmdStr,
		TargetBadge: badge,
		IsRemote:    isRem,
		Done:        true,
	}
	toolEntry := &ChatEntry{
		ID:          a.allocID(),
		Kind:        EntryTool,
		ToolCallID:  fmt.Sprintf("bang-%d", time.Now().UnixNano()),
		ToolName:    "bash",
		ToolSummary: singleLinePreview(cmdStr, 80),
		TargetBadge: toolBadge,
		IsRemote:    isRem,
		ToolRunning: true,
		CardOpen:    true,
	}
	a.entries = append(a.entries, userEntry, toolEntry)

	a.isRunning = true
	a.activeToolStatus = "bash · " + singleLinePreview(cmdStr, 36)
	ctx, cancel := context.WithCancel(context.Background())
	a.cancelRun = cancel

	runDirect := func() {
		defer cancel()
		out, durMs, err := a.mgr.ExecDirectBash(ctx, cmdStr, func(partial string) {
			a.updateUI(func() {
				toolEntry.ToolOutput = StripANSI(partial)
			})
		})
		a.updateUI(func() {
			a.isRunning = false
			a.activeToolStatus = ""
			toolEntry.ToolRunning = false
			toolEntry.Done = true
			toolEntry.DurationMs = durMs
			toolEntry.ToolError = err != nil
			if cleaned := StripANSI(out); cleaned != "" {
				toolEntry.ToolOutput = cleaned
			}
			if a.win != nil {
				a.refreshGitStatusAsync()
			}
		})
	}

	if a.win != nil {
		go runDirect()
	} else {
		runDirect()
	}
	return true
}

func (a *DeskApp) handleSlashCommand(c *ui.Context, trimmed string) bool {
	if !strings.HasPrefix(trimmed, "/") {
		return false
	}
	fields := strings.Fields(trimmed)
	cmd := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))

	switch cmd {
	case "/compact":
		a.input = ""
		a.CompactActiveSession(c, rest)
		return true
	case "/export":
		a.input = ""
		a.ExportActiveSession(c, rest)
		return true
	case "/mcp":
		a.input = ""
		a.settingsModal.ActiveTab = 3
		a.settingsModal.Open = true
		return true
	case "/tree", "/branches":
		a.input = ""
		if a.mgr != nil {
			a.treeSnap = a.mgr.SessionTreeSnapshot()
		}
		a.branchPopoverOpen = true
		return true
	case "/new", "/clear":
		a.input = ""
		a.startNewSession(c)
		return true
	}
	return false
}

func (a *DeskApp) buildPaletteCommands() []PaletteCommand {
	mod := platformModLabel()
	cmds := []PaletteCommand{
		{
			ID:       "sess-new",
			Category: "会话",
			Title:    "新建对话",
			Subtitle: "在当前工作区开启全新对话上下文",
			Shortcut: mod + "N",
			Icon:     IconCompose,
			Run: func(c *ui.Context) {
				a.startNewSession(c)
			},
		},
		{
			ID:       "sess-compact",
			Category: "上下文",
			Title:    "主动压缩上下文 (/compact)",
			Subtitle: "生成检查点摘要释放上下文窗口，原分支保留在分支树中",
			Icon:     IconCompress,
			Run: func(c *ui.Context) {
				a.CompactActiveSession(c, "")
			},
		},
		{
			ID:       "sess-export",
			Category: "会话",
			Title:    "导出当前对话为 Markdown (/export)",
			Subtitle: "将提问、AI 回复、工具输出与代码 Diff 导出为 .md 文件",
			Icon:     IconDownload,
			Run: func(c *ui.Context) {
				a.ExportActiveSession(c, "")
			},
		},
		{
			ID:       "sess-tree",
			Category: "会话",
			Title:    "查看当前对话分支树 (/tree)",
			Subtitle: "打开顶部 JSONL 对话回溯与分叉拓扑浮窗",
			Icon:     IconBranch,
			Run: func(_ *ui.Context) {
				if a.mgr != nil {
					a.treeSnap = a.mgr.SessionTreeSnapshot()
				}
				a.branchPopoverOpen = true
			},
		},
	}

	// Workspace switch commands
	for _, wt := range a.cfg.Targets {
		targetCopy := wt
		icon := IconFolder
		if targetCopy.IsRemote {
			icon = IconCloud
		}
		cmds = append(cmds, PaletteCommand{
			ID:       "ws-" + targetCopy.ID,
			Category: "工作区",
			Title:    "切换到: " + targetCopy.ShortName(),
			Subtitle: targetCopy.DisplaySub(),
			Icon:     icon,
			Run: func(c *ui.Context) {
				a.SwitchWorkspace(c, targetCopy)
			},
		})
	}

	cmds = append(cmds,
		PaletteCommand{
			ID:       "ws-manage",
			Category: "工作区",
			Title:    "管理本地与远程 SSH 工作区...",
			Subtitle: "添加本地目录、浏览远程服务器目录或修改信任状态",
			Icon:     IconServer,
			Run: func(_ *ui.Context) {
				a.settingsModal.ActiveTab = 1
				a.settingsModal.Open = true
			},
		},
		PaletteCommand{
			ID:       "git-refresh",
			Category: "工作区",
			Title:    "刷新 Git 与 SSH 状态",
			Subtitle: "重新探测当前分支变更与远程连接延迟",
			Icon:     IconRefresh,
			Run: func(_ *ui.Context) {
				if a.win != nil {
					a.refreshGitStatusAsync()
					a.probeActiveSSHAsync(false)
				} else {
					a.RefreshGitStatusSync()
				}
			},
		},
	)

	// Active profile model quick-switch commands
	if prof := a.cfg.ActiveProfile(); prof != nil {
		for _, mID := range prof.AvailableModels() {
			modelID := mID
			cmds = append(cmds, PaletteCommand{
				ID:       "model-" + modelID,
				Category: "模型",
				Title:    "切换模型: " + modelID,
				Subtitle: prof.Name,
				Icon:     IconSparkles,
				Run: func(c *ui.Context) {
					a.switchModel(c, modelID)
				},
			})
		}
	}

	cmds = append(cmds,
		PaletteCommand{
			ID:       "mcp-manage",
			Category: "扩展",
			Title:    "管理 MCP 工具服务 (/mcp)",
			Subtitle: "添加或检测 Stdio 本地进程与 Streamable HTTP 远程 MCP 服务",
			Icon:     IconPlug,
			Run: func(_ *ui.Context) {
				a.settingsModal.ActiveTab = 3
				a.settingsModal.Open = true
			},
		},
		PaletteCommand{
			ID:       "view-sidebar",
			Category: "视图",
			Title:    "展开 / 收起左侧边栏",
			Shortcut: mod + "B",
			Icon:     IconSidebarLeft,
			Run: func(_ *ui.Context) {
				a.sidebarCollapsed = !a.sidebarCollapsed
			},
		},
		PaletteCommand{
			ID:       "view-theme",
			Category: "视图",
			Title:    "切换深色 / 浅色外观",
			Shortcut: mod + "⇧L",
			Icon:     IconSun,
			Run: func(c *ui.Context) {
				a.toggleTheme(c)
			},
		},
		PaletteCommand{
			ID:       "diag-copy",
			Category: "诊断",
			Title:    "复制运行诊断信息与日志摘要",
			Subtitle: "复制系统环境、SSH/MCP 状态与最新运行日志，便于排查问题",
			Icon:     IconCopy,
			Run: func(c *ui.Context) {
				a.CopyDiagnosticSummary(c)
			},
		},
		PaletteCommand{
			ID:       "settings-open",
			Category: "设置",
			Title:    "打开设置中心 (渠道 / 工作区 / 安全 / MCP)",
			Shortcut: mod + ",",
			Icon:     IconSettings,
			Run: func(_ *ui.Context) {
				a.settingsModal.ActiveTab = 0
				a.settingsModal.Open = true
			},
		},
	)
	return cmds
}
