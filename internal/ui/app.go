package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/coding"

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
	toolCards        map[string]*ChatEntry
	chatList         ui.ListState
	mdOpts           markdown.Options

	// Composer & run state
	input            string
	isRunning        bool
	activeToolStatus string
	lastError        string
	cancelRun        context.CancelFunc

	// Sidebars & modals
	savedSessions []coding.SessionInfo
	activeSessID  string
	termDrawer    *TermDrawer
	settingsModal SettingsModal
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

func (a *DeskApp) currentTargetBadge() string {
	t := a.mgr.Router().Target()
	if t.IsRemote {
		return "🌐 " + t.SSHHost
	}
	return "💻 " + filepath.Base(t.WorkDir)
}

func (a *DeskApp) refreshSessionsList() {
	if a.mgr != nil {
		a.savedSessions = a.mgr.ListSavedSessions()
	}
}

func (a *DeskApp) finalizeCurrentAssistant(msg agent.AgentMessage) {
	if a.currentAssistant == nil {
		return
	}
	SyncAssistantEntryFromMessage(a.currentAssistant, msg)
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
				entry := &ChatEntry{
					ID:          a.allocID(),
					Kind:        EntryAssistant,
					TargetBadge: a.currentTargetBadge(),
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
				entry := &ChatEntry{
					ID:          a.allocID(),
					Kind:        EntryAssistant,
					TargetBadge: a.currentTargetBadge(),
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
			if ev.Message == nil || ev.Message.MessageRole() == ai.RoleAssistant {
				a.finalizeCurrentAssistant(ev.Message)
			}

		case agent.EvToolExecutionStart:
			a.finalizeCurrentAssistant(nil)
			sum, dOld, dNew := SummarizeToolArgs(ev.ToolName, ev.Args)
			card := &ChatEntry{
				ID:          a.allocID(),
				Kind:        EntryTool,
				ToolCallID:  ev.ToolCallID,
				ToolName:    ev.ToolName,
				ToolSummary: sum,
				DiffOld:     dOld,
				DiffNew:     dNew,
				ToolRunning: true,
				TargetBadge: a.currentTargetBadge(),
				CardOpen:    ev.ToolName == "edit",
			}
			a.toolCards[ev.ToolCallID] = card
			a.entries = append(a.entries, card)
			a.activeToolStatus = fmt.Sprintf("⚙️ 正在执行 [%s] %s", ev.ToolName, sum)

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

	if a.isRunning {
		a.entries = append(a.entries, &ChatEntry{
			ID:          a.allocID(),
			Kind:        EntryUser,
			Text:        prompt,
			Steered:     true,
			TargetBadge: a.currentTargetBadge(),
			Done:        true,
		})
		a.mgr.Steer(prompt)
		return
	}

	a.entries = append(a.entries, &ChatEntry{
		ID:          a.allocID(),
		Kind:        EntryUser,
		Text:        prompt,
		TargetBadge: a.currentTargetBadge(),
		Done:        true,
	})

	a.isRunning = true
	ctx, cancel := context.WithCancel(context.Background())
	a.cancelRun = cancel

	go func() {
		res, err := a.mgr.Run(ctx, prompt)
		a.updateUI(func() {
			a.isRunning = false
			a.activeToolStatus = ""
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

// View builds the declarative GPU interface every frame.
func (a *DeskApp) View(c *ui.Context) {
	t := c.Theme()

	// Keyboard shortcut: Cmd+, opens Settings
	c.OnWindowShortcut(ui.Cmd, ui.Key(','), func() {
		a.settingsModal.Open = true
	})

	ui.Row(c).Fill().Background(t.Background).Children(func() {
		// 1. Left Sidebar: Environment Target Hot-Switcher & Session History
		a.renderLeftSidebar(c)

		// 2. Center Main Column: Top Status Bar + Markdown Conversation Stream + Composer
		a.renderCenterMain(c)

		// 3. Right Drawer: Real-time Bash Command & Stdout/Stderr Output Stream
		a.termDrawer.View(c)
	})

	// Settings Modal (API Endpoints & Workspaces)
	a.settingsModal.Render(c, a.cfg, func() {
		_ = a.mgr.ApplyProfile(a.cfg.ActiveProfile())
		if target := a.cfg.ActiveTarget(); target != nil {
			a.mgr.SwitchTarget(*target)
		}
	})
}

func (a *DeskApp) renderLeftSidebar(c *ui.Context) {
	t := c.Theme()
	activeTarget := a.mgr.Router().Target()

	ui.Column(c).Width(250).FillHeight().Padding(12).Gap(10).Background(t.Surface).BorderWidth(0, 1, 0, 0).BorderColor(t.Border).Children(func() {
		// Section 1: Workspace Target Hot-Switch
		ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
			ui.Text(c, "执行环境 (热切换)").Bold().FontSize(13)
			ui.Spacer(c)
			if ui.Button(c, "+").Clicked() {
				a.settingsModal.ActiveTab = 1
				a.settingsModal.Open = true
			}
		})

		ui.Scroll(c).MaxHeight(220).FillWidth().Children(func() {
			ui.Column(c).FillWidth().Gap(6).Children(func() {
				for i := range a.cfg.Targets {
					wt := a.cfg.Targets[i]
					isSelected := wt.ID == activeTarget.ID
					icon := "💻 "
					if wt.IsRemote {
						icon = "🌐 "
					}
					label := icon + wt.Name
					var btn ui.Element
					if isSelected {
						btn = ui.PrimaryButton(c, label).FillWidth()
					} else {
						btn = ui.Button(c, label).FillWidth()
					}
					if btn.Clicked() {
						a.cfg.ActiveTargetID = wt.ID
						_ = a.cfg.Save()
						a.mgr.SwitchTarget(wt)
						c.Toast("已热切换执行环境至: " + wt.Name)
					}
				}
			})
		})

		ui.Divider(c)

		// Section 2: JSONL Session History
		ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
			ui.Text(c, "会话历史 (.jsonl)").Bold().FontSize(13)
			ui.Spacer(c)
			if ui.Button(c, "🔄").Clicked() {
				a.refreshSessionsList()
			}
		})

		if ui.Button(c, "＋ 新建对话 (New Session)").FillWidth().Disabled(a.isRunning).Clicked() {
			if err := a.mgr.InitOrResetSession(a.cfg.ActiveProfile()); err == nil {
				a.entries = nil
				a.toolCards = map[string]*ChatEntry{}
				a.currentAssistant = nil
				a.activeSessID = ""
				a.lastError = ""
				a.refreshSessionsList()
			}
		}

		ui.Scroll(c).Grow(1).FillWidth().Children(func() {
			ui.Column(c).FillWidth().Gap(4).Children(func() {
				if len(a.savedSessions) == 0 {
					ui.Text(c, "暂无历史会话，发送第一条消息后自动持久化为树状 JSONL。").
						FontSize(11).
						TextColor(t.TextMuted).
						Padding(4, 2)
				}
				for _, info := range a.savedSessions {
					sessInfo := info
					shortID := sessInfo.ID
					if len(shortID) > 8 {
						shortID = shortID[:8]
					}
					ts := sessInfo.Timestamp
					if len(ts) >= 16 {
						ts = strings.ReplaceAll(ts[:16], "T", " ")
					}
					label := fmt.Sprintf("📄 %s (%d条)", ts, sessInfo.Messages)
					if ts == "" {
						label = fmt.Sprintf("📄 %s (%d条)", shortID, sessInfo.Messages)
					}

					var btn ui.Element
					if a.activeSessID == sessInfo.ID {
						btn = ui.PrimaryButton(c, label).FillWidth()
					} else {
						btn = ui.Button(c, label).FillWidth()
					}
					if btn.Disabled(a.isRunning).Clicked() {
						msgs, err := a.mgr.ResumeSavedSession(sessInfo.Path, a.cfg.ActiveProfile())
						if err != nil {
							a.lastError = err.Error()
						} else {
							a.toolCards = map[string]*ChatEntry{}
							a.currentAssistant = nil
							a.entries = RebuildEntriesFromMessages(msgs, &a.nextEntryID)
							a.activeSessID = sessInfo.ID
							a.lastError = ""
							c.Toast("已加载历史会话")
						}
					}
				}
			})
		})

		ui.Divider(c)
		if ui.Button(c, "⚙️ 模型与 API 设置").FillWidth().Clicked() {
			a.settingsModal.ActiveTab = 0
			a.settingsModal.Open = true
		}
	})
}

func (a *DeskApp) renderCenterMain(c *ui.Context) {
	t := c.Theme()
	target := a.mgr.Router().Target()
	prof := a.cfg.ActiveProfile()

	ui.Column(c).Grow(1).FillHeight().Children(func() {
		// Top Status Bar
		ui.Row(c).FillWidth().Padding(10, 16).Gap(10).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
			modeBadge := "🟢 本地: " + target.WorkDir
			if target.IsRemote {
				modeBadge = "🟣 远程 SSH: " + target.SSHHost + ":" + target.WorkDir
			}
			ui.Badge(c, modeBadge)

			if prof != nil {
				modelTag := fmt.Sprintf("🤖 %s (%s · Think:%s)", prof.ModelID, prof.Format, prof.ThinkingLevel)
				if ui.Button(c, modelTag).Clicked() {
					a.settingsModal.ActiveTab = 0
					a.settingsModal.Open = true
				}
			}

			if a.activeToolStatus != "" {
				ui.Text(c, a.activeToolStatus).FontSize(12).TextColor(t.Accent)
			}

			ui.Spacer(c)

			termToggleLabel := "隐藏终端栏"
			if !a.termDrawer.Open {
				termToggleLabel = "🐚 打开终端栏"
			}
			if ui.Button(c, termToggleLabel).Clicked() {
				a.termDrawer.Open = !a.termDrawer.Open
			}
		})

		// Error alert bar if last run failed
		if a.lastError != "" {
			ui.Row(c).FillWidth().Padding(8, 16).Gap(8).AlignItems(ui.Center).Background(ui.RGBA(239, 68, 68, 0.14)).BorderWidth(0, 0, 1, 0).BorderColor(t.Danger).Children(func() {
				ui.Text(c, "⚠️ "+a.lastError).FontSize(12).TextColor(t.Danger).Grow(1).Selectable()
				if ui.Button(c, "配置 API Key").Clicked() {
					a.settingsModal.ActiveTab = 0
					a.settingsModal.Open = true
				}
				if ui.Button(c, "关闭").Clicked() {
					a.lastError = ""
				}
			})
		}

		// Central Markdown Conversation Stream
		a.chatList.Key = func(i int) any {
			if i < len(a.entries) {
				return a.entries[i].ID
			}
			return -1
		}
		a.chatList.FollowEnd = true
		a.chatList.FollowAnimation = &ui.Smooth

		ui.List(c, &a.chatList, len(a.entries), func(i int) {
			RenderChatEntry(c, a.entries[i], &a.mdOpts)
		}).Grow(1).Padding(16, 20).Gap(14).Children(func() {
			if len(a.entries) == 0 {
				ui.Column(c).Margin(ui.Auto).AlignItems(ui.Center).Gap(8).MaxWidth(480).Children(func() {
					ui.Text(c, "🧠 大脑在本地，手脚可插拔").Bold().FontSize(18)
					ui.Text(c, "左侧可随时在同一对话中热切换本地目录与 ~/.ssh/config 远程主机；右侧实时回显 bash 标准输出流；运行中继续发送消息将自动触发 Steer 方向盘插队纠偏。").
						FontSize(13).
						TextColor(t.TextMuted).
						TextAlign(ui.Center)
				})
			}
		})

		// Bottom Composer & Steer/Stop Controls
		ui.Row(c).FillWidth().Padding(12, 16).Gap(10).AlignItems(ui.End).BorderWidth(1, 0, 0, 0).BorderColor(t.Border).Children(func() {
			placeholder := "输入任务指令 (Enter 发送，Shift+Enter 换行；运行中发送将直接触发 ⚡ Steer 插队纠偏)..."
			ui.TextArea(c, &a.input).
				Lines(1, 6).
				Grow(1).
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

			btnLabel := "发送 (Run)"
			if a.isRunning {
				btnLabel = "⚡ 插队指导 (Steer)"
			}
			if ui.PrimaryButton(c, btnLabel).Disabled(strings.TrimSpace(a.input) == "").Clicked() {
				a.submitPrompt(a.input)
			}

			if a.isRunning {
				if ui.Button(c, "🛑 停止").Clicked() {
					a.mgr.Abort()
					if a.cancelRun != nil {
						a.cancelRun()
					}
				}
			}
		})
	})
}
