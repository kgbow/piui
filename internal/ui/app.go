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

	// Composer & run state
	input            string
	isRunning        bool
	activeToolStatus string
	lastError        string
	cancelRun        context.CancelFunc

	// Sidebars, Session Branch Tree & Modals
	sidebarTab    int // 0 = 会话历史 (.jsonl), 1 = 分支树 (Session Tree)
	savedSessions []harness.SavedSessionSummary
	activeSessID  string
	treeSnap      *harness.SessionTreeSnapshot
	termDrawer    *TermDrawer
	settingsModal SettingsModal
	approvalModal ApprovalModal
}

// NewDeskApp initializes the application state, config, and sky-valley/pi harness.
func NewDeskApp(workDir string) (*DeskApp, error) {
	cfg := store.LoadOrInit(workDir)
	app := &DeskApp{
		cfg:        cfg,
		toolCards:  map[string]*ChatEntry{},
		termDrawer: NewTermDrawer(),
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

func (a *DeskApp) currentToolCardBadge() (string, bool) {
	t := a.mgr.Router().Target()
	return t.ToolCardBadge(), t.IsRemote
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
	if err := a.mgr.InitOrResetSession(a.cfg.ActiveProfile()); err == nil {
		a.entries = nil
		a.toolCards = map[string]*ChatEntry{}
		a.currentAssistant = nil
		a.lastUserEntry = nil
		a.activeSessID = ""
		a.treeSnap = nil
		a.lastError = ""
		a.refreshSessionsList()
		if c != nil {
			c.Toast("已开启新会话")
		}
	}
}

func (a *DeskApp) toggleTheme(c *ui.Context) {
	next := a.cfg.ToggleTheme()
	_ = a.cfg.Save()
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
			badge, isRem := a.currentToolCardBadge()
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
			a.activeToolStatus = fmt.Sprintf("[%s] %s", ev.ToolName, singleLinePreview(sum, 36))

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
			if err != nil {
				a.lastError = err.Error()
			} else if res != nil && res.ErrorMessage != "" {
				a.lastError = res.ErrorMessage
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
		// Rewind to right before this user prompt and pre-fill the composer with its text so the user can edit & resend.
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
		a.lastError = ""
		c.Toast("已回退至该提问前并回填输入框，修改后发送将创建新分支")
		return
	}

	// Branch right after this assistant response.
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
	a.lastError = ""
	c.Toast("已定位至分支节点 #" + shortEntryID(entry.EntryID) + "，继续发送消息将分叉出新分支")
}

// View builds the declarative Antigravity GPU interface every frame.
func (a *DeskApp) View(c *ui.Context) {
	isLight := a.cfg.IsLightTheme()
	p := PaletteFor(isLight)
	c.SetTheme(BuildMyGoTheme(isLight))

	// Keyboard shortcuts
	c.OnWindowShortcut(ui.Cmd, ui.Key(','), func() {
		a.settingsModal.Open = true
	})
	c.OnWindowShortcut(ui.Cmd, ui.Key('n'), func() {
		a.startNewSession(c)
	})
	c.OnWindowShortcut(ui.Cmd, ui.Key('j'), func() {
		a.termDrawer.Open = !a.termDrawer.Open
	})
	c.OnWindowShortcut(ui.Cmd|ui.Shift, ui.Key('l'), func() {
		a.toggleTheme(c)
	})

	ui.Row(c).Fill().Background(p.BgCanvas).Children(func() {
		// 1. Left Sidebar (265px): Environment Target Hot-Switcher + Session History / Branch Tree + Footer
		a.renderLeftSidebar(c, p)

		// 2. Center Main Stage: 44px Top Bar + Centered Conversation Stream + Floating Pill Composer
		a.renderCenterMain(c, p)

		// 3. Right Drawer (360px): Real-time Bash Command & Stdout/Stderr Output Stream
		a.termDrawer.View(c, p)
	})

	// Settings Modal (API Endpoints, Workspaces & Safety Policy)
	a.settingsModal.Render(c, p, a.cfg, a.mgr, a.updateUI, func() {
		_ = a.mgr.ApplyProfile(a.cfg.ActiveProfile())
		a.mgr.SetSafetyPolicy(a.cfg.SafetyPolicy)
		if target := a.cfg.ActiveTarget(); target != nil {
			a.mgr.SwitchTarget(*target)
		}
	})

	// Human-in-the-Loop High-Risk Tool Approval Modal
	if a.mgr != nil {
		a.approvalModal.Render(c, p, a.mgr.SafetyGate())
	}
}

func (a *DeskApp) renderLeftSidebar(c *ui.Context, p Palette) {
	activeTarget := a.mgr.Router().Target()

	ui.Column(c).Width(265).FillHeight().Background(p.BgSidebar).
		BorderWidth(0, 1, 0, 0).BorderColor(p.BorderSubtle).Children(func() {
		// Section 1: Workspace Target Hot-Switcher (.sb-section)
		ui.Column(c).FillWidth().Padding(10, 8, 8, 8).Gap(4).
			BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).Children(func() {
			ui.Row(c).FillWidth().Padding(0, 4, 2, 4).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "执行环境 (同会话热切换)").Bold().FontSize(11).TextColor(p.TextMuted)
				ui.Spacer(c)
				IconBtn(c, p, IconPlus, 24, 14, func() {
					a.settingsModal.ActiveTab = 1
					a.settingsModal.Open = true
				})
			})

			ui.Scroll(c).MaxHeight(165).FillWidth().Children(func() {
				ui.Column(c).FillWidth().Gap(3).Children(func() {
					for i := range a.cfg.Targets {
						wt := a.cfg.Targets[i]
						isSelected := wt.ID == activeTarget.ID

						icon := IconFolder
						iconBg := p.LocalSoft
						iconFg := p.LocalColor
						pillText := "Local"
						if wt.IsRemote {
							icon = IconServer
							iconBg = p.SSHSoft
							iconFg = p.SSHColor
							pillText = "SSH"
						}
						if isSelected {
							pillText = "当前挂载"
						}

						b := ui.ButtonBase(c).FillWidth().Padding(7, 9).Radius(8).
							Gap(9).AlignItems(ui.Center)
						rowBg := ui.Transparent
						borderCol := ui.Transparent
						nameCol := p.TextSecondary
						if isSelected {
							rowBg = p.BgActive
							borderCol = p.BorderStrong
							nameCol = p.TextPrimary
						} else if b.Hovered() {
							rowBg = p.BgHover
							nameCol = p.TextPrimary
						}

						b.Background(rowBg).Border(1, borderCol).Children(func() {
							ui.Row(c).Size(24, 24).Radius(6).Background(iconBg).
								AlignItems(ui.Center).Justify(ui.Center).Children(func() {
								ui.Icon(c, icon).Size(13, 13).TextColor(iconFg)
							})
							ui.Column(c).Grow(1).Gap(1).Children(func() {
								ui.Text(c, wt.ShortName()).Bold().FontSize(12.5).TextColor(nameCol).SingleLine().Ellipsis("...")
								ui.Text(c, wt.DisplaySub()).Font("Menlo").FontSize(10.5).TextColor(p.TextMuted).SingleLine().Ellipsis("...")
							})
							ui.Row(c).Padding(1, 6).Radius(99).Background(iconBg).Children(func() {
								ui.Text(c, pillText).Bold().Font("Menlo").FontSize(10).TextColor(iconFg)
							})
						})
						if b.Clicked() {
							a.cfg.ActiveTargetID = wt.ID
							_ = a.cfg.Save()
							a.mgr.SwitchTarget(wt)
							c.Toast("已热切换执行环境至: " + wt.ShortName())
						}
					}
				})
			})
		})

		// Section 2: Middle Session History & Branch Tree (.sb-middle)
		ui.Column(c).Grow(1).FillWidth().Padding(10, 8, 6, 8).Gap(6).Children(func() {
			// New Session Button (.new-sess-btn)
			nsBtn := ui.ButtonBase(c).FillWidth().Height(33).Radius(8).
				Border(1, p.BorderStrong).Gap(7).
				AlignItems(ui.Center).Justify(ui.Center).Disabled(a.isRunning)
			nsBg := p.BgSurface
			if nsBtn.Hovered() && !a.isRunning {
				nsBg = p.BgHover
			}
			nsBtn.Background(nsBg).Children(func() {
				ui.Icon(c, IconPlus).Size(14, 14).TextColor(p.TextPrimary)
				ui.Text(c, "新建对话 (New Session)").Bold().FontSize(12.5).TextColor(p.TextPrimary)
			})
			if nsBtn.Clicked() {
				a.startNewSession(c)
			}

			// 2-Tab Segmented Bar + Refresh Icon Button (.seg-bar)
			branchCount := 1
			if a.treeSnap != nil && a.treeSnap.BranchCount > 0 {
				branchCount = a.treeSnap.BranchCount
			}
			treeTabLabel := fmt.Sprintf("分支树 (%d)", branchCount)

			ui.Row(c).FillWidth().Gap(4).AlignItems(ui.Center).Children(func() {
				ui.Row(c).Grow(1).Padding(2, 2).Radius(7).
					Background(p.BgCanvas).Border(1, p.BorderSubtle).Gap(2).Children(func() {
					for idx, lbl := range []string{"会话历史", treeTabLabel} {
						tabIdx := idx
						tabLbl := lbl
						active := a.sidebarTab == tabIdx
						tb := ui.ButtonBase(c).Grow(1).Padding(4, 6).Radius(5).
							AlignItems(ui.Center).Justify(ui.Center)
						tBg := ui.Transparent
						tFg := p.TextMuted
						if active {
							tBg = p.BgElevated
							tFg = p.TextPrimary
						} else if tb.Hovered() {
							tFg = p.TextSecondary
						}
						tb.Background(tBg).Children(func() {
							el := ui.Text(c, tabLbl).FontSize(11.5).TextColor(tFg)
							if active {
								el.Bold()
							}
						})
						if tb.Clicked() {
							a.sidebarTab = tabIdx
						}
					}
				})

				IconBtn(c, p, IconRefresh, 26, 13, func() {
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

			// Section 3: Sleek Single-Row Bottom Footer (.sb-footer)
		ui.Row(c).FillWidth().Padding(8, 10).Gap(6).AlignItems(ui.Center).
			BorderWidth(1, 0, 0, 0).BorderColor(p.BorderSubtle).Children(func() {
			// Settings Button
			setBtn := ui.ButtonBase(c).Grow(1).Height(31).Padding(0, 10).Radius(7).
				Border(1, p.BorderSubtle).Gap(8).AlignItems(ui.Center)
			setBg := ui.Transparent
			setFg := p.TextSecondary
			if setBtn.Hovered() {
				setBg = p.BgHover
				setFg = p.TextPrimary
			}
			setBtn.Background(setBg).Children(func() {
				ui.Icon(c, IconSettings).Size(14, 14).TextColor(setFg)
				ui.Text(c, "设置与模型渠道").FontSize(12).TextColor(setFg)
			})
			if setBtn.Clicked() {
				a.settingsModal.ActiveTab = 0
				a.settingsModal.Open = true
			}

			// Compact Theme Toggle Icon Button (Dark Obsidian <-> Light Daylight)
			themeIcon := IconSun
			if p.IsLight {
				themeIcon = IconMoon
			}
			thBtn := ui.ButtonBase(c).Size(31, 31).Radius(7).
				Border(1, p.BorderSubtle).AlignItems(ui.Center).Justify(ui.Center)
			thBg := ui.Transparent
			thFg := p.TextSecondary
			if thBtn.Hovered() {
				thBg = p.BgHover
				thFg = p.TextPrimary
			}
			thBtn.Background(thBg).Children(func() {
				ui.Icon(c, themeIcon).Size(14, 14).TextColor(thFg)
			})
			if thBtn.Clicked() {
				a.toggleTheme(c)
			}
		})
	})
}

func (a *DeskApp) renderSessionListPanel(c *ui.Context, p Palette) {
	ui.Scroll(c).Grow(1).FillWidth().Children(func() {
		ui.Column(c).FillWidth().Gap(3).Children(func() {
			if len(a.savedSessions) == 0 {
				ui.Column(c).FillWidth().Padding(10, 8).Radius(8).
					Background(p.BgSurface).Border(1, p.BorderSubtle).Children(func() {
					ui.Text(c, "暂无历史会话，发送第一条消息后自动持久化为树状 JSONL。").
						FontSize(11).TextColor(p.TextMuted)
				})
			}

			for _, info := range a.savedSessions {
				sessInfo := info
				isActive := a.activeSessID == sessInfo.ID

				b := ui.ButtonBase(c).FillWidth().Padding(8, 10).Radius(8).Disabled(a.isRunning)
				bg := ui.Transparent
				borderCol := ui.Transparent
				titleCol := p.TextSecondary
				if isActive {
					bg = p.BgActive
					borderCol = p.BorderSubtle
					titleCol = p.TextPrimary
				} else if b.Hovered() && !a.isRunning {
					bg = p.BgHover
					titleCol = p.TextPrimary
				}

				metaLine := fmt.Sprintf("%s · %d 条消息", sessInfo.Timestamp, sessInfo.Messages)
				if sessInfo.Timestamp == "" {
					metaLine = fmt.Sprintf("%s · %d 条消息", shortEntryID(sessInfo.ID), sessInfo.Messages)
				}

				b.Background(bg).Border(1, borderCol).Children(func() {
					ui.Column(c).FillWidth().Gap(2).Children(func() {
						ui.Text(c, sessInfo.Title).Bold().FontSize(12.5).TextColor(titleCol).SingleLine().Ellipsis("...")
						ui.Text(c, metaLine).Font("Menlo").FontSize(10.5).TextColor(p.TextMuted).SingleLine()
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
	ui.Column(c).Grow(1).FillWidth().Padding(8, 9).Radius(8).
		Background(p.BgSurface).Border(1, p.BorderSubtle).Gap(6).Children(func() {
		if a.treeSnap == nil || len(a.treeSnap.Nodes) == 0 {
			ui.Text(c, "当前会话尚无分支节点。发送消息后将在此实时生成可视化 Session 分支树，点击任意历史节点即可回溯或分叉重试。").
				FontSize(11).TextColor(p.TextMuted).Padding(4, 2)
			return
		}

		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(5).
			Padding(0, 0, 4, 0).BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).Children(func() {
			ui.Icon(c, IconBranch).Size(12, 12).TextColor(p.Accent)
			ui.Text(c, fmt.Sprintf("分支树视图 (%d 分支 · %d 节点)", a.treeSnap.BranchCount, len(a.treeSnap.Nodes))).
				Bold().FontSize(11).TextColor(p.TextSecondary)
			ui.Spacer(c)
			ui.Text(c, "点击节点回溯").FontSize(10).TextColor(p.TextMuted)
		})

		ui.Scroll(c).Grow(1).FillWidth().Children(func() {
			a.renderTreeNodesList(c, p, a.treeSnap.Nodes)
		})
	})
}

func (a *DeskApp) renderTreeNodesList(c *ui.Context, p Palette, nodes []harness.SessionTreeNode) {
	ui.Column(c).FillWidth().Gap(2).Children(func() {
		for _, node := range nodes {
			n := node
			indent := float32(n.Depth * 12)
			if indent > 36 {
				indent = 36
			}

			ui.Row(c).FillWidth().Padding(0, 0, 0, indent).Children(func() {
				nb := ui.ButtonBase(c).FillWidth().Padding(4, 6).Radius(5).
					Gap(6).AlignItems(ui.Center).Disabled(a.isRunning)

				rowBg := ui.Transparent
				textCol := p.TextMuted
				dotCol := p.TextMuted
				if n.IsActiveCursor {
					rowBg = p.AccentSoft
					textCol = p.Accent
					dotCol = p.Accent
				} else if n.OnActiveBranch {
					textCol = p.TextPrimary
					dotCol = p.LocalColor
				}
				if nb.Hovered() && !a.isRunning && !n.IsActiveCursor {
					rowBg = p.BgHover
					textCol = p.TextPrimary
				}

				roleTag := "AI"
				if n.Role == "user" {
					roleTag = "You"
				} else if n.Role == "compaction" {
					roleTag = "Sum"
				}
				if n.BranchIndex > 0 {
					roleTag = fmt.Sprintf("[分支%d]", n.BranchIndex)
				}

				nb.Background(rowBg).Children(func() {
					ui.Box(c).Size(6, 6).Radius(99).Background(dotCol)
					ui.Text(c, roleTag).Font("Menlo").FontSize(10).TextColor(p.TextMuted)
					el := ui.Text(c, n.Label).Grow(1).FontSize(11.5).TextColor(textCol).SingleLine().Ellipsis("...")
					if n.IsActiveCursor || n.OnActiveBranch {
						el.Bold()
					}
					if n.IsForkPoint {
						ui.Row(c).Padding(1, 5).Radius(4).Background(p.WarnSoft).Children(func() {
							ui.Text(c, "分叉点").Bold().Font("Menlo").FontSize(9.5).TextColor(p.WarnColor)
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
			})
		}
	})
}

func (a *DeskApp) switchChannel(c *ui.Context, profileID string) {
	a.cfg.ActiveProfileID = profileID
	prof := a.cfg.ActiveProfile()
	_ = a.cfg.Save()
	if a.mgr != nil {
		_ = a.mgr.ApplyProfile(prof)
	}
	if c != nil && prof != nil {
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
	_ = a.cfg.Save()
	if a.mgr != nil {
		_ = a.mgr.ApplyProfile(prof)
	}
	if c != nil {
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
	_ = a.cfg.Save()
	if a.mgr != nil {
		_ = a.mgr.ApplyProfile(prof)
	}
	if c != nil {
		c.Toast(fmt.Sprintf("已切换至渠道 [%s] · 模型 [%s]", prof.Name, modelID))
	}
}

func (a *DeskApp) switchThinkingLevel(c *ui.Context, level string) {
	prof := a.cfg.ActiveProfile()
	if prof == nil {
		return
	}
	prof.ThinkingLevel = level
	_ = a.cfg.Save()
	if a.mgr != nil {
		_ = a.mgr.ApplyProfile(prof)
	}
	if c != nil {
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
	_ = a.cfg.Save()
	if a.mgr != nil {
		_ = a.mgr.ApplyProfile(prof)
	}
	if c != nil {
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
				curThink := p.ThinkingLevel
				if curThink == "" {
					curThink = "medium"
				}

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

	ui.Column(c).Grow(1).FillHeight().Background(p.BgCanvas).Children(func() {
		// 44px Top Status Bar (.top-bar) — Minimal & Clean
		ui.Row(c).FillWidth().Height(44).Padding(0, 16).Gap(8).
			AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).Children(func() {
			// Active Environment Status Badge (.status-badge)
			envBg := p.LocalSoft
			envFg := p.LocalColor
			envText := "本地工作区 · " + target.DisplaySub()
			if target.IsRemote {
				envBg = p.SSHSoft
				envFg = p.SSHColor
				envText = "远程 SSH · " + target.DisplaySub()
			}
			ui.Row(c).Padding(4, 10).Radius(6).Background(envBg).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(6, 6).Radius(99).Background(envFg)
				ui.Text(c, envText).Bold().Font("Menlo").FontSize(11.5).TextColor(envFg)
			})

			// Branch Count Chip (only shown when conversation has forked into multiple branches)
			if a.treeSnap != nil && a.treeSnap.BranchCount > 1 {
				a.renderTopChip(c, p, IconBranch, fmt.Sprintf("%d 分支", a.treeSnap.BranchCount), func() {
					a.sidebarTab = 1
				})
			}

			// Context Token Estimate (when conversation has history)
			if tokens := a.mgr.EstimateContextTokens(); tokens > 0 {
				ui.Text(c, fmt.Sprintf("~%s tokens", formatTokenCount(tokens))).
					Font("Menlo").FontSize(11).TextColor(p.TextMuted)
			}

			if a.activeToolStatus != "" {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.Spinner(c).Size(12, 12)
					ui.Text(c, a.activeToolStatus).Font("Menlo").FontSize(11.5).TextColor(p.Accent)
				})
			}

			ui.Spacer(c)

			// Safety Policy Chip (.top-chip)
			safetyLabel := "远程高危拦截"
			switch a.cfg.SafetyPolicy {
			case store.SafetyPolicyAlwaysHighRisk:
				safetyLabel = "全局高危拦截"
			case store.SafetyPolicyStrictRemote:
				safetyLabel = "远程严格审批"
			case store.SafetyPolicyOff:
				safetyLabel = "拦截已关闭"
			}
			a.renderTopChip(c, p, IconShield, safetyLabel, func() {
				a.settingsModal.ActiveTab = 2
				a.settingsModal.Open = true
			})

			// Right Terminal Drawer Toggle Chip
			termToggleLabel := "终端"
			if !a.termDrawer.Open {
				termToggleLabel = "显示终端"
			}
			a.renderTopChip(c, p, IconTerminal, termToggleLabel, func() {
				a.termDrawer.Open = !a.termDrawer.Open
			})
		})

		// Error alert bar if last run failed
		if a.lastError != "" {
			ui.Row(c).FillWidth().Padding(8, 16).Gap(8).AlignItems(ui.Center).
				Background(p.DangerSoft).BorderWidth(0, 0, 1, 0).BorderColor(p.DangerColor).Children(func() {
				ui.Icon(c, IconShieldAlert).Size(14, 14).TextColor(p.DangerColor)
				ui.Text(c, a.lastError).FontSize(12).TextColor(p.DangerColor).Grow(1).Selectable()
				MetaActionBtn(c, p, "配置渠道与模型", func() {
					a.settingsModal.ActiveTab = 0
					a.settingsModal.Open = true
				})
				MetaActionBtn(c, p, "关闭", func() {
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
			ui.Column(c).FillWidth().AlignItems(ui.Center).Children(func() {
				ui.Column(c).FillWidth().MaxWidth(760).Children(func() {
					RenderChatEntryWithActions(c, p, a.entries[i], &a.mdOpts, canFork, func(clicked *ChatEntry) {
						a.forkFromChatEntry(c, clicked)
					})
				})
			})
		}).Grow(1).Padding(20, 24, 10, 24).Gap(16).Children(func() {
			if len(a.entries) == 0 {
				ui.Column(c).Margin(ui.Auto).AlignItems(ui.Center).Gap(8).MaxWidth(520).Children(func() {
					ui.Text(c, "大脑在本地，手脚可插拔").Bold().FontSize(18).TextColor(p.TextPrimary)
					ui.Text(c, "左侧支持同会话热切换本地目录与 ~/.ssh/config 远端主机，以及可视化 Session 分支树回溯分叉；底部胶囊输入栏支持多渠道模型随时热切换与高危命令弹窗审批。").
						FontSize(13).
						TextColor(p.TextMuted).
						TextAlign(ui.Center)
				})
			}
		})

		// Bottom Floating Pill Composer (.composer-wrap -> .composer-card)
		ui.Column(c).FillWidth().Padding(4, 24, 14, 24).AlignItems(ui.Center).Children(func() {
			ui.Column(c).FillWidth().MaxWidth(760).Padding(10, 12, 8, 12).Radius(14).
				Background(p.BgSurface).Border(1, p.BorderStrong).Gap(8).Children(func() {
				placeholder := "输入任务指令 (Enter 发送，Shift+Enter 换行；运行中发送将直接触发 Steer 插队纠偏)..."
				ui.TextArea(c, &a.input).
					Lines(1, 6).
					FillWidth().
					Placeholder(placeholder).
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

				// Bottom Composer Pill Bar (.composer-bar) — Single Unified Cascading Selector + Send/Steer
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					if prof != nil {
						a.renderUnifiedCascaderPill(c, p, prof)
					}

					ui.Spacer(c)

					// Stop Button (when running)
					if a.isRunning {
						stopBtn := ui.ButtonBase(c).Padding(5, 11).Radius(99).
							Border(1, p.DangerColor).Gap(5).AlignItems(ui.Center)
						stopBg := p.DangerSoft
						if stopBtn.Hovered() {
							stopBg = p.DangerColor.Alpha(0.24)
						}
						stopBtn.Background(stopBg).Children(func() {
							ui.Icon(c, IconStop).Size(11, 11).TextColor(p.DangerColor)
							ui.Text(c, "停止").Bold().FontSize(11.5).TextColor(p.DangerColor)
						})
						if stopBtn.Clicked() {
							a.mgr.Abort()
							if a.cancelRun != nil {
								a.cancelRun()
							}
							a.approvalModal.Clear()
						}
					}

					// Primary Send / Steer Pill Button (.send-btn)
					hasInput := strings.TrimSpace(a.input) != ""
					sendBtn := ui.ButtonBase(c).Padding(5, 13).Radius(99).
						Gap(5).AlignItems(ui.Center).Disabled(!hasInput)

					btnBg := p.BtnPrimaryBg
					btnFg := p.BtnPrimaryFg
					btnIcon := IconArrowUp
					btnText := "发送"
					if a.isRunning {
						btnBg = p.WarnColor
						btnFg = ui.Hex("#09090B")
						btnIcon = IconBolt
						btnText = "Steer 插队"
					}
					if !hasInput {
						btnBg = p.BgElevated
						btnFg = p.TextMuted
					} else if sendBtn.Hovered() {
						btnBg = btnBg.Alpha(0.9)
					}

					sendBtn.Background(btnBg).Children(func() {
						ui.Text(c, btnText).Bold().FontSize(12).TextColor(btnFg)
						ui.Icon(c, btnIcon).Size(13, 13).TextColor(btnFg)
					})
					if sendBtn.Clicked() {
						a.submitPrompt(a.input)
					}
				})
			})
		})
	})
}

func (a *DeskApp) renderTopChip(c *ui.Context, p Palette, icon *ui.SVG, label string, onClick func()) {
	b := ui.ButtonBase(c).Padding(4, 9).Radius(6).Border(1, p.BorderSubtle).Gap(5).AlignItems(ui.Center)
	bg := ui.Transparent
	fg := p.TextSecondary
	if b.Hovered() || b.Pressed() {
		bg = p.BgHover
		fg = p.TextPrimary
	}
	b.Background(bg).Children(func() {
		ui.Icon(c, icon).Size(13, 13).TextColor(fg)
		ui.Text(c, label).FontSize(11.5).TextColor(fg)
	})
	if b.Clicked() && onClick != nil {
		onClick()
	}
}

// renderUnifiedCascaderPill renders the single cascading selector pill for Provider ▸ Model ▸ Thinking Level.
func (a *DeskApp) renderUnifiedCascaderPill(c *ui.Context, p Palette, prof *store.ProviderProfile) {
	b := ui.ButtonBase(c).Padding(4, 10).Radius(99).Border(1, p.BorderSubtle).Gap(6).AlignItems(ui.Center)
	bg := p.BgElevated
	if b.Hovered() || b.Pressed() {
		bg = p.BgHover
	}

	modelDisplay := strings.TrimSpace(prof.ModelID)
	if modelDisplay == "" {
		modelDisplay = "选择模型"
	}
	thinkDisplay := capitalizeWord(prof.ThinkingLevel)

	b.Background(bg).Children(func() {
		ui.Box(c).Size(6, 6).Radius(99).Background(p.LocalColor)
		ui.Text(c, prof.Name).FontSize(11.5).TextColor(p.TextSecondary).SingleLine()
		ui.Text(c, "/").FontSize(11).TextColor(p.TextMuted)
		ui.Text(c, modelDisplay).Bold().Font("Menlo").FontSize(11.5).TextColor(p.TextPrimary).SingleLine()
		ui.Row(c).Padding(1, 6).Radius(99).Background(p.AccentSoft).Children(func() {
			ui.Text(c, thinkDisplay).Bold().Font("Menlo").FontSize(10).TextColor(p.Accent)
		})
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
