package ui

import (
	"context"
	"fmt"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"

	"piui/internal/harness"
	"piui/internal/store"
)

// DeskApp is the root state for the native MyGo GPU desktop client.
type DeskApp struct {
	win *mygo.Window
	cfg *store.AppConfig
	mgr *harness.Manager

	// Conversation timeline
	entries          []*ChatEntry
	nextEntryID      int
	currentAssistant *ChatEntry
	lastUserEntry    *ChatEntry
	toolCards        map[string]*ChatEntry
	chatList         ui.ListState
	mdOpts           markdown.Options
	mdThemeLight     *bool

	// Composer & run state
	input            string
	isRunning        bool
	activeToolStatus string
	lastError        string
	cancelRun        context.CancelFunc

	// Sidebars, Split Panes, Session Branch Tree & Modals
	sidebarWidth     float32
	sidebarCollapsed bool
	sidebarTab       int // 0 = 会话历史 (.jsonl), 1 = 分支树 (Session Tree)
	savedSessions    []harness.SavedSessionSummary
	activeSessID     string
	treeSnap         *harness.SessionTreeSnapshot
	termDrawer       *TermDrawer
	settingsModal    SettingsModal
	approvalModal    ApprovalModal
}

// NewDeskApp initializes the application state, config, and sky-valley/pi harness.
func NewDeskApp(workDir string) (*DeskApp, error) {
	cfg := store.LoadOrInit(workDir)
	app := &DeskApp{
		cfg:          cfg,
		toolCards:    map[string]*ChatEntry{},
		sidebarWidth: DefaultSidebarWidth,
		termDrawer:   NewTermDrawer(),
	}
	app.mdOpts.Reveal = &markdown.Reveal{Unit: markdown.ByWord}

	mgr, err := harness.NewManager(
		workDir,
		cfg,
		func(chunk string) {
			app.updateUI(func() {
				app.termDrawer.AppendChunk(chunk)
			})
		},
		app.handleAgentEvent,
	)
	if err != nil {
		return nil, err
	}
	mgr.SafetyGate().SetPromptHandler(func(req *harness.ApprovalRequest) {
		app.updateUI(func() {
			app.approvalModal.Enqueue(req)
		})
	})

	app.mgr = mgr
	var startupWarns []string
	if cfg.LoadWarning != "" {
		startupWarns = append(startupWarns, cfg.LoadWarning)
	}
	if warn := mgr.RecorderWarning(); warn != "" {
		startupWarns = append(startupWarns, warn)
	}
	if len(startupWarns) > 0 {
		app.lastError = strings.Join(startupWarns, " | ")
	}
	app.refreshSessionsList()
	return app, nil
}

// BindWindow attaches the native MyGo window handle once ready.
func (a *DeskApp) BindWindow(w *mygo.Window) {
	a.win = w
}

func (a *DeskApp) updateUI(fn func()) {
	if a.win != nil {
		a.win.Update(fn)
	} else {
		fn()
	}
}

func (a *DeskApp) allocID() int {
	a.nextEntryID++
	return a.nextEntryID
}

func (a *DeskApp) currentTurnBadge() (string, bool) {
	t := a.mgr.Router().Target()
	return t.TurnBadge(), t.IsRemote
}

func (a *DeskApp) toolCardBadgeFor(toolCallID string) (string, bool) {
	t := a.mgr.Router().PeekToolTarget(toolCallID)
	return t.ToolCardBadge(), t.IsRemote
}

func (a *DeskApp) saveConfigOrWarn(c *ui.Context) bool {
	if err := a.cfg.Save(); err != nil {
		msg := "保存配置文件失败: " + err.Error()
		a.lastError = msg
		if c != nil {
			c.Toast(msg)
		}
		return false
	}
	return true
}

func (a *DeskApp) applyProfileOrWarn(c *ui.Context, prof *store.ProviderProfile) bool {
	if a.mgr == nil {
		return true
	}
	if err := a.mgr.ApplyProfile(prof); err != nil {
		msg := "应用模型渠道配置失败: " + err.Error()
		a.lastError = msg
		if c != nil {
			c.Toast(msg)
		}
		return false
	}
	return true
}

func (a *DeskApp) refreshSessionsList() {
	if a.mgr != nil {
		a.savedSessions = a.mgr.ListSavedSessionSummaries()
		a.treeSnap = a.mgr.SessionTreeSnapshot()
		if a.activeSessID == "" && a.treeSnap != nil {
			a.activeSessID = a.treeSnap.SessionID
		}
	}
}

func (a *DeskApp) startNewSession(c *ui.Context) {
	if a.isRunning || a.mgr == nil {
		return
	}
	if err := a.mgr.InitOrResetSession(a.cfg.ActiveProfile()); err != nil {
		a.lastError = "新建会话初始化失败: " + err.Error()
		if c != nil {
			c.Toast(a.lastError)
		}
		return
	}
	a.entries = nil
	a.toolCards = map[string]*ChatEntry{}
	a.currentAssistant = nil
	a.lastUserEntry = nil
	a.activeSessID = ""
	a.treeSnap = nil
	a.approvalModal.Clear()
	a.lastError = a.mgr.RecorderWarning()
	a.refreshSessionsList()
	if c != nil {
		c.Toast("已开启新会话")
	}
}

func (a *DeskApp) toggleTheme(c *ui.Context) {
	next := a.cfg.ToggleTheme()
	if !a.saveConfigOrWarn(c) {
		return
	}
	if c != nil {
		if next == store.ThemeLight {
			c.Toast("已切换至浅色主题 (Antigravity Daylight)")
		} else {
			c.Toast("已切换至深色主题 (Antigravity Obsidian)")
		}
	}
}

func (a *DeskApp) finalizeCurrentAssistant(msg agent.AgentMessage) {
	if a.currentAssistant == nil {
		return
	}
	SyncAssistantEntryFromMessage(a.currentAssistant, msg)
	if a.currentAssistant.EntryID == "" && a.mgr != nil {
		a.currentAssistant.EntryID = a.mgr.LastRecordedEntryID()
	}
	a.currentAssistant.Done = true
	if a.currentAssistant.IsEmpty() {
		// Remove empty assistant placeholder (e.g. a pure tool-call turn with no text/thinking)
		for i := len(a.entries) - 1; i >= 0; i-- {
			if a.entries[i] == a.currentAssistant {
				a.entries = append(a.entries[:i], a.entries[i+1:]...)
				break
			}
		}
	}
	a.currentAssistant = nil
}

// handleAgentEvent processes streaming events from sky-valley/pi/coding.Session.
func (a *DeskApp) handleAgentEvent(_ context.Context, ev agent.AgentEvent) error {
	a.updateUI(func() {
		switch ev.Type {
		case agent.EvMessageStart:
			if ev.Message != nil && ev.Message.MessageRole() == ai.RoleAssistant {
				a.finalizeCurrentAssistant(nil)
				parentID := ""
				if a.mgr != nil {
					parentID = a.mgr.LastRecordedEntryID()
				}
				badge, isRem := a.currentTurnBadge()
				entry := &ChatEntry{
					ID:            a.allocID(),
					Kind:          EntryAssistant,
					TargetBadge:   badge,
					IsRemote:      isRem,
					ParentEntryID: parentID,
				}
				SyncAssistantEntryFromMessage(entry, ev.Message)
				a.currentAssistant = entry
				a.entries = append(a.entries, entry)
			}

		case agent.EvMessageUpdate:
			if ev.AssistantMessageEvent == nil {
				return
			}
			if a.currentAssistant == nil || a.currentAssistant.Done {
				parentID := ""
				if a.mgr != nil {
					parentID = a.mgr.LastRecordedEntryID()
				}
				badge, isRem := a.currentTurnBadge()
				entry := &ChatEntry{
					ID:            a.allocID(),
					Kind:          EntryAssistant,
					TargetBadge:   badge,
					IsRemote:      isRem,
					ParentEntryID: parentID,
				}
				a.currentAssistant = entry
				a.entries = append(a.entries, entry)
			}
			switch ev.AssistantMessageEvent.Type {
			case ai.EventTextDelta:
				a.currentAssistant.Text += ev.AssistantMessageEvent.Delta
			case ai.EventThinkingDelta:
				a.currentAssistant.Thinking += ev.AssistantMessageEvent.Delta
			}

		case agent.EvMessageEnd:
			if ev.Message != nil {
				switch ev.Message.MessageRole() {
				case ai.RoleUser:
					if a.lastUserEntry != nil && a.mgr != nil {
						a.lastUserEntry.EntryID = a.mgr.LastRecordedEntryID()
					}
				case ai.RoleAssistant:
					a.finalizeCurrentAssistant(ev.Message)
				case ai.RoleToolResult:
					// Advance the safe checkout EntryID of the preceding assistant turn past this toolResult
					if a.mgr != nil {
						latestID := a.mgr.LastRecordedEntryID()
						if latestID != "" {
							for i := len(a.entries) - 1; i >= 0; i-- {
								if a.entries[i].Kind == EntryAssistant {
									a.entries[i].EntryID = latestID
									break
								}
							}
						}
					}
				}
			} else {
				a.finalizeCurrentAssistant(nil)
			}
			if a.mgr != nil {
				a.treeSnap = a.mgr.SessionTreeSnapshot()
			}

		case agent.EvToolExecutionStart:
			a.finalizeCurrentAssistant(nil)
			sum, dOld, dNew := SummarizeToolArgs(ev.ToolName, ev.Args)
			badge, isRem := a.toolCardBadgeFor(ev.ToolCallID)
			card := &ChatEntry{
				ID:          a.allocID(),
				Kind:        EntryTool,
				ToolCallID:  ev.ToolCallID,
				ToolName:    ev.ToolName,
				ToolSummary: sum,
				DiffOld:     dOld,
				DiffNew:     dNew,
				ToolRunning: true,
				TargetBadge: badge,
				IsRemote:    isRem,
				CardOpen:    ev.ToolName == "edit",
			}
			a.toolCards[ev.ToolCallID] = card
			a.entries = append(a.entries, card)
			a.activeToolStatus = fmt.Sprintf("%s · %s", ev.ToolName, singleLinePreview(sum, 36))

		case agent.EvToolExecutionUpdate:
			if card, ok := a.toolCards[ev.ToolCallID]; ok {
				if partial := ExtractResultText(ev.PartialResult); partial != "" {
					card.ToolOutput = partial
				}
			}

		case agent.EvToolExecutionEnd:
			if card, ok := a.toolCards[ev.ToolCallID]; ok {
				card.ToolRunning = false
				card.ToolError = ev.IsError
				if ev.DurationMs != nil {
					card.DurationMs = *ev.DurationMs
				}
				if out := ExtractResultText(ev.Result); out != "" {
					card.ToolOutput = out
				}
			}
			a.activeToolStatus = ""
		}
	})
	return nil
}

// submitPrompt either starts a new agent turn or injects a Steer message mid-run.
func (a *DeskApp) submitPrompt(raw string) {
	prompt := strings.TrimSpace(raw)
	if prompt == "" {
		return
	}
	a.input = ""
	a.lastError = ""

	parentID := ""
	if a.mgr != nil {
		parentID = a.mgr.LastRecordedEntryID()
	}
	badge, isRem := a.currentTurnBadge()

	if a.isRunning {
		userEntry := &ChatEntry{
			ID:            a.allocID(),
			Kind:          EntryUser,
			Text:          prompt,
			Steered:       true,
			TargetBadge:   badge,
			IsRemote:      isRem,
			ParentEntryID: parentID,
			Done:          true,
		}
		a.lastUserEntry = userEntry
		a.entries = append(a.entries, userEntry)
		a.mgr.Steer(prompt)
		return
	}

	userEntry := &ChatEntry{
		ID:            a.allocID(),
		Kind:          EntryUser,
		Text:          prompt,
		TargetBadge:   badge,
		IsRemote:      isRem,
		ParentEntryID: parentID,
		Done:          true,
	}
	a.lastUserEntry = userEntry
	a.entries = append(a.entries, userEntry)

	a.isRunning = true
	ctx, cancel := context.WithCancel(context.Background())
	a.cancelRun = cancel

	go func() {
		res, err := a.mgr.Run(ctx, prompt)
		a.updateUI(func() {
			a.isRunning = false
			a.activeToolStatus = ""
			a.approvalModal.Clear()
			a.finalizeCurrentAssistant(nil)

			runErr := ""
			if err != nil && ctx.Err() == nil {
				runErr = err.Error()
			} else if res != nil && res.ErrorMessage != "" {
				runErr = res.ErrorMessage
			}
			if runErr != "" {
				hasTurnError := false
				for i := len(a.entries) - 1; i >= 0; i-- {
					if a.entries[i].Kind == EntryUser {
						break
					}
					if a.entries[i].Kind == EntryAssistant && a.entries[i].ErrorText != "" {
						hasTurnError = true
						break
					}
				}
				if !hasTurnError {
					a.entries = append(a.entries, &ChatEntry{
						ID:          a.allocID(),
						Kind:        EntryAssistant,
						ErrorText:   runErr,
						TargetBadge: badge,
						IsRemote:    isRem,
						Done:        true,
					})
				}
			}
			a.refreshSessionsList()
		})
	}()
}

// forkFromChatEntry rewinds or branches the active JSONL session tree from a clicked ChatEntry.
func (a *DeskApp) forkFromChatEntry(c *ui.Context, entry *ChatEntry) {
	if a.isRunning || entry == nil || a.mgr == nil {
		return
	}

	if entry.Kind == EntryUser {
		projected, snap, err := a.mgr.CheckoutBranch(entry.ParentEntryID, a.cfg.ActiveProfile())
		if err != nil {
			a.lastError = err.Error()
			return
		}
		a.toolCards = map[string]*ChatEntry{}
		a.currentAssistant = nil
		a.entries = RebuildEntriesFromProjected(projected, &a.nextEntryID)
		a.treeSnap = snap
		a.input = entry.Text
		a.sidebarTab = 1
		a.sidebarCollapsed = false
		a.lastError = ""
		c.Toast("已回退至该提问前并回填输入框，修改后发送将创建新分支")
		return
	}

	projected, snap, err := a.mgr.CheckoutBranch(entry.EntryID, a.cfg.ActiveProfile())
	if err != nil {
		a.lastError = err.Error()
		return
	}
	a.toolCards = map[string]*ChatEntry{}
	a.currentAssistant = nil
	a.entries = RebuildEntriesFromProjected(projected, &a.nextEntryID)
	a.treeSnap = snap
	a.sidebarTab = 1
	a.sidebarCollapsed = false
	a.lastError = ""
	c.Toast("已定位至分支节点 #" + shortEntryID(entry.EntryID) + "，继续发送消息将分叉出新分支")
}

func (a *DeskApp) syncMarkdownOptions(isLight bool, p Palette) {
	if a.mdThemeLight == nil || *a.mdThemeLight != isLight {
		flag := isLight
		a.mdThemeLight = &flag
		a.mdOpts.Theme = BuildMarkdownTheme(p)
		pal := p
		a.mdOpts.CodeBlock = func(c *ui.Context, b *markdown.CodeBlockView) {
			RenderMarkdownCodeBlock(c, pal, b)
		}
	}
}

// View builds the declarative Antigravity GPU interface every frame.
func (a *DeskApp) View(c *ui.Context) {
	isLight := a.cfg.IsLightTheme()
	p := PaletteFor(isLight)
	c.SetTheme(BuildMyGoTheme(isLight))
	a.syncMarkdownOptions(isLight, p)

	if a.sidebarWidth <= 0 {
		a.sidebarWidth = DefaultSidebarWidth
	}

	// Global Window Keyboard Shortcuts
	c.OnWindowShortcut(ui.Cmd, ui.Key(','), func() {
		a.settingsModal.Open = true
	})
	c.OnWindowShortcut(ui.Cmd, ui.Key('n'), func() {
		a.startNewSession(c)
	})
	c.OnWindowShortcut(ui.Cmd, ui.Key('j'), func() {
		a.termDrawer.Open = !a.termDrawer.Open
	})
	c.OnWindowShortcut(ui.Cmd, ui.Key('b'), func() {
		a.sidebarCollapsed = !a.sidebarCollapsed
	})
	c.OnWindowShortcut(ui.Cmd|ui.Shift, ui.Key('l'), func() {
		a.toggleTheme(c)
	})

	renderMainAndTerm := func() {
		if a.termDrawer.Open {
			if a.termDrawer.Width <= 0 {
				a.termDrawer.Width = DefaultTermWidth
			}
			// Right-to-left split puts TermDrawer (first) on the right with resizable divider,
			// while both child panes restore LeftToRight internally.
			ui.Split(c, &a.termDrawer.Width, func() {
				a.termDrawer.View(c, p)
			}, func() {
				a.renderCenterMain(c, p)
			}).Direction(ui.RightToLeft).SplitReset(DefaultTermWidth).Fill()
		} else {
			a.renderCenterMain(c, p)
		}
	}

	if a.sidebarCollapsed {
		ui.Row(c).Fill().Background(p.BgCanvas).Children(renderMainAndTerm)
	} else {
		ui.Split(c, &a.sidebarWidth, func() {
			a.renderLeftSidebar(c, p)
		}, renderMainAndTerm).SplitReset(DefaultSidebarWidth).Fill().Background(p.BgCanvas)
	}

	// Settings Modal (API Endpoints, Workspaces & Safety Policy)
	a.settingsModal.Render(c, p, a.cfg, a.mgr, a.updateUI, func() error {
		if err := a.mgr.ApplyProfile(a.cfg.ActiveProfile()); err != nil {
			a.lastError = "应用渠道配置失败: " + err.Error()
			return err
		}
		a.mgr.SetSafetyPolicy(a.cfg.SafetyPolicy)
		if target := a.cfg.ActiveTarget(); target != nil {
			a.mgr.SwitchTarget(*target)
		}
		return nil
	})

	// Human-in-the-Loop High-Risk Tool Approval Modal
	if a.mgr != nil {
		a.approvalModal.Render(c, p, a.mgr.SafetyGate())
	}
}

func (a *DeskApp) renderLeftSidebar(c *ui.Context, p Palette) {
	activeTarget := a.mgr.Router().Target()
	bar := c.TitleBar()
	leftHeaderPad := max(bar.Left, 12)

	ui.Column(c).Fill().Clip().Background(p.BgSidebar).Children(func() {
		// Row 0: Unified 44px Top Header Bar (aligns pixel-for-pixel with CenterMain & TermDrawer)
		ui.Row(c).FillWidth().Height(TopBarHeight).Padding(0, 8, 0, leftHeaderPad).Gap(4).
			AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).
			DragWindow().Children(func() {
			if bar.Left == 0 {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Padding(0, 2).Children(func() {
					ui.Icon(c, IconSparkles).Size(14, 14).TextColor(p.Accent)
					ui.Text(c, "PiUI").FontWeight(WeightSemibold).FontSize(FontBody).TextColor(p.TextPrimary).SingleLine()
				})
			}
			ui.Spacer(c)
			IconTipBtn(c, p, IconSidebarLeft, 28, 14, "收起侧边栏 (Ctrl/Cmd+B)", func() {
				a.sidebarCollapsed = true
			})
			nsBtn := ui.ButtonBase(c).Size(28, 28).Radius(RadiusSM).
				AlignItems(ui.Center).Justify(ui.Center).
				Disabled(a.isRunning).Tooltip("新建对话 (Ctrl/Cmd+N)").Label("新建对话")
			nsBg := ui.Transparent
			nsFg := p.TextSecondary
			if a.isRunning {
				nsFg = p.TextMuted
			} else if nsBtn.Hovered() || nsBtn.Pressed() {
				nsBg = p.BgHover
				nsFg = p.TextPrimary
			}
			nsBtn.Background(nsBg).Children(func() {
				ui.Icon(c, IconCompose).Size(14, 14).TextColor(nsFg)
			})
			if nsBtn.Clicked() {
				a.startNewSession(c)
			}
		})

		// Section 1: Workspace Target Hot-Switcher — Quiet & Neutral by Default
		ui.Column(c).FillWidth().Padding(10, 8, 10, 8).Gap(5).
			BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).Children(func() {
			ui.Row(c).FillWidth().Padding(0, 4).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "工作区").FontWeight(WeightMedium).FontSize(FontCaption).TextColor(p.TextMuted)
				ui.Spacer(c)
				IconTipBtn(c, p, IconPlus, 22, 13, "添加本地目录或远程 SSH 目标", func() {
					a.settingsModal.ActiveTab = 1
					a.settingsModal.Open = true
				})
			})

			hasPendingApproval := a.approvalModal.PendingCount() > 0
			// Sized to cleanly fit 4 rows (4*36 + 3*2 = 150px) without awkward mid-row clipping
			ui.Scroll(c).MaxHeight(152).FillWidth().Children(func() {
				ui.Column(c).FillWidth().Gap(2).Children(func() {
					for i := range a.cfg.Targets {
						wt := a.cfg.Targets[i]
						isSelected := wt.ID == activeTarget.ID

						icon := IconFolder
						iconBg := p.BgElevated
						iconFg := p.TextSecondary
						if isSelected && !wt.IsRemote {
							iconFg = p.TextPrimary
						}
						if wt.IsRemote {
							icon = IconServer
							iconBg = p.SSHSoft
							iconFg = p.SSHColor
						}

						b := ui.ButtonBase(c).FillWidth().MinWidth(0).Height(36).Padding(0, 8).Radius(RadiusMD).
							Gap(8).AlignItems(ui.Center).Disabled(hasPendingApproval)
						targetBg := ui.Transparent
						nameCol := p.TextSecondary
						if isSelected {
							targetBg = p.BgActive
							nameCol = p.TextPrimary
						} else if b.Hovered() && !hasPendingApproval {
							targetBg = p.BgHover
							nameCol = p.TextPrimary
						}

						tip := "切换执行环境至 " + wt.ShortName() + " (" + wt.DisplaySub() + ")"
						if hasPendingApproval {
							tip = "请先处理当前挂起的安全审批弹窗，再切换执行环境"
						}

						b.Background(targetBg).
							Tooltip(tip).
							Children(func() {
								ui.Row(c).Size(22, 22).Shrink(0).Radius(RadiusSM).Background(iconBg).
									AlignItems(ui.Center).Justify(ui.Center).Children(func() {
									ui.Icon(c, icon).Size(12.5, 12.5).TextColor(iconFg)
								})
								ui.Column(c).Grow(1).MinWidth(0).Gap(0).Children(func() {
									el := ui.Text(c, wt.ShortName()).FontSize(FontSmall).TextColor(nameCol).SingleLine().Ellipsis("...")
									if isSelected {
										el.FontWeight(WeightMedium)
									}
									ui.Text(c, wt.DisplaySub()).Font(FontMono).FontSize(10).TextColor(p.TextMuted).SingleLine().Ellipsis("...")
								})
								if wt.IsRemote {
									ui.Row(c).Padding(1, 5).Shrink(0).Radius(RadiusSM).Background(p.SSHSoft).Gap(3).AlignItems(ui.Center).Children(func() {
										if isSelected {
											ui.Box(c).Size(4.5, 4.5).Radius(RadiusPill).Background(p.SSHColor)
										}
										ui.Text(c, "SSH").Font(FontMono).FontWeight(WeightMedium).FontSize(9.5).TextColor(p.SSHColor)
									})
								} else if isSelected {
									ui.Box(c).Size(5.5, 5.5).Shrink(0).Radius(RadiusPill).Background(p.LocalColor)
								}
							})
						if b.Clicked() {
							a.cfg.ActiveTargetID = wt.ID
							a.mgr.SwitchTarget(wt)
							if a.saveConfigOrWarn(c) {
								c.Toast("已切换执行环境至: " + wt.ShortName())
							}
						}
					}
				})
			})
		})

		// Section 2: Unified Session History & Visual Branch Tree
		ui.Column(c).Grow(1).FillWidth().MinWidth(0).Padding(10, 8, 6, 8).Gap(8).Children(func() {
			branchCount := 1
			if a.treeSnap != nil && a.treeSnap.BranchCount > 0 {
				branchCount = a.treeSnap.BranchCount
			}

			// Labeled Segmented Switcher + Refresh Action
			ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Padding(0, 2).Children(func() {
				ui.Row(c).Grow(1).Padding(2).Radius(RadiusMD).
					Background(p.InputBg).Border(1, p.BorderSubtle).Gap(2).Children(func() {
					// Tab 0: Session History
					histActive := a.sidebarTab == 0
					hb := ui.ButtonBase(c).Grow(1).Height(24).Padding(0, 6).Radius(RadiusSM).Gap(5).
						AlignItems(ui.Center).Justify(ui.Center).Tooltip("历史对话列表")
					hBg := ui.Transparent
					hFg := p.TextMuted
					if histActive {
						hBg = p.BgElevated
						if p.IsLight {
							hBg = p.BgSurface
						}
						hFg = p.TextPrimary
					} else if hb.Hovered() {
						hFg = p.TextSecondary
					}
					hb.Background(hBg).Children(func() {
						ui.Icon(c, IconChat).Size(12, 12).TextColor(hFg)
						el := ui.Text(c, "对话").FontSize(FontCaption).TextColor(hFg).SingleLine()
						if histActive {
							el.FontWeight(WeightMedium)
						}
					})
					if hb.Clicked() {
						a.sidebarTab = 0
					}

					// Tab 1: Session Branch Tree
					treeActive := a.sidebarTab == 1
					treeLabel := "分支"
					if branchCount > 1 {
						treeLabel = fmt.Sprintf("分支 %d", branchCount)
					}
					tb := ui.ButtonBase(c).Grow(1).Height(24).Padding(0, 6).Radius(RadiusSM).Gap(5).
						AlignItems(ui.Center).Justify(ui.Center).
						Tooltip(fmt.Sprintf("会话分支树 (%d 分支)", branchCount))
					tBg := ui.Transparent
					tFg := p.TextMuted
					if treeActive {
						tBg = p.BgElevated
						if p.IsLight {
							tBg = p.BgSurface
						}
						tFg = p.TextPrimary
					} else if tb.Hovered() {
						tFg = p.TextSecondary
					}
					tb.Background(tBg).Children(func() {
						ui.Icon(c, IconBranch).Size(12, 12).TextColor(tFg)
						el := ui.Text(c, treeLabel).FontFeatures("tnum").FontSize(FontCaption).TextColor(tFg).SingleLine()
						if treeActive {
							el.FontWeight(WeightMedium)
						}
					})
					if tb.Clicked() {
						a.sidebarTab = 1
					}
				})

				IconTipBtn(c, p, IconRefresh, 26, 13, "刷新会话列表与分支树", func() {
					a.refreshSessionsList()
					c.Toast("已刷新会话列表与分支树")
				})
			})

			if a.sidebarTab == 0 {
				a.renderSessionListPanel(c, p)
			} else {
				a.renderSessionTreePanel(c, p)
			}
		})

		// Section 3: Quiet Bottom Utility Bar (40px)
		ui.Row(c).FillWidth().Height(40).Padding(0, 10).Gap(4).AlignItems(ui.Center).
			BorderWidth(1, 0, 0, 0).BorderColor(p.BorderSubtle).Children(func() {
			ui.Text(c, "PiUI Desk").FontWeight(WeightMedium).FontSize(FontCaption).TextColor(p.TextMuted).Padding(0, 4)
			ui.Spacer(c)

			themeIcon := IconSun
			themeTip := "切换至浅色主题 (Ctrl/Cmd+Shift+L)"
			if p.IsLight {
				themeIcon = IconMoon
				themeTip = "切换至深色主题 (Ctrl/Cmd+Shift+L)"
			}
			IconTipBtn(c, p, themeIcon, 28, 14, themeTip, func() {
				a.toggleTheme(c)
			})
			IconTipBtn(c, p, IconSettings, 28, 14, "偏好设置 (Ctrl/Cmd+,)", func() {
				a.settingsModal.ActiveTab = 0
				a.settingsModal.Open = true
			})
		})
	})
}

func (a *DeskApp) renderSessionListPanel(c *ui.Context, p Palette) {
	ui.Scroll(c).Grow(1).FillWidth().Children(func() {
		ui.Column(c).FillWidth().Gap(2).Children(func() {
			if len(a.savedSessions) == 0 {
				ui.Column(c).FillWidth().Padding(16, 10).AlignItems(ui.Center).Gap(4).Children(func() {
					ui.Text(c, "暂无历史对话").FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextSecondary)
					ui.Text(c, "发送第一条指令后自动持久化为树状会话").
						FontSize(FontCaption).LineHeight(1.5).TextColor(p.TextMuted).TextAlign(ui.Center)
				})
				return
			}

			for _, info := range a.savedSessions {
				sessInfo := info
				isActive := a.activeSessID == sessInfo.ID

				b := ui.ButtonBase(c).FillWidth().MinWidth(0).Padding(7, 10).Radius(RadiusMD).Disabled(a.isRunning)
				targetBg := ui.Transparent
				titleCol := p.TextSecondary
				if isActive {
					targetBg = p.BgActive
					titleCol = p.TextPrimary
				} else if b.Hovered() && !a.isRunning {
					targetBg = p.BgHover
					titleCol = p.TextPrimary
				}

				metaLine := fmt.Sprintf("%s · %d 条消息", sessInfo.Timestamp, sessInfo.Messages)
				if sessInfo.Timestamp == "" {
					metaLine = fmt.Sprintf("#%s · %d 条消息", shortEntryID(sessInfo.ID), sessInfo.Messages)
				}

				b.Background(targetBg).Children(func() {
					ui.Column(c).FillWidth().MinWidth(0).Gap(2).Children(func() {
						el := ui.Text(c, sessInfo.Title).FontSize(FontSmall).TextColor(titleCol).SingleLine().Ellipsis("...")
						if isActive {
							el.FontWeight(WeightMedium)
						}
						ui.Text(c, metaLine).FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextMuted).SingleLine()
					})
				})
				if b.Clicked() {
					projected, snap, err := a.mgr.ResumeSavedSessionProjected(sessInfo.Path, a.cfg.ActiveProfile())
					if err != nil {
						a.lastError = err.Error()
					} else {
						a.toolCards = map[string]*ChatEntry{}
						a.currentAssistant = nil
						a.lastUserEntry = nil
						a.entries = RebuildEntriesFromProjected(projected, &a.nextEntryID)
						a.activeSessID = sessInfo.ID
						a.treeSnap = snap
						a.lastError = ""
						c.Toast("已加载历史会话: " + sessInfo.Title)
					}
				}
			}
		})
	})
}

func (a *DeskApp) renderSessionTreePanel(c *ui.Context, p Palette) {
	// Shares the exact same borderless sidebar container style as renderSessionListPanel!
	ui.Column(c).Grow(1).FillWidth().MinWidth(0).Gap(6).Children(func() {
		if a.treeSnap == nil || len(a.treeSnap.Nodes) == 0 {
			ui.Column(c).FillWidth().Padding(16, 10).AlignItems(ui.Center).Gap(4).Children(func() {
				ui.Text(c, "尚无会话分支节点").FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextSecondary)
				ui.Text(c, "对话产生后将在此实时展示拓扑树，点击任意节点即可回溯或分叉重试。").
					FontSize(FontCaption).LineHeight(1.5).TextColor(p.TextMuted).TextAlign(ui.Center)
			})
			return
		}

		ui.Row(c).FillWidth().Padding(0, 6).AlignItems(ui.Center).Gap(5).Children(func() {
			ui.Text(c, fmt.Sprintf("%d 条分支 · 共 %d 个节点", a.treeSnap.BranchCount, len(a.treeSnap.Nodes))).
				FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextMuted)
		})

		ui.Scroll(c).Grow(1).FillWidth().Children(func() {
			a.renderTreeNodesList(c, p, a.treeSnap.Nodes)
		})
	})
}

func treeEffectiveDepth(d int) int {
	return min(max(d, 0), 5)
}

// treeColumnContinuesBelow reports whether DFS tree column `col` needs a vertical trunk segment
// extending below the midpoint of `nodes[idx]` to connect a subsequent descendant or fork sibling.
func treeColumnContinuesBelow(nodes []harness.SessionTreeNode, idx, col int) bool {
	curDepth := treeEffectiveDepth(nodes[idx].Depth)
	for j := idx + 1; j < len(nodes); j++ {
		dj := treeEffectiveDepth(nodes[j].Depth)
		if dj < col {
			return false
		}
		if dj == col {
			return nodes[j].BranchIndex == 0
		}
		if dj == col+1 && (nodes[j].BranchIndex > 0 || (j == idx+1 && col == curDepth)) {
			return true
		}
	}
	return false
}

func (a *DeskApp) renderTreeNodesList(c *ui.Context, p Palette, nodes []harness.SessionTreeNode) {
	ui.Column(c).FillWidth().MinWidth(0).Gap(1).Children(func() {
		for idx, node := range nodes {
			n := node
			effectiveDepth := treeEffectiveDepth(n.Depth)
			gutterW := float32(effectiveDepth*14 + 16)

			nb := ui.ButtonBase(c).FillWidth().MinWidth(0).Height(30).Padding(0, 8, 0, 4).Radius(RadiusSM).
				Gap(6).AlignItems(ui.Center).Disabled(a.isRunning)

			targetBg := ui.Transparent
			textCol := p.TextMuted
			dotCol := p.BorderStrong
			lineCol := p.BorderStrong
			if n.IsActiveCursor {
				targetBg = p.AccentSoft
				textCol = p.TextPrimary
				dotCol = p.Accent
				lineCol = p.Accent.Alpha(0.55)
			} else if n.OnActiveBranch {
				textCol = p.TextSecondary
				dotCol = p.LocalColor
				lineCol = p.BorderStrong
			}
			if nb.Hovered() && !a.isRunning && !n.IsActiveCursor {
				targetBg = p.BgHover
				textCol = p.TextPrimary
			}

			rolePrefix := "AI"
			if n.Role == "user" {
				rolePrefix = "你"
			} else if n.Role == "compaction" {
				rolePrefix = "摘要"
			}

			rowIdx := idx
			nb.Background(targetBg).Children(func() {
				// Custom Vector Tree Connector Gutter + Node Dot (exact DFS elbow & trunk topology)
				ui.Box(c).Size(gutterW, 30).Shrink(0).Draw(func(pt *ui.Painter, r ui.Rect) {
					guideColor := p.BorderStrong
					cx := r.X + float32(effectiveDepth*14) + 7
					cy := r.Y + r.H/2

					// Determine whether current node connects vertically from curDepth or via an elbow from parentCol
					elbowFromCol := -1
					if rowIdx > 0 {
						prevDepth := treeEffectiveDepth(nodes[rowIdx-1].Depth)
						if effectiveDepth > prevDepth {
							elbowFromCol = prevDepth
						} else if n.BranchIndex > 0 && effectiveDepth > 0 {
							elbowFromCol = effectiveDepth - 1
						}
					}

					// Ancestor columns 0 .. effectiveDepth-1
					for d := 0; d < effectiveDepth; d++ {
						gx := r.X + float32(d*14) + 7
						contBelow := treeColumnContinuesBelow(nodes, rowIdx, d)
						if d == elbowFromCol {
							pt.Line(gx, r.Y, gx, cy, 1, guideColor)
							if contBelow {
								pt.Line(gx, cy, gx, r.Y+r.H, 1, guideColor)
							}
							pt.Line(gx, cy, cx, cy, 1, lineCol)
						} else if contBelow {
							pt.Line(gx, r.Y, gx, r.Y+r.H, 1, guideColor)
						}
					}

					// Current node column vertical segments
					if rowIdx > 0 && elbowFromCol == -1 {
						pt.Line(cx, r.Y, cx, cy, 1, lineCol)
					}
					if !n.IsLeaf && treeColumnContinuesBelow(nodes, rowIdx, effectiveDepth) {
						pt.Line(cx, cy, cx, r.Y+r.H, 1, lineCol)
					}

					// Node circle
					dotR := float32(3.0)
					if n.IsActiveCursor {
						dotR = 3.5
					}
					pt.Fill(ui.Rect{X: cx - dotR, Y: cy - dotR, W: dotR * 2, H: dotR * 2}, dotCol, dotR)
				})

				ui.Text(c, rolePrefix).FontSize(10.5).TextColor(p.TextMuted).Shrink(0)

				el := ui.Text(c, n.Label).Grow(1).MinWidth(0).FontSize(FontSmall).TextColor(textCol).SingleLine().Ellipsis("...")
				if n.IsActiveCursor || n.OnActiveBranch {
					el.FontWeight(WeightMedium)
				}

				if n.IsForkPoint {
					ui.Row(c).Padding(1, 5).Shrink(0).Radius(4).Background(p.WarnSoft).Children(func() {
						ui.Text(c, "分叉").FontWeight(WeightMedium).FontSize(10).TextColor(p.WarnColor)
					})
				}
			})

			if nb.Clicked() {
				projected, snap, err := a.mgr.CheckoutBranch(n.EntryID, a.cfg.ActiveProfile())
				if err != nil {
					a.lastError = err.Error()
				} else {
					a.toolCards = map[string]*ChatEntry{}
					a.currentAssistant = nil
					a.lastUserEntry = nil
					a.entries = RebuildEntriesFromProjected(projected, &a.nextEntryID)
					a.treeSnap = snap
					a.lastError = ""
					c.Toast("已切换至会话树节点 #" + shortEntryID(n.EntryID))
				}
			}
		}
	})
}

func (a *DeskApp) switchChannel(c *ui.Context, profileID string) {
	a.cfg.ActiveProfileID = profileID
	prof := a.cfg.ActiveProfile()
	saved := a.saveConfigOrWarn(c)
	applied := a.applyProfileOrWarn(c, prof)
	if saved && applied && c != nil && prof != nil {
		c.Toast(fmt.Sprintf("已切换渠道至: %s (模型: %s)", prof.Name, prof.ModelID))
	}
}

func (a *DeskApp) switchModel(c *ui.Context, modelID string) {
	prof := a.cfg.ActiveProfile()
	if prof == nil {
		return
	}
	prof.ModelID = modelID
	prof.AddModel(modelID)
	saved := a.saveConfigOrWarn(c)
	applied := a.applyProfileOrWarn(c, prof)
	if saved && applied && c != nil {
		c.Toast(fmt.Sprintf("已切换模型至: %s (%s)", modelID, prof.Name))
	}
}

func (a *DeskApp) switchChannelAndModel(c *ui.Context, profileID, modelID string) {
	a.cfg.ActiveProfileID = profileID
	prof := a.cfg.ActiveProfile()
	if prof == nil {
		return
	}
	prof.ModelID = modelID
	prof.AddModel(modelID)
	saved := a.saveConfigOrWarn(c)
	applied := a.applyProfileOrWarn(c, prof)
	if saved && applied && c != nil {
		c.Toast(fmt.Sprintf("已切换至渠道 [%s] · 模型 [%s]", prof.Name, modelID))
	}
}

func (a *DeskApp) switchThinkingLevel(c *ui.Context, level string) {
	prof := a.cfg.ActiveProfile()
	if prof == nil {
		return
	}
	prof.ThinkingLevel = level
	saved := a.saveConfigOrWarn(c)
	applied := a.applyProfileOrWarn(c, prof)
	if saved && applied && c != nil {
		c.Toast("已切换思考深度至: " + level)
	}
}

func (a *DeskApp) switchChannelModelThinking(c *ui.Context, profileID, modelID, thinkingLevel string) {
	a.cfg.ActiveProfileID = profileID
	prof := a.cfg.ActiveProfile()
	if prof == nil {
		return
	}
	if strings.TrimSpace(modelID) != "" {
		prof.ModelID = modelID
		prof.AddModel(modelID)
	}
	if strings.TrimSpace(thinkingLevel) != "" {
		prof.ThinkingLevel = thinkingLevel
	}
	saved := a.saveConfigOrWarn(c)
	applied := a.applyProfileOrWarn(c, prof)
	if saved && applied && c != nil {
		c.Toast(fmt.Sprintf("已切换至 %s · %s (Think: %s)", prof.Name, prof.ModelID, capitalizeWord(prof.ThinkingLevel)))
	}
}

// renderCascadingModelMenu builds a single unified 3-level cascading menu:
// 供应商渠道 (Provider) ▸ 模型 (Model) ▸ 思考深度 (Thinking Level).
func (a *DeskApp) renderCascadingModelMenu(c *ui.Context, m *ui.Menu) {
	activeProf := a.cfg.ActiveProfile()

	for i := range a.cfg.Profiles {
		p := a.cfg.Profiles[i]
		isCurrentChannel := activeProf != nil && p.ID == activeProf.ID
		channelTitle := p.Name
		if isCurrentChannel {
			channelTitle = "● " + p.Name
		}

		models := p.AvailableModels()
		m.Submenu(channelTitle, func(mModel *ui.Menu) {
			if len(models) == 0 {
				if mModel.Item("拉取或添加该渠道模型...").Chosen() {
					a.cfg.ActiveProfileID = p.ID
					a.settingsModal.ActiveTab = 0
					a.settingsModal.Open = true
				}
				return
			}

			for _, mdl := range models {
				modelID := mdl
				isCurrentModel := isCurrentChannel && activeProf.ModelID == modelID
				modelTitle := modelID
				if isCurrentModel {
					modelTitle = "● " + modelID
				}

				pid := p.ID
				mModel.Submenu(modelTitle, func(mThink *ui.Menu) {
					for idx, val := range thinkingValues {
						lvl := val
						lbl := fmt.Sprintf("Think: %s (%s)", thinkingLabels[idx], val)
						checked := isCurrentModel && strings.EqualFold(activeProf.ThinkingLevel, lvl)
						if mThink.Item(lbl).Radio(checked).Chosen() {
							a.switchChannelModelThinking(c, pid, modelID, lvl)
						}
					}
				})
			}
		})
	}

	if activeProf != nil {
		m.Separator()
		m.Submenu("当前思考深度: "+capitalizeWord(activeProf.ThinkingLevel), func(mThink *ui.Menu) {
			for idx, val := range thinkingValues {
				lvl := val
				lbl := fmt.Sprintf("%s (%s)", thinkingLabels[idx], val)
				if mThink.Item(lbl).Radio(strings.EqualFold(activeProf.ThinkingLevel, lvl)).Chosen() {
					a.switchThinkingLevel(c, lvl)
				}
			}
		})
	}

	m.Separator()
	if m.Item("管理渠道与拉取模型...").Chosen() {
		a.settingsModal.ActiveTab = 0
		a.settingsModal.Open = true
	}
}

func (a *DeskApp) renderCenterMain(c *ui.Context, p Palette) {
	target := a.mgr.Router().Target()
	prof := a.cfg.ActiveProfile()
	bar := c.TitleBar()

	leftPad := float32(16)
	if a.sidebarCollapsed {
		leftPad = max(bar.Left, 12)
	}
	rightPad := float32(14)
	if !a.termDrawer.Open {
		rightPad = max(bar.Right, 14)
	}

	ui.Column(c).Direction(ui.LeftToRight).Grow(1).MinWidth(0).FillHeight().Clip().Background(p.BgCanvas).Children(func() {
		// Unified 44px Top Status Bar — Calm, Balanced & Draggable
		ui.Row(c).FillWidth().MinWidth(0).Height(TopBarHeight).Padding(0, rightPad, 0, leftPad).Gap(8).
			AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).
			DragWindow().Children(func() {
			if a.sidebarCollapsed {
				IconTipBtn(c, p, IconSidebarLeft, 28, 14, "展开侧边栏 (Ctrl/Cmd+B)", func() {
					a.sidebarCollapsed = false
				})
				IconTipBtn(c, p, IconCompose, 28, 14, "新建对话 (Ctrl/Cmd+N)", func() {
					a.startNewSession(c)
				})
			}

			// Quiet Clickable Workspace Breadcrumb Pill (Neutral when Local, Purple when Remote SSH)
			wsBtn := ui.ButtonBase(c).MaxWidth(420).MinWidth(0).Height(26).Padding(0, 10).Radius(RadiusSM).
				Gap(6).AlignItems(ui.Center).
				Tooltip("点击管理工作区与远程 SSH 目标")
			if target.IsRemote {
				wsBg := p.SSHSoft
				if wsBtn.Hovered() {
					wsBg = p.SSHColor.Alpha(0.2)
				}
				wsBtn.Background(wsBg).Border(1, p.SSHColor.Alpha(0.28)).Children(func() {
					ui.Icon(c, IconServer).Size(12, 12).TextColor(p.SSHColor).Shrink(0)
					ui.Text(c, "SSH · "+target.DisplaySub()).
						Font(FontMono).FontWeight(WeightMedium).FontSize(FontCaption + 0.5).TextColor(p.SSHColor).
						MinWidth(0).SingleLine().Ellipsis("...")
				})
			} else {
				wsBg := p.BgSurface
				if wsBtn.Hovered() {
					wsBg = p.BgHover
				}
				wsBtn.Background(wsBg).Border(1, p.BorderSubtle).Children(func() {
					ui.Box(c).Size(5.5, 5.5).Shrink(0).Radius(RadiusPill).Background(p.LocalColor)
					ui.Text(c, target.ShortName()).FontWeight(WeightMedium).FontSize(FontSmall).TextColor(p.TextPrimary).Shrink(0).SingleLine()
					ui.Text(c, target.DisplaySub()).Font(FontMono).FontSize(FontCaption).TextColor(p.TextMuted).
						MinWidth(0).SingleLine().Ellipsis("...")
				})
			}
			if wsBtn.Clicked() {
				a.settingsModal.ActiveTab = 1
				a.settingsModal.Open = true
			}

			// Branch Count Badge (only shown when conversation has forked into multiple branches)
			if a.treeSnap != nil && a.treeSnap.BranchCount > 1 {
				b := ui.ButtonBase(c).Height(26).Padding(0, 8).Radius(RadiusSM).
					Border(1, p.BorderSubtle).Gap(4).AlignItems(ui.Center).
					Tooltip(fmt.Sprintf("当前会话共 %d 个分支，点击查看分支树", a.treeSnap.BranchCount))
				bBg := ui.Transparent
				if b.Hovered() || b.Pressed() {
					bBg = p.BgHover
				}
				b.Background(bBg).Children(func() {
					ui.Icon(c, IconBranch).Size(12, 12).TextColor(p.Accent)
					ui.Text(c, fmt.Sprintf("%d", a.treeSnap.BranchCount)).
						FontFeatures("tnum").FontWeight(WeightMedium).FontSize(FontCaption).TextColor(p.TextSecondary)
				})
				if b.Clicked() {
					a.sidebarCollapsed = false
					a.sidebarTab = 1
				}
			}

			// Context Token Estimate (when conversation has history)
			if tokens := a.mgr.EstimateContextTokens(); tokens > 0 {
				ui.Text(c, fmt.Sprintf("~%s tokens", formatTokenCount(tokens))).
					FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextMuted).Shrink(0)
			}

			if a.activeToolStatus != "" {
				ui.Row(c).MaxWidth(260).MinWidth(0).Padding(2, 8).Radius(RadiusPill).
					Background(p.AccentSoft).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.Spinner(c).Size(11, 11)
					ui.Text(c, a.activeToolStatus).Font(FontMono).FontSize(FontCaption).TextColor(p.Accent).
						MinWidth(0).SingleLine().Ellipsis("...")
				})
			}

			ui.Spacer(c)

			// Safety Policy Icon Button (quiet neutral tint in default mode, semantic tint for overrides)
			safetyLabel := "远程高危拦截"
			safetyIcon := IconShield
			safetyTint := p.TextSecondary
			switch a.cfg.SafetyPolicy {
			case store.SafetyPolicyAlwaysHighRisk:
				safetyLabel = "全局高危拦截"
				safetyIcon = IconShieldCheck
				safetyTint = p.Accent
			case store.SafetyPolicyStrictRemote:
				safetyLabel = "远程严格审批"
				safetyIcon = IconShieldAlert
				safetyTint = p.SSHColor
			case store.SafetyPolicyOff:
				safetyLabel = "拦截已关闭"
				safetyIcon = IconShieldOff
				safetyTint = p.WarnColor
			}
			ToolbarIconBtn(c, p, safetyIcon, 28, 14.5, false, safetyTint, "安全拦截策略: "+safetyLabel, func() {
				a.settingsModal.ActiveTab = 2
				a.settingsModal.Open = true
			})

			// Right Terminal Drawer Toggle Icon Button (with unread dot indicator)
			termTip := "展开实时命令输出流 (Ctrl/Cmd+J)"
			if a.termDrawer.Open {
				termTip = "收起实时命令输出流 (Ctrl/Cmd+J)"
			}
			termBtn := ToolbarIconBtn(c, p, IconTerminal, 28, 14.5, a.termDrawer.Open, ui.Transparent, termTip, func() {
				a.termDrawer.Open = !a.termDrawer.Open
			})
			if a.termDrawer.Unread && !a.termDrawer.Open {
				termBtn.DrawOver(func(pt *ui.Painter, r ui.Rect) {
					pt.Fill(ui.Rect{X: r.X + r.W - 7, Y: r.Y + 4, W: 5, H: 5}, p.Accent, 99)
				})
			}
		})

		// Error alert bar for session/system-level notices
		if a.lastError != "" {
			ui.Row(c).FillWidth().MinWidth(0).Padding(7, 16).Gap(8).AlignItems(ui.Center).
				Background(p.DangerSoft).BorderWidth(0, 0, 1, 0).BorderColor(p.DangerColor.Alpha(0.35)).Children(func() {
				ui.Icon(c, IconShieldAlert).Size(14, 14).TextColor(p.DangerColor).Shrink(0)
				ui.Text(c, FormatErrorHeadline(a.lastError)).FontSize(FontSmall).TextColor(p.DangerColor).
					Grow(1).MinWidth(0).SingleLine().Ellipsis("...").Selectable()
				IconTipBtn(c, p, IconCopy, 24, 13, "复制报错信息", func() {
					c.WriteClipboard(a.lastError)
					c.Toast("已复制报错信息到剪贴板")
				})
				IconTipBtn(c, p, IconSettings, 24, 13.5, "配置渠道与模型", func() {
					a.settingsModal.ActiveTab = 0
					a.settingsModal.Open = true
				})
				IconTipBtn(c, p, IconCross, 24, 13.5, "关闭提示", func() {
					a.lastError = ""
				})
			})
		}

		// Central Markdown Conversation Stream (.chat-scroll)
		a.chatList.Key = func(i int) any {
			if i < len(a.entries) {
				return a.entries[i].ID
			}
			return -1
		}
		a.chatList.FollowEnd = true
		a.chatList.FollowAnimation = &ui.Smooth

		canFork := !a.isRunning
		ui.List(c, &a.chatList, len(a.entries), func(i int) {
			var prevKind EntryKind
			if i > 0 {
				prevKind = a.entries[i-1].Kind
			}
			topGap := EntryTopSpacing(prevKind, a.entries[i].Kind, i == 0)
			showTurnFooter := i+1 >= len(a.entries) || a.entries[i+1].Kind != EntryTool

			ui.Column(c).FillWidth().MinWidth(0).Margin(topGap, 0, 0, 0).AlignItems(ui.Center).Children(func() {
				ui.Column(c).FillWidth().MaxWidth(760).MinWidth(0).Children(func() {
					RenderChatTurnEntry(c, p, a.entries[i], &a.mdOpts, canFork, showTurnFooter, func(clicked *ChatEntry) {
						a.forkFromChatEntry(c, clicked)
					})
				})
			})
		}).Grow(1).MinWidth(0).Padding(22, 24, 12, 24).Gap(0).Children(func() {
			if len(a.entries) == 0 {
				a.renderWelcomeEmptyState(c, p)
			}
		})

		// Bottom Borderless Unified Floating Composer (.composer-card)
		ui.Column(c).FillWidth().MinWidth(0).Padding(4, 24, 16, 24).AlignItems(ui.Center).Children(func() {
			card := ui.Column(c).FillWidth().MaxWidth(760).MinWidth(0).
				Padding(10, 12, 8, 14).Radius(RadiusLG).
				Background(p.BgSurface).Gap(8)

			focused := card.FocusWithin()
			borderCol := p.BorderStrong
			shadowBlur := float32(20)
			if focused {
				borderCol = p.BorderFocus
				shadowBlur = 28
			}

			card.Border(1, borderCol).
				Shadow(0, 6, shadowBlur, -4, p.ShadowSoft).
				Children(func() {
					placeholder := "输入任务指令，Enter 发送，Shift+Enter 换行..."
					if a.isRunning {
						placeholder = "Agent 运行中 — 输入纠偏指令按 Enter 将立即触发 Steer 插队引导..."
					}

					// Borderless TextAreaBase eliminates nested box & 80px empty height bloat!
					ui.TextAreaBase(c, &a.input).
						Lines(1, 7).
						FillWidth().
						Padding(2, 2, 4, 0).
						FontSize(FontBody).
						LineHeight(1.5).
						TextColor(p.TextPrimary).
						Placeholder(placeholder).
						PlaceholderColor(p.TextMuted).
						FocusRing(false).
						AutoFocus().
						OnTextKey(func(k ui.TextKey) bool {
							if k.Key != ui.KeyEnter || k.Mods != 0 {
								return false
							}
							txt := strings.TrimSpace(k.Text())
							if txt == "" {
								return true
							}
							k.SetText("")
							a.submitPrompt(txt)
							return true
						})

					// Bottom Composer Toolbar — Quiet Unified Cascader Pill + Send/Steer
					ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
						if prof != nil {
							a.renderUnifiedCascaderPill(c, p, prof)
						}

						ui.Spacer(c)

						hasInput := strings.TrimSpace(a.input) != ""
						if hasInput {
							hint := "Enter 发送"
							if a.isRunning {
								hint = "Enter 插队纠偏"
							}
							ui.Text(c, hint).FontSize(FontCaption).TextColor(p.TextMuted).Padding(0, 2)
						}

						// Stop Icon Button (when running)
						if a.isRunning {
							stopBtn := ui.ButtonBase(c).Size(28, 28).Radius(RadiusPill).
								Border(1, p.DangerColor.Alpha(0.45)).AlignItems(ui.Center).Justify(ui.Center).
								Tooltip("停止当前运行").Label("停止当前运行")
							stopBg := p.DangerSoft
							if stopBtn.Hovered() || stopBtn.Pressed() {
								stopBg = p.DangerColor.Alpha(0.24)
							}
							stopBtn.Background(stopBg).Children(func() {
								ui.Icon(c, IconStop).Size(11, 11).TextColor(p.DangerColor)
							})
							if stopBtn.Clicked() {
								a.mgr.Abort()
								if a.cancelRun != nil {
									a.cancelRun()
								}
								a.approvalModal.Clear()
							}
						}

						// Circular Send / Steer Icon Button
						sendTip := "发送指令 (Enter)"
						if a.isRunning {
							sendTip = "Steer 插队发送纠偏指令 (Enter)"
						}
						sendBtn := ui.ButtonBase(c).Size(28, 28).Radius(RadiusPill).
							AlignItems(ui.Center).Justify(ui.Center).Disabled(!hasInput).
							Tooltip(sendTip).Label(sendTip)

						btnBg := p.BtnPrimaryBg
						btnFg := p.BtnPrimaryFg
						btnIcon := IconArrowUp
						iconSz := float32(14)
						if a.isRunning {
							btnBg = p.WarnColor
							btnFg = ui.Hex("#09090B")
							btnIcon = IconBolt
							iconSz = 13.5
						}
						if !hasInput {
							btnBg = p.BgElevated
							btnFg = p.TextMuted
						} else if sendBtn.Hovered() {
							btnBg = btnBg.Alpha(0.88)
						}

						sendBtn.Background(btnBg).Children(func() {
							ui.Icon(c, btnIcon).Size(iconSz, iconSz).TextColor(btnFg)
						})
						if sendBtn.Clicked() {
							a.submitPrompt(a.input)
						}
					})
				})
		})
	})
}

func (a *DeskApp) renderWelcomeEmptyState(c *ui.Context, p Palette) {
	ui.Column(c).Margin(ui.Auto).AlignItems(ui.Center).Gap(12).MaxWidth(540).Padding(24).Children(func() {
		// Subtle Elevated Brand Mark
		ui.Row(c).Size(44, 44).Radius(RadiusLG).
			Background(p.BgSurface).Border(1, p.BorderStrong).
			Shadow(0, 6, 20, -4, p.ShadowSoft).
			AlignItems(ui.Center).Justify(ui.Center).Children(func() {
			ui.Icon(c, IconSparkles).Size(20, 20).TextColor(p.Accent)
		})

		ui.Column(c).AlignItems(ui.Center).Gap(6).Children(func() {
			ui.Text(c, "大脑在本地，手脚可插拔").
				FontWeight(WeightSemibold).FontSize(FontHero).TextColor(p.TextPrimary)
			ui.Text(c, "同一会话支持随时热切换本地项目与远程 SSH 服务器，内置树状会话分叉回溯与高危命令拦截审批。").
				FontSize(FontBody).LineHeight(1.6).TextColor(p.TextMuted).TextAlign(ui.Center)
		})

		// Interactive Quick-Start Action Pills
		ui.Row(c).Gap(8).Wrap().Justify(ui.Center).Padding(6, 0, 0, 0).Children(func() {
			renderQuickActionChip(c, p, IconCompass, "扫描当前项目架构", func() {
				a.input = "请先扫描当前工作区的目录结构与关键模块，给出一份简明的架构概览。"
			})
			renderQuickActionChip(c, p, IconServer, "工作区与 SSH 目标", func() {
				a.settingsModal.ActiveTab = 1
				a.settingsModal.Open = true
			})
			renderQuickActionChip(c, p, IconSettings, "配置模型渠道", func() {
				a.settingsModal.ActiveTab = 0
				a.settingsModal.Open = true
			})
		})
	})
}

func renderQuickActionChip(c *ui.Context, p Palette, icon *ui.SVG, label string, onClick func()) {
	b := ui.ButtonBase(c).Height(30).Padding(0, 12).Radius(RadiusPill).
		Border(1, p.BorderSubtle).Gap(6).AlignItems(ui.Center)
	targetBg := p.BgSurface
	fg := p.TextSecondary
	if b.Hovered() || b.Pressed() {
		targetBg = p.BgHover
		fg = p.TextPrimary
	}
	b.Background(targetBg).Children(func() {
		ui.Icon(c, icon).Size(13, 13).TextColor(p.TextMuted)
		ui.Text(c, label).FontSize(FontSmall).FontWeight(WeightMedium).TextColor(fg).SingleLine()
	})
	if b.Clicked() && onClick != nil {
		onClick()
	}
}

// renderUnifiedCascaderPill renders the quiet cascading selector pill for Provider ▸ Model ▸ Thinking Level.
func (a *DeskApp) renderUnifiedCascaderPill(c *ui.Context, p Palette, prof *store.ProviderProfile) {
	b := ui.ButtonBase(c).Height(26).Padding(0, 10).Radius(RadiusPill).
		Border(1, p.BorderSubtle).Gap(5).AlignItems(ui.Center)
	targetBg := p.InputBg
	if b.Hovered() || b.Pressed() {
		targetBg = p.BgHover
	}

	modelDisplay := strings.TrimSpace(prof.ModelID)
	if modelDisplay == "" {
		modelDisplay = "选择模型"
	}
	thinkDisplay := capitalizeWord(prof.ThinkingLevel)

	b.Background(targetBg).Children(func() {
		ui.Text(c, prof.Name).FontSize(FontCaption + 0.5).TextColor(p.TextMuted).SingleLine()
		ui.Text(c, "/").FontSize(FontCaption).TextColor(p.TextMuted)
		ui.Text(c, modelDisplay).Font(FontMono).FontWeight(WeightMedium).FontSize(FontCaption + 0.5).TextColor(p.TextPrimary).SingleLine()
		if !strings.EqualFold(prof.ThinkingLevel, "off") {
			ui.Text(c, "·").FontSize(FontCaption).TextColor(p.TextMuted)
			thinkCol := p.TextSecondary
			if strings.EqualFold(prof.ThinkingLevel, "high") {
				thinkCol = p.Accent
			}
			ui.Text(c, thinkDisplay).FontSize(FontCaption).FontWeight(WeightMedium).TextColor(thinkCol).SingleLine()
		}
		ui.Icon(c, IconChevronDown).Size(11, 11).TextColor(p.TextMuted)
	}).PopoverMenuButton(func(m *ui.Menu) {
		a.renderCascadingModelMenu(c, m)
	})
}

func capitalizeWord(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Medium"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func formatTokenCount(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000.0)
	}
	return fmt.Sprintf("%d", n)
}
