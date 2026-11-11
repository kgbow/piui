package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/transfer"
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

	// Composer, @file autocomplete, multimodal attachments & run state
	input                string
	pendingAttachments   []PendingAttachment
	nextAttachID         int
	pickingAttachment    bool
	indexingFiles        bool
	atSelectedIdx        int
	atDismissedToken     string
	isRunning            bool
	isCompacting         bool
	activeToolStatus     string
	lastError            string
	cancelRun            context.CancelFunc
	trustBannerDismissed bool

	// Sidebars, Split Panes, Workspace -> Session Tree, Session Branch Popover & Modals
	sidebarWidth        float32
	sidebarCollapsed    bool
	savedSessions       []harness.SavedSessionSummary
	workspaceSessions   map[string][]harness.SavedSessionSummary
	collapsedWorkspaces map[string]bool
	expandedAllSessions map[string]bool
	sidebarSearchOpen   bool
	sessionSearch       string
	renamingSessID      string
	renameDraft         string
	activeSessID        string
	treeSnap            *harness.SessionTreeSnapshot
	branchPopoverOpen   bool
	probingGit          bool
	gitPopoverOpen      bool
	ctxPopoverOpen      bool
	settingsModal       SettingsModal
	approvalModal       ApprovalModal
	cmdPalette          CommandPaletteModal
}

// NewDeskApp initializes the application state, config, and sky-valley/pi harness for workDir.
func NewDeskApp(workDir string) (*DeskApp, error) {
	return NewDeskAppWithOptions(store.LoadOptions{
		StartupDir:         workDir,
		ExplicitStartupDir: strings.TrimSpace(workDir) != "",
	})
}

// NewDeskAppWithOptions initializes DeskApp honoring CLI vs GUI desktop launch semantics.
func NewDeskAppWithOptions(opts store.LoadOptions) (*DeskApp, error) {
	cfg := store.LoadOrInitWithOptions(opts)
	baseDir := strings.TrimSpace(opts.StartupDir)
	if baseDir == "" {
		if t := cfg.ActiveTarget(); t != nil && !t.IsRemote && t.WorkDir != "" {
			baseDir = t.WorkDir
		}
	}

	app := &DeskApp{
		cfg:                 cfg,
		toolCards:           map[string]*ChatEntry{},
		sidebarWidth:        DefaultSidebarWidth,
		workspaceSessions:   map[string][]harness.SavedSessionSummary{},
		collapsedWorkspaces: map[string]bool{},
		expandedAllSessions: map[string]bool{},
	}
	app.mdOpts.Reveal = &markdown.Reveal{Unit: markdown.ByWord}

	mgr, err := harness.NewManager(
		baseDir,
		cfg,
		nil,
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

// Close cancels any active agent run and cleanly flushes/closes session persistence.
func (a *DeskApp) Close() error {
	if a.cancelRun != nil {
		a.cancelRun()
	}
	if a.mgr != nil {
		a.mgr.Abort()
		return a.mgr.Close()
	}
	return nil
}

// BindWindow attaches the native MyGo window handle once ready and triggers background SSH/Git/file checks.
func (a *DeskApp) BindWindow(w *mygo.Window) {
	a.win = w
	if a.mgr != nil {
		if a.mgr.Router().Target().IsRemote {
			a.probeActiveSSHAsync(false)
		}
		a.refreshGitStatusAsync()
		a.ensureWorkspaceFilesIndexed()
	}
}

func (a *DeskApp) updateUI(fn func()) {
	if a.win != nil {
		a.win.Update(fn)
	} else {
		fn()
	}
}

func (a *DeskApp) probeActiveSSHAsync(reconnect bool) {
	if a.mgr == nil {
		return
	}
	t := a.mgr.Router().Target()
	if !t.IsRemote || strings.TrimSpace(t.SSHHost) == "" {
		return
	}
	a.mgr.SetSSHChecking(t.SSHHost)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.mgr.RefreshSSHWorkspace(ctx, reconnect)
		a.updateUI(func() {})
	}()
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
	if a.mgr == nil {
		return
	}
	var titles map[string]string
	if a.cfg != nil {
		titles = a.cfg.SessionTitles
	}
	if a.workspaceSessions == nil {
		a.workspaceSessions = map[string][]harness.SavedSessionSummary{}
	} else {
		clear(a.workspaceSessions)
	}

	activeTarget := a.mgr.Router().Target()
	if a.cfg != nil {
		for _, wt := range a.cfg.Targets {
			list := a.mgr.ListWorkspaceSessionSummaries(wt, titles)
			a.workspaceSessions[wt.ID] = list
			if wt.ID == activeTarget.ID {
				a.savedSessions = list
			}
		}
	}
	if _, ok := a.workspaceSessions[activeTarget.ID]; !ok {
		list := a.mgr.ListWorkspaceSessionSummaries(activeTarget, titles)
		a.workspaceSessions[activeTarget.ID] = list
		a.savedSessions = list
	}

	a.treeSnap = a.mgr.SessionTreeSnapshot()
	if a.activeSessID == "" && a.treeSnap != nil {
		a.activeSessID = a.treeSnap.SessionID
	}
}

// OpenWorkspaceSession loads a saved JSONL session, automatically switching the active workspace
// first if the clicked session belongs to a different Local or Remote SSH workspace.
func (a *DeskApp) OpenWorkspaceSession(c *ui.Context, wt store.WorkspaceTarget, sessInfo harness.SavedSessionSummary) {
	if a.isRunning || a.mgr == nil || a.approvalModal.PendingCount() > 0 {
		return
	}

	cur := a.mgr.Router().Target()
	sameWorkspace := cur.ID == wt.ID && cur.SameLocation(wt)
	if sameWorkspace {
		projected, snap, err := a.mgr.ResumeSavedSessionProjected(sessInfo.Path, a.cfg.ActiveProfile())
		if err != nil {
			a.lastError = err.Error()
			if c != nil {
				c.Toast(a.lastError)
			}
			return
		}
		a.toolCards = map[string]*ChatEntry{}
		a.currentAssistant = nil
		a.lastUserEntry = nil
		a.branchPopoverOpen = false
		a.entries = RebuildEntriesFromProjected(projected, &a.nextEntryID)
		a.activeSessID = sessInfo.ID
		a.treeSnap = snap
		a.lastError = ""
		if c != nil {
			c.Toast("已加载历史会话: " + sessInfo.Title)
		}
		return
	}

	a.cfg.ActiveTargetID = wt.ID
	projected, snap, err := a.mgr.SwitchWorkspaceAndResumeSession(wt, sessInfo.Path, a.cfg.ActiveProfile())
	if err != nil {
		a.lastError = "切换工作区并加载会话失败: " + err.Error()
		if c != nil {
			c.Toast(a.lastError)
		}
		return
	}

	a.toolCards = map[string]*ChatEntry{}
	a.currentAssistant = nil
	a.lastUserEntry = nil
	a.pendingAttachments = nil
	a.atSelectedIdx = 0
	a.atDismissedToken = ""
	a.gitPopoverOpen = false
	a.ctxPopoverOpen = false
	a.branchPopoverOpen = false
	a.renamingSessID = ""
	a.trustBannerDismissed = false
	a.approvalModal.Clear()
	a.entries = RebuildEntriesFromProjected(projected, &a.nextEntryID)
	a.activeSessID = sessInfo.ID
	a.treeSnap = snap
	a.lastError = a.mgr.RecorderWarning()
	a.refreshSessionsList()

	if a.win != nil {
		if wt.IsRemote {
			a.probeActiveSSHAsync(false)
		}
		a.refreshGitStatusAsync()
		a.ensureWorkspaceFilesIndexed()
	}
	if a.saveConfigOrWarn(c) && c != nil {
		c.Toast(fmt.Sprintf("已切换至 %s · %s", wt.ShortName(), sessInfo.Title))
	}
}

func (a *DeskApp) triggerQuickAddLocalWorkspace() {
	if a.win == nil {
		a.settingsModal.NewTargetRemote = false
		a.settingsModal.ActiveTab = 1
		a.settingsModal.Open = true
		return
	}
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent:            a.win,
			Title:             "选择本地项目目录",
			Directory:         true,
			CreateDirectories: true,
		})
		if err != nil || len(paths) == 0 {
			return
		}
		picked := strings.TrimSpace(paths[0])
		if picked == "" {
			return
		}
		a.updateUI(func() {
			added := a.cfg.UpsertLocalTarget(picked, "", false)
			_ = a.cfg.Save()
			a.refreshSessionsList()
			if !a.isRunning && a.approvalModal.PendingCount() == 0 {
				a.SwitchWorkspace(nil, added)
			}
		})
	}()
}

// SwitchWorkspace transitions the UI and Manager to a Local or Remote SSH workspace,
// isolating conversation history and context to that workspace.
func (a *DeskApp) SwitchWorkspace(c *ui.Context, wt store.WorkspaceTarget) {
	if a.isRunning || a.mgr == nil || a.approvalModal.PendingCount() > 0 {
		return
	}
	cur := a.mgr.Router().Target()
	if cur.ID == wt.ID && cur.SameLocation(wt) {
		return
	}

	a.cfg.ActiveTargetID = wt.ID
	if err := a.mgr.SwitchWorkspace(wt, a.cfg.ActiveProfile()); err != nil {
		a.lastError = "切换工作区失败: " + err.Error()
		if c != nil {
			c.Toast(a.lastError)
		}
		return
	}

	a.entries = nil
	a.toolCards = map[string]*ChatEntry{}
	a.currentAssistant = nil
	a.lastUserEntry = nil
	a.pendingAttachments = nil
	a.atSelectedIdx = 0
	a.atDismissedToken = ""
	a.gitPopoverOpen = false
	a.ctxPopoverOpen = false
	a.branchPopoverOpen = false
	a.activeSessID = ""
	a.treeSnap = nil
	a.renamingSessID = ""
	a.trustBannerDismissed = false
	a.approvalModal.Clear()
	a.lastError = a.mgr.RecorderWarning()
	a.refreshSessionsList()

	if a.win != nil {
		if wt.IsRemote {
			a.probeActiveSSHAsync(false)
		}
		a.refreshGitStatusAsync()
		a.ensureWorkspaceFilesIndexed()
	}
	if a.saveConfigOrWarn(c) && c != nil {
		c.Toast("已切换至工作区: " + wt.ShortName())
	}
}

// TrustActiveWorkspace marks the active workspace as trusted so project-scoped .pi/skills
// and remote AGENTS.md are loaded into the Agent's system prompt.
func (a *DeskApp) TrustActiveWorkspace(c *ui.Context) {
	if a.mgr == nil {
		return
	}
	t := a.mgr.Router().Target()
	a.cfg.SetTargetTrusted(t.ID, true)
	a.trustBannerDismissed = false
	_ = a.saveConfigOrWarn(c)

	if t.IsRemote {
		t.Trusted = true
		a.mgr.Router().UpdateTargetMetadata(t)
		if a.win != nil {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				a.mgr.SetWorkspaceTrust(ctx, true)
				a.updateUI(func() {})
			}()
		} else {
			a.mgr.SetWorkspaceTrust(context.Background(), true)
		}
	} else {
		a.mgr.SetWorkspaceTrust(context.Background(), true)
	}
	if c != nil {
		c.Toast("已信任当前工作区，项目级 Skills 与指令将在下一轮对话生效")
	}
}

// RenameSession saves a custom display title for a session ID (empty string restores default auto-title).
func (a *DeskApp) RenameSession(c *ui.Context, sessionID, newTitle string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	a.cfg.SetSessionTitle(sessionID, newTitle)
	a.renamingSessID = ""
	a.renameDraft = ""
	_ = a.saveConfigOrWarn(c)
	a.refreshSessionsList()
	if c != nil {
		c.Toast("已更新会话标题")
	}
}

// DeleteSession deletes a saved JSONL session from disk and resets the timeline if it was active.
func (a *DeskApp) DeleteSession(c *ui.Context, sessInfo harness.SavedSessionSummary) {
	if a.isRunning || a.mgr == nil {
		return
	}
	wasActive, err := a.mgr.DeleteSavedSession(sessInfo.Path, a.cfg.ActiveProfile())
	if err != nil {
		a.lastError = "删除会话失败: " + err.Error()
		if c != nil {
			c.Toast(a.lastError)
		}
		return
	}
	a.cfg.DeleteSessionTitle(sessInfo.ID)
	_ = a.saveConfigOrWarn(nil)

	if wasActive || a.activeSessID == sessInfo.ID {
		a.entries = nil
		a.toolCards = map[string]*ChatEntry{}
		a.currentAssistant = nil
		a.lastUserEntry = nil
		a.activeSessID = ""
		a.treeSnap = nil
	}
	if a.renamingSessID == sessInfo.ID {
		a.renamingSessID = ""
		a.renameDraft = ""
	}
	a.refreshSessionsList()
	if c != nil {
		c.Toast("已删除会话: " + sessInfo.Title)
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
	a.pendingAttachments = nil
	a.atSelectedIdx = 0
	a.atDismissedToken = ""
	a.ctxPopoverOpen = false
	a.branchPopoverOpen = false
	a.activeSessID = ""
	a.treeSnap = nil
	a.renamingSessID = ""
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
					card.ToolOutput = StripANSI(partial)
				}
			}

		case agent.EvToolExecutionEnd:
			if card, ok := a.toolCards[ev.ToolCallID]; ok {
				card.ToolRunning = false
				card.ToolError = ev.IsError
				if ev.IsError {
					card.CardOpen = true
				}
				if ev.DurationMs != nil {
					card.DurationMs = *ev.DurationMs
				}
				if out := ExtractResultText(ev.Result); out != "" {
					card.ToolOutput = StripANSI(out)
				}
				a.syncToolCardMutationSnapshot(card)
			}
			a.activeToolStatus = ""
		}
	})
	return nil
}

// submitPrompt either starts a new agent turn or injects a Steer message mid-run.
func (a *DeskApp) submitPrompt(raw string) {
	a.submitPromptWithContext(nil, raw)
}

func (a *DeskApp) submitPromptWithContext(c *ui.Context, raw string) {
	prompt := strings.TrimSpace(raw)
	if a.handleSlashCommand(c, prompt) {
		return
	}
	if !a.isRunning && strings.HasPrefix(prompt, "!") {
		a.ExecBangCommand(c, prompt)
		return
	}
	if prompt == "" && len(a.pendingAttachments) == 0 {
		return
	}
	atts, attBadges := a.consumePendingAttachments()
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
			Attachments:   attBadges,
			Steered:       true,
			TargetBadge:   badge,
			IsRemote:      isRem,
			ParentEntryID: parentID,
			Done:          true,
		}
		a.lastUserEntry = userEntry
		a.entries = append(a.entries, userEntry)
		a.mgr.SteerWithAttachments(prompt, atts)
		return
	}

	userEntry := &ChatEntry{
		ID:            a.allocID(),
		Kind:          EntryUser,
		Text:          prompt,
		Attachments:   attBadges,
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
		res, err := a.mgr.RunWithAttachments(ctx, prompt, atts)
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
			if a.win != nil {
				a.refreshGitStatusAsync()
			}
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
	c.OnWindowShortcut(ui.Cmd, ui.KeyK, func() {
		a.cmdPalette.Toggle()
	})
	c.OnWindowShortcut(ui.Cmd, ui.KeyComma, func() {
		a.settingsModal.Open = true
	})
	c.OnWindowShortcut(ui.Cmd, ui.KeyN, func() {
		a.startNewSession(c)
	})
	c.OnWindowShortcut(ui.Cmd, ui.KeyB, func() {
		a.sidebarCollapsed = !a.sidebarCollapsed
	})
	c.OnWindowShortcut(ui.Cmd|ui.Shift, ui.KeyL, func() {
		a.toggleTheme(c)
	})
	c.OnWindowShortcut(0, ui.KeyEscape, func() {
		if a.isRunning && !a.settingsModal.Open && !a.cmdPalette.Open && a.approvalModal.PendingCount() == 0 {
			a.mgr.Abort()
			if a.cancelRun != nil {
				a.cancelRun()
			}
		}
	})

	if a.sidebarCollapsed {
		ui.Row(c).Fill().Background(p.BgCanvas).Children(func() {
			a.renderCenterMain(c, p)
		})
	} else {
		ui.Split(c, &a.sidebarWidth, func() {
			a.renderLeftSidebar(c, p)
		}, func() {
			a.renderCenterMain(c, p)
		}).SplitReset(DefaultSidebarWidth).Fill().Background(p.BgCanvas)
	}

	// Settings Modal (API Endpoints, Workspaces, Safety Policy & MCP Servers)
	a.settingsModal.Render(c, p, a.win, a.cfg, a.mgr, a.updateUI, func() error {
		if err := a.mgr.ApplyProfile(a.cfg.ActiveProfile()); err != nil {
			a.lastError = "应用渠道配置失败: " + err.Error()
			return err
		}
		a.mgr.SetSafetyPolicy(a.cfg.SafetyPolicy)
		mcpCopy := make([]store.MCPServerConfig, len(a.cfg.MCPServers))
		for i, ms := range a.cfg.MCPServers {
			mcpCopy[i] = ms.Clone()
		}
		if a.win != nil {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_ = a.mgr.SyncMCPServers(ctx, mcpCopy)
				a.updateUI(func() {})
			}()
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = a.mgr.SyncMCPServers(ctx, mcpCopy)
			cancel()
		}
		if target := a.cfg.ActiveTarget(); target != nil {
			cur := a.mgr.Router().Target()
			if !cur.SameLocation(*target) {
				if !a.isRunning {
					a.SwitchWorkspace(nil, *target)
				} else {
					a.mgr.SwitchTarget(*target)
				}
			} else if cur.Trusted != target.Trusted || cur.Name != target.Name {
				a.mgr.Router().UpdateTargetMetadata(*target)
				if cur.Trusted != target.Trusted {
					if target.IsRemote && a.win != nil {
						go func(trusted bool) {
							ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()
							a.mgr.SetWorkspaceTrust(ctx, trusted)
							a.updateUI(func() {})
						}(target.Trusted)
					} else {
						a.mgr.SetWorkspaceTrust(context.Background(), target.Trusted)
					}
				}
			}
		}
		a.refreshSessionsList()
		return nil
	})

	// Spotlight Command Palette (Cmd/Ctrl+K)
	a.cmdPalette.Render(c, p, a.buildPaletteCommands())

	// Human-in-the-Loop High-Risk Tool Approval Modal
	if a.mgr != nil {
		a.approvalModal.Render(c, p, a.mgr.SafetyGate())
	}
}

const defaultVisibleSessionsPerWorkspace = 6

func (a *DeskApp) sessionsForWorkspace(wt store.WorkspaceTarget, activeTargetID string) []harness.SavedSessionSummary {
	if a.workspaceSessions != nil {
		if list, ok := a.workspaceSessions[wt.ID]; ok && len(list) > 0 {
			return list
		}
	}
	if wt.ID == activeTargetID && len(a.savedSessions) > 0 {
		return a.savedSessions
	}
	return nil
}

func (a *DeskApp) visibleWorkspaceSessions(wtID string, sessions []harness.SavedSessionSummary, isActiveWorkspace bool) ([]harness.SavedSessionSummary, bool) {
	if len(sessions) <= defaultVisibleSessionsPerWorkspace {
		return sessions, false
	}
	if a.expandedAllSessions != nil && a.expandedAllSessions[wtID] {
		return sessions, true
	}
	out := make([]harness.SavedSessionSummary, defaultVisibleSessionsPerWorkspace)
	copy(out, sessions[:defaultVisibleSessionsPerWorkspace])
	if isActiveWorkspace && a.activeSessID != "" {
		inTop := false
		for _, s := range out {
			if s.ID == a.activeSessID {
				inTop = true
				break
			}
		}
		if !inTop {
			for i := defaultVisibleSessionsPerWorkspace; i < len(sessions); i++ {
				if sessions[i].ID == a.activeSessID {
					out[defaultVisibleSessionsPerWorkspace-1] = sessions[i]
					break
				}
			}
		}
	}
	return out, true
}

func (a *DeskApp) renderLeftSidebar(c *ui.Context, p Palette) {
	activeTarget := a.mgr.Router().Target()
	bar := c.TitleBar()
	leftHeaderPad := max(bar.Left, 12)
	switchBlocked := a.isRunning || a.approvalModal.PendingCount() > 0

	ui.Column(c).Fill().Clip().Background(p.BgSidebar).Children(func() {
		// Row 0: Unified 44px Top Header Bar (aligns pixel-for-pixel with CenterMain)
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

		// Section 1: Quiet "工作区" Header + Filter & Add-Workspace Actions
		searchActive := a.sidebarSearchOpen || strings.TrimSpace(a.sessionSearch) != ""
		ui.Row(c).FillWidth().Padding(10, 10, 4, 16).Gap(2).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "工作区").FontWeight(WeightMedium).FontSize(FontSmall).TextColor(p.TextMuted).SingleLine()
			ui.Spacer(c)

			ToolbarIconBtn(c, p, IconFilter, 26, 13.5, searchActive, ui.Transparent, "筛选工作区与会话", func() {
				if searchActive {
					a.sidebarSearchOpen = false
					a.sessionSearch = ""
				} else {
					a.sidebarSearchOpen = true
				}
			})

			addBtn := ui.ButtonBase(c).Size(26, 26).Radius(RadiusSM).
				AlignItems(ui.Center).Justify(ui.Center).
				Tooltip("添加本地项目目录或手动配置远程 SSH 工作区").Label("添加工作区")
			addBg := ui.Transparent
			addFg := p.TextMuted
			if addBtn.Hovered() || addBtn.Pressed() {
				addBg = p.BgHover
				addFg = p.TextPrimary
			}
			addBtn.Background(addBg).Children(func() {
				ui.Icon(c, IconFolderPlus).Size(14, 14).TextColor(addFg)
			}).PopoverMenuButton(func(m *ui.Menu) {
				if m.Item("打开本地项目目录...").Chosen() {
					a.triggerQuickAddLocalWorkspace()
				}
				if m.Item("手动添加远程 SSH 工作区...").Chosen() {
					a.settingsModal.NewTargetRemote = true
					a.settingsModal.ActiveTab = 1
					a.settingsModal.Open = true
				}
				m.Separator()
				if m.Item("刷新会话列表").Chosen() {
					a.refreshSessionsList()
					c.Toast("已刷新所有工作区会话列表")
				}
				if m.Item("管理所有工作区...").Chosen() {
					a.settingsModal.ActiveTab = 1
					a.settingsModal.Open = true
				}
			})
		})

		if searchActive {
			ui.Row(c).FillWidth().Padding(2, 10, 6, 10).Gap(4).AlignItems(ui.Center).Children(func() {
				ui.TextInput(c, &a.sessionSearch).Grow(1).Placeholder("搜索工作区或会话标题...").AutoFocus().
					OnTextKey(func(k ui.TextKey) bool {
						if k.Key == ui.KeyEscape {
							a.sessionSearch = ""
							a.sidebarSearchOpen = false
							return true
						}
						return false
					})
				if strings.TrimSpace(a.sessionSearch) != "" {
					IconTipBtn(c, p, IconCross, 24, 12.5, "清空筛选", func() {
						a.sessionSearch = ""
					})
				}
			})
		}

		// Section 2: Unified Single-Scroll Workspace -> Sessions Tree
		q := strings.ToLower(strings.TrimSpace(a.sessionSearch))
		ui.Scroll(c).Grow(1).FillWidth().Children(func() {
			ui.Column(c).FillWidth().MinWidth(0).Padding(4, 8, 14, 8).Gap(10).Children(func() {
				matchedWorkspaces := 0
				for i := range a.cfg.Targets {
					wt := a.cfg.Targets[i]
					allSessions := a.sessionsForWorkspace(wt, activeTarget.ID)
					sessions := allSessions

					if q != "" {
						wsMatches := strings.Contains(strings.ToLower(wt.ShortName()), q) ||
							strings.Contains(strings.ToLower(wt.DisplaySub()), q)
						var filtered []harness.SavedSessionSummary
						for _, s := range allSessions {
							if strings.Contains(strings.ToLower(s.Title), q) || strings.Contains(strings.ToLower(s.Timestamp), q) {
								filtered = append(filtered, s)
							}
						}
						if !wsMatches && len(filtered) == 0 {
							continue
						}
						if len(filtered) > 0 {
							sessions = filtered
						}
					}

					matchedWorkspaces++
					a.renderSidebarWorkspaceNode(c, p, wt, activeTarget.ID, sessions, q != "", switchBlocked)
				}

				if matchedWorkspaces == 0 && q != "" {
					ui.Text(c, "未找到匹配的工作区或会话").
						FontSize(FontCaption).TextColor(p.TextMuted).Padding(12, 16)
				}
			})
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

func (a *DeskApp) renderSidebarWorkspaceNode(
	c *ui.Context,
	p Palette,
	wt store.WorkspaceTarget,
	activeTargetID string,
	sessions []harness.SavedSessionSummary,
	forceExpand bool,
	switchBlocked bool,
) {
	isSelected := wt.ID == activeTargetID
	if a.collapsedWorkspaces == nil {
		a.collapsedWorkspaces = map[string]bool{}
	}
	if a.expandedAllSessions == nil {
		a.expandedAllSessions = map[string]bool{}
	}
	collapsed := a.collapsedWorkspaces[wt.ID] && !forceExpand

	ui.Column(c).FillWidth().MinWidth(0).Gap(1).Children(func() {
		// Parent Workspace Row — 8px left pad + 15px icon + 9px gap => title text starts cleanly at x=32px
		wsRow := ui.Row(c).FillWidth().MinWidth(0).Height(30).Radius(RadiusSM).AlignItems(ui.Center)
		wsHovered := wsRow.Hovered()
		wsBg := ui.Transparent
		if wsHovered && !switchBlocked {
			wsBg = p.BgHover
		}

		wsIcon := IconFolder
		wsIconCol := p.TextMuted
		wsNameCol := p.TextSecondary
		if wt.IsRemote {
			wsIcon = IconCloud
			if isSelected {
				wsIconCol = p.SSHColor
			} else if wsHovered {
				wsIconCol = p.SSHColor.Alpha(0.85)
			}
		} else if isSelected {
			wsIconCol = p.TextPrimary
		} else if wsHovered {
			wsIconCol = p.TextSecondary
		}
		if isSelected || wsHovered {
			wsNameCol = p.TextPrimary
		}

		tip := wt.ShortName() + " · " + wt.DisplaySub()
		if a.approvalModal.PendingCount() > 0 {
			tip = "请先处理当前挂起的安全审批弹窗，再切换工作区"
		} else if a.isRunning {
			tip = "请等待当前对话轮次完成或点击停止后，再切换工作区"
		}

		wsRow.Background(wsBg).Padding(0, 4, 0, 0).ContextMenu(func(m *ui.Menu) {
			if m.Item("在此工作区新建对话").Disabled(switchBlocked).Chosen() {
				a.collapsedWorkspaces[wt.ID] = false
				if isSelected {
					a.startNewSession(c)
				} else {
					a.SwitchWorkspace(c, wt)
				}
			}
			if len(sessions) > 0 {
				toggleLabel := "折叠会话列表"
				if a.collapsedWorkspaces[wt.ID] {
					toggleLabel = "展开会话列表"
				}
				if m.Item(toggleLabel).Chosen() {
					a.collapsedWorkspaces[wt.ID] = !a.collapsedWorkspaces[wt.ID]
				}
			}
			trustLabel := "信任项目级 Skills 与指令"
			if wt.Trusted {
				trustLabel = "取消信任项目级 Skills"
			}
			if m.Item(trustLabel).Chosen() {
				nextTrust := !wt.Trusted
				a.cfg.SetTargetTrusted(wt.ID, nextTrust)
				_ = a.saveConfigOrWarn(c)
				if isSelected {
					a.mgr.SetWorkspaceTrust(context.Background(), nextTrust)
				}
			}
			m.Separator()
			if m.Item("管理工作区设置...").Chosen() {
				a.settingsModal.ActiveTab = 1
				a.settingsModal.Open = true
			}
			if len(a.cfg.Targets) > 1 {
				if m.Item("移除此工作区").Danger().Disabled(switchBlocked).Chosen() {
					if a.cfg.RemoveTarget(wt.ID) {
						_ = a.saveConfigOrWarn(c)
						a.refreshSessionsList()
						if isSelected {
							if next := a.cfg.ActiveTarget(); next != nil {
								a.SwitchWorkspace(c, *next)
							}
						}
					}
				}
			}
		}).Children(func() {
			b := ui.ButtonBase(c).Grow(1).MinWidth(0).Height(30).Padding(0, 6, 0, 8).Radius(RadiusSM).
				Gap(9).AlignItems(ui.Center).Disabled(switchBlocked).Tooltip(tip)
			b.Children(func() {
				ui.Icon(c, wsIcon).Size(15, 15).TextColor(wsIconCol).Shrink(0)
				el := ui.Text(c, wt.ShortName()).Grow(1).MinWidth(0).
					FontSize(FontBody).TextColor(wsNameCol).SingleLine().Ellipsis("...")
				if isSelected {
					el.FontWeight(WeightMedium)
				}
				if collapsed && len(sessions) > 0 && !wsHovered {
					ui.Text(c, fmt.Sprintf("%d", len(sessions))).
						FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextMuted).Shrink(0)
					ui.Icon(c, IconChevronRight).Size(12, 12).TextColor(p.TextMuted).Shrink(0)
				}
			})
			if b.Clicked() {
				if !isSelected {
					a.collapsedWorkspaces[wt.ID] = false
					a.SwitchWorkspace(c, wt)
				} else if len(sessions) > 0 {
					a.collapsedWorkspaces[wt.ID] = !a.collapsedWorkspaces[wt.ID]
				}
			}

			if wsHovered && !switchBlocked {
				ui.Row(c).Shrink(0).Gap(1).AlignItems(ui.Center).Children(func() {
					IconTipBtn(c, p, IconPlus, 22, 12.5, "在 "+wt.ShortName()+" 新建对话", func() {
						a.collapsedWorkspaces[wt.ID] = false
						if isSelected {
							a.startNewSession(c)
						} else {
							a.SwitchWorkspace(c, wt)
						}
					})
					if len(sessions) > 0 {
						chev := IconChevronDown
						chevTip := "折叠工作区会话"
						if collapsed {
							chev = IconChevronRight
							chevTip = "展开工作区会话"
						}
						IconTipBtn(c, p, chev, 22, 12, chevTip, func() {
							a.collapsedWorkspaces[wt.ID] = !a.collapsedWorkspaces[wt.ID]
						})
					}
				})
			}
		})

		if collapsed {
			return
		}

		if len(sessions) == 0 {
			if isSelected {
				ui.Row(c).FillWidth().Height(26).Padding(0, 8, 0, 32).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "暂无历史对话").FontSize(FontCaption).TextColor(p.TextMuted).SingleLine()
				})
			}
			return
		}

		shownSessions := sessions
		hasOverflow := false
		if !forceExpand {
			shownSessions, hasOverflow = a.visibleWorkspaceSessions(wt.ID, sessions, isSelected)
		}

		for _, info := range shownSessions {
			sessInfo := info
			a.renderSidebarSessionItem(c, p, wt, sessInfo, isSelected, switchBlocked)
		}

		if hasOverflow {
			expandedAll := a.expandedAllSessions[wt.ID]
			moreLabel := fmt.Sprintf("查看全部 (%d)", len(sessions))
			if expandedAll {
				moreLabel = "收起"
			}
			mb := ui.ButtonBase(c).FillWidth().MinWidth(0).Height(26).Padding(0, 8, 0, 32).Radius(RadiusSM).
				AlignItems(ui.Center).Label(moreLabel)
			mCol := p.TextMuted
			mBg := ui.Transparent
			if mb.Hovered() {
				mCol = p.TextSecondary
				mBg = p.BgHover
			}
			mb.Background(mBg).Children(func() {
				ui.Text(c, moreLabel).FontFeatures("tnum").FontSize(FontSmall).TextColor(mCol).SingleLine()
			})
			if mb.Clicked() {
				a.expandedAllSessions[wt.ID] = !expandedAll
			}
		}
	})
}

func (a *DeskApp) renderSidebarSessionItem(
	c *ui.Context,
	p Palette,
	wt store.WorkspaceTarget,
	sessInfo harness.SavedSessionSummary,
	isSelectedWorkspace bool,
	switchBlocked bool,
) {
	isActive := isSelectedWorkspace && a.activeSessID == sessInfo.ID

	if a.renamingSessID == sessInfo.ID {
		ui.Row(c).FillWidth().MinWidth(0).Padding(2, 4, 2, 28).Radius(RadiusSM).
			Background(p.BgSurface).Border(1, p.BorderFocus).Gap(3).AlignItems(ui.Center).Children(func() {
			ui.TextInput(c, &a.renameDraft).Grow(1).Placeholder("输入自定义标题").AutoFocus().
				OnTextKey(func(k ui.TextKey) bool {
					if k.Key == ui.KeyEnter && k.Mods == 0 {
						a.RenameSession(c, sessInfo.ID, k.Text())
						return true
					}
					if k.Key == ui.KeyEscape {
						a.renamingSessID = ""
						a.renameDraft = ""
						return true
					}
					return false
				})
			IconTipBtn(c, p, IconCheck, 22, 12.5, "确认保存标题", func() {
				a.RenameSession(c, sessInfo.ID, a.renameDraft)
			})
			IconTipBtn(c, p, IconCross, 22, 12.5, "取消重命名", func() {
				a.renamingSessID = ""
				a.renameDraft = ""
			})
		})
		return
	}

	rowWrap := ui.Row(c).FillWidth().MinWidth(0).Height(29).Radius(RadiusSM).AlignItems(ui.Center)
	rowHovered := rowWrap.Hovered() || rowWrap.FocusWithin()
	targetBg := ui.Transparent
	titleCol := p.TextSecondary
	if isActive {
		targetBg = p.BgActive
		titleCol = p.TextPrimary
	} else if rowHovered && !switchBlocked {
		targetBg = p.BgHover
		titleCol = p.TextPrimary
	}

	ageText := sessInfo.RelativeTime
	if ageText == "" {
		ageText = sessInfo.Timestamp
	}

	metaTip := sessInfo.Title
	if sessInfo.Timestamp != "" {
		metaTip = fmt.Sprintf("%s\n%s · %d 条消息", sessInfo.Title, sessInfo.Timestamp, sessInfo.Messages)
	}

	rowWrap.Background(targetBg).Padding(0, 4, 0, 0).ContextMenu(func(m *ui.Menu) {
		if m.Item("打开会话").Disabled(switchBlocked).Chosen() {
			a.OpenWorkspaceSession(c, wt, sessInfo)
		}
		if m.Item("重命名会话...").Disabled(a.isRunning).Chosen() {
			a.renamingSessID = sessInfo.ID
			a.renameDraft = sessInfo.Title
		}
		m.Separator()
		if m.Item("删除会话").Danger().Disabled(a.isRunning).Chosen() {
			a.DeleteSession(c, sessInfo)
		}
	}).Children(func() {
		// Left padding 32px aligns session text cleanly beneath the parent workspace title (past the 15px icon)
		b := ui.ButtonBase(c).Grow(1).MinWidth(0).Height(29).Padding(0, 6, 0, 32).Radius(RadiusSM).
			Gap(8).AlignItems(ui.Center).Disabled(switchBlocked).Tooltip(metaTip)
		b.Children(func() {
			el := ui.Text(c, sessInfo.Title).Grow(1).MinWidth(0).
				FontSize(FontSmall + 0.5).TextColor(titleCol).SingleLine().Ellipsis("...")
			if isActive {
				el.FontWeight(WeightMedium)
			}
			if (!rowHovered || switchBlocked) && ageText != "" {
				ui.Text(c, ageText).FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextMuted).Shrink(0).SingleLine()
			}
		})
		if b.Clicked() {
			a.OpenWorkspaceSession(c, wt, sessInfo)
		}

		if rowHovered && !switchBlocked {
			ui.Row(c).Shrink(0).Gap(1).AlignItems(ui.Center).Children(func() {
				IconTipBtn(c, p, IconFileEdit, 21, 11.5, "重命名会话", func() {
					a.renamingSessID = sessInfo.ID
					a.renameDraft = sessInfo.Title
				})
				DangerIconBtn(c, p, IconTrash, 21, 11.5, "删除会话", func() {
					a.DeleteSession(c, sessInfo)
				})
			})
		}
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
					a.branchPopoverOpen = false
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

func (a *DeskApp) renderSSHStatusPill(c *ui.Context, p Palette) {
	st := a.mgr.SSHStatus()
	pill := ui.ButtonBase(c).Height(26).Padding(0, 9).Radius(RadiusSM).Gap(5).AlignItems(ui.Center).Shrink(0)

	switch {
	case st.Checking:
		pill.Background(p.InputBg).Border(1, p.BorderSubtle).Tooltip("正在检测 SSH 连通性与远端环境...").Children(func() {
			ui.Spinner(c).Size(11, 11)
			ui.Text(c, "连接检测中").FontSize(FontCaption).TextColor(p.TextMuted).SingleLine()
		})
	case st.Connected:
		bg := p.LocalSoft
		if pill.Hovered() {
			bg = p.LocalColor.Alpha(0.2)
		}
		tip := fmt.Sprintf("SSH 已连接 (%s · %s)，点击刷新状态", st.RemoteOS, st.ResolvedWorkDir)
		pill.Background(bg).Border(1, p.LocalColor.Alpha(0.3)).Tooltip(tip).Children(func() {
			ui.Box(c).Size(5.5, 5.5).Radius(RadiusPill).Background(p.LocalColor)
			ui.Text(c, fmt.Sprintf("%dms", st.LatencyMs)).Font(FontMono).FontFeatures("tnum").
				FontWeight(WeightMedium).FontSize(FontCaption).TextColor(p.LocalColor).SingleLine()
		})
		if pill.Clicked() {
			a.probeActiveSSHAsync(false)
		}
	case st.Error != "":
		bg := p.DangerSoft
		if pill.Hovered() {
			bg = p.DangerColor.Alpha(0.22)
		}
		pill.Background(bg).Border(1, p.DangerColor.Alpha(0.35)).
			Tooltip("SSH 连接异常: " + st.Error + "（点击重置复用连接并重连）").Children(func() {
			ui.Icon(c, IconRefresh).Size(11.5, 11.5).TextColor(p.DangerColor)
			ui.Text(c, "未连接 · 重连").FontSize(FontCaption).FontWeight(WeightMedium).TextColor(p.DangerColor).SingleLine()
		})
		if pill.Clicked() {
			a.probeActiveSSHAsync(true)
		}
	default:
		bg := p.InputBg
		if pill.Hovered() {
			bg = p.BgHover
		}
		pill.Background(bg).Border(1, p.BorderSubtle).Tooltip("点击检测 SSH 服务器连接延迟与状态").Children(func() {
			ui.Icon(c, IconRefresh).Size(11.5, 11.5).TextColor(p.TextMuted)
			ui.Text(c, "检测连接").FontSize(FontCaption).TextColor(p.TextSecondary).SingleLine()
		})
		if pill.Clicked() {
			a.probeActiveSSHAsync(false)
		}
	}
}

func (a *DeskApp) renderBranchTreePill(c *ui.Context, p Palette) {
	branchCount := 0
	if a.treeSnap != nil {
		branchCount = a.treeSnap.BranchCount
	}
	if branchCount <= 1 && !a.branchPopoverOpen {
		return
	}
	if branchCount < 1 {
		branchCount = 1
	}

	b := ui.ButtonBase(c).Height(26).Padding(0, 8).Radius(RadiusSM).
		Border(1, p.BorderSubtle).Gap(4).AlignItems(ui.Center).Shrink(0).
		Tooltip(fmt.Sprintf("当前会话共 %d 个对话分支，点击切换或回溯分支节点", branchCount))
	bBg := ui.Transparent
	if b.Hovered() || b.Pressed() || a.branchPopoverOpen {
		bBg = p.BgHover
	}
	b.Background(bBg).Children(func() {
		ui.Icon(c, IconBranch).Size(12, 12).TextColor(p.Accent)
		ui.Text(c, fmt.Sprintf("分支 · %d", branchCount)).
			FontFeatures("tnum").FontWeight(WeightMedium).FontSize(FontCaption).TextColor(p.TextSecondary).SingleLine()
	})
	if b.Clicked() {
		if !a.branchPopoverOpen && a.mgr != nil {
			a.treeSnap = a.mgr.SessionTreeSnapshot()
		}
		a.branchPopoverOpen = !a.branchPopoverOpen
	}

	ui.PopoverBase(c, b, &a.branchPopoverOpen, func(panel ui.Element) {
		panel.Width(350).Margin(4, 0, 0, 0).Padding(0).Radius(RadiusMD).
			Background(p.ModalBg).Border(1, p.BorderStrong).
			Shadow(0, 12, 32, -6, p.ShadowModal).Children(func() {
			ui.Column(c).FillWidth().Children(func() {
				ui.Row(c).FillWidth().Padding(10, 12).AlignItems(ui.Center).Gap(6).
					BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).Children(func() {
					ui.Icon(c, IconBranch).Size(13, 13).TextColor(p.Accent)
					ui.Text(c, "对话分支拓扑树").FontWeight(WeightSemibold).FontSize(FontSmall).TextColor(p.TextPrimary)
					if a.treeSnap != nil && len(a.treeSnap.Nodes) > 0 {
						ui.Text(c, fmt.Sprintf("%d 分支 · %d 节点", a.treeSnap.BranchCount, len(a.treeSnap.Nodes))).
							FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextMuted)
					}
					ui.Spacer(c)
					IconTipBtn(c, p, IconRefresh, 24, 12.5, "刷新分支树", func() {
						if a.mgr != nil {
							a.treeSnap = a.mgr.SessionTreeSnapshot()
						}
					})
				})

				if a.treeSnap == nil || len(a.treeSnap.Nodes) == 0 {
					ui.Column(c).FillWidth().Padding(18, 14).AlignItems(ui.Center).Gap(4).Children(func() {
						ui.Text(c, "当前会话尚无分支节点").FontSize(FontSmall).TextColor(p.TextSecondary)
						ui.Text(c, "点击任意消息右侧的分叉按钮重试，或执行 /compact 后将自动生成多分支拓扑。").
							FontSize(FontCaption).LineHeight(1.5).TextColor(p.TextMuted).TextAlign(ui.Center)
					})
					return
				}

				ui.Scroll(c).MaxHeight(340).FillWidth().Children(func() {
					ui.Column(c).FillWidth().Padding(6).Children(func() {
						a.renderTreeNodesList(c, p, a.treeSnap.Nodes)
					})
				})
			})
		})
	})
}

func (a *DeskApp) renderCenterMain(c *ui.Context, p Palette) {
	target := a.mgr.Router().Target()
	prof := a.cfg.ActiveProfile()
	bar := c.TitleBar()

	leftPad := float32(16)
	if a.sidebarCollapsed {
		leftPad = max(bar.Left, 12)
	}
	rightPad := max(bar.Right, 14)

	mainCol := ui.Column(c).Direction(ui.LeftToRight).Grow(1).MinWidth(0).FillHeight().Clip().Background(p.BgCanvas)
	fileDragOver := mainCol.FileDragOver()
	if droppedPaths := mainCol.DroppedFiles(); len(droppedPaths) > 0 {
		for _, dp := range droppedPaths {
			_ = a.AddAttachmentFromPath(c, dp)
		}
	}
	if drop, ok := ui.DropData(mainCol, transfer.DropOptions{
		Formats: []transfer.Format{transfer.FileList, transfer.PNG},
	}); ok {
		a.HandleDroppedTransfer(c, drop.Data)
	}
	mainCol.Children(func() {
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

			maxCrumbW := float32(420)
			if target.IsRemote {
				maxCrumbW = 310
			}
			// Quiet Clickable Workspace Breadcrumb Pill (Neutral when Local, Purple when Remote SSH)
			wsBtn := ui.ButtonBase(c).MaxWidth(maxCrumbW).MinWidth(0).Height(26).Padding(0, 10).Radius(RadiusSM).
				Gap(6).AlignItems(ui.Center).
				Tooltip("点击管理本地与远程 SSH 工作区")
			if target.IsRemote {
				wsBg := p.SSHSoft
				if wsBtn.Hovered() {
					wsBg = p.SSHColor.Alpha(0.2)
				}
				wsBtn.Background(wsBg).Border(1, p.SSHColor.Alpha(0.28)).Children(func() {
					ui.Icon(c, IconCloud).Size(13, 13).TextColor(p.SSHColor).Shrink(0)
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

			if target.IsRemote {
				a.renderSSHStatusPill(c, p)
			}

			// Live Git Branch & Uncommitted Changes Pill (Local & Remote SSH)
			a.renderGitStatusPill(c, p)

			// Session Conversation Branch Popover Pill (shown automatically when chat forks into >1 branches, or opened via Command Palette)
			a.renderBranchTreePill(c, p)

			// Interactive Context Window Budget & Compaction (/compact) Pill
			a.renderContextBudgetPill(c, p)

			if a.activeToolStatus != "" {
				ui.Row(c).MaxWidth(220).MinWidth(0).Padding(2, 8).Radius(RadiusPill).
					Background(p.AccentSoft).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.Spinner(c).Size(11, 11)
					ui.Text(c, a.activeToolStatus).Font(FontMono).FontSize(FontCaption).TextColor(p.Accent).
						MinWidth(0).SingleLine().Ellipsis("...")
				})
			}

			ui.Spacer(c)

			if mcpTools := a.mgr.MCPActiveToolCount(); mcpTools > 0 {
				mb := ui.ButtonBase(c).Height(26).Padding(0, 8).Radius(RadiusSM).
					Border(1, p.BorderSubtle).Gap(4).AlignItems(ui.Center).Shrink(0).
					Tooltip(fmt.Sprintf("已挂载 %d 个外部 MCP 工具，点击管理 MCP 服务", mcpTools))
				mBg := ui.Transparent
				if mb.Hovered() {
					mBg = p.BgHover
				}
				mb.Background(mBg).Children(func() {
					ui.Icon(c, IconPlug).Size(12, 12).TextColor(p.Accent)
					ui.Textf(c, "MCP · %d", mcpTools).Font(FontMono).FontFeatures("tnum").FontSize(FontCaption).TextColor(p.TextSecondary)
				})
				if mb.Clicked() {
					a.settingsModal.ActiveTab = 3
					a.settingsModal.Open = true
				}
			}

			if len(a.entries) > 0 {
				IconTipBtn(c, p, IconDownload, 28, 14, "导出当前对话为 Markdown (/export)", func() {
					a.ExportActiveSession(c, "")
				})
			}

			// Command Palette Shortcut Button (Cmd/Ctrl+K)
			ToolbarIconBtn(c, p, IconCommand, 28, 14, a.cmdPalette.Open, ui.Transparent, "打开快捷命令面板 ("+platformModLabel()+"K)", func() {
				a.cmdPalette.Toggle()
			})

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
		})

		// Workspace Project Trust Gate Banner (shown when untrusted .pi/skills exist in repo)
		if !a.trustBannerDismissed && a.mgr != nil && a.mgr.HasUntrustedProjectSkills() {
			ui.Row(c).FillWidth().MinWidth(0).Padding(7, 16).Gap(8).AlignItems(ui.Center).
				Background(p.WarnSoft).BorderWidth(0, 0, 1, 0).BorderColor(p.WarnColor.Alpha(0.35)).Children(func() {
				ui.Icon(c, IconShieldCheck).Size(14, 14).TextColor(p.WarnColor).Shrink(0)
				ui.Text(c, "检测到当前工作区包含项目级 .pi/skills 技能配置，出于安全默认未加载。确认仓库来源可信后可启用。").
					FontSize(FontSmall).TextColor(p.TextPrimary).Grow(1).MinWidth(0).SingleLine().Ellipsis("...")
				tb := ui.ButtonBase(c).Height(24).Padding(0, 10).Radius(RadiusSM).
					Background(p.Accent).AlignItems(ui.Center).Shrink(0).Label("信任此工作区")
				tb.Children(func() {
					ui.Text(c, "信任此工作区").FontSize(FontCaption).FontWeight(WeightMedium).TextColor(ui.Hex("#FFFFFF"))
				})
				if tb.Clicked() {
					a.TrustActiveWorkspace(c)
				}
				IconTipBtn(c, p, IconCross, 24, 13, "暂不加载", func() {
					a.trustBannerDismissed = true
				})
			})
		}

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
					RenderChatTurnEntryWithRevert(c, p, a.entries[i], &a.mdOpts, canFork, showTurnFooter, func(clicked *ChatEntry) {
						a.forkFromChatEntry(c, clicked)
					}, func(rev *ChatEntry) {
						a.RevertToolEntry(c, rev)
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
			atQuery, atMatches, atActive := a.activeAtAutocompleteMatches()
			if atActive {
				a.renderAtAutocompletePanel(c, p, atQuery, atMatches)
			}

			card := ui.Column(c).FillWidth().MaxWidth(760).MinWidth(0).
				Padding(10, 12, 8, 14).Radius(RadiusLG).
				Background(p.BgSurface).Gap(8)

			focused := card.FocusWithin()
			borderCol := p.BorderStrong
			shadowBlur := float32(20)
			if fileDragOver {
				borderCol = p.Accent
				shadowBlur = 30
			} else if focused {
				borderCol = p.BorderFocus
				shadowBlur = 28
			}

			card.Border(1, borderCol).
				Shadow(0, 6, shadowBlur, -4, p.ShadowSoft).
				Children(func() {
					a.renderComposerAttachmentStrip(c, p)

					placeholder := "输入指令（支持 @引用文件、!直连执行命令、粘贴/拖拽附件、/compact 压缩、/export 导出）..."
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
							if k.Mods == ui.Cmd && k.Key == ui.KeyV {
								if a.TryPasteClipboardAttachment(c) {
									return true
								}
								return false
							}
							if atActive && k.Mods == 0 {
								switch k.Key {
								case ui.KeyDown:
									if len(atMatches) > 0 {
										a.atSelectedIdx = (a.atSelectedIdx + 1) % len(atMatches)
									}
									return true
								case ui.KeyUp:
									if len(atMatches) > 0 {
										a.atSelectedIdx = (a.atSelectedIdx - 1 + len(atMatches)) % len(atMatches)
									}
									return true
								case ui.KeyEscape:
									a.atDismissedToken = "@" + atQuery
									return true
								case ui.KeyTab, ui.KeyEnter:
									if len(atMatches) > 0 && a.atSelectedIdx >= 0 && a.atSelectedIdx < len(atMatches) {
										updated := harness.ReplaceActiveAtMention(k.Text(), atMatches[a.atSelectedIdx])
										k.SetText(updated)
										a.input = updated
										a.atSelectedIdx = 0
										return true
									}
								}
							}
							if k.Key != ui.KeyEnter || k.Mods != 0 {
								return false
							}
							txt := strings.TrimSpace(k.Text())
							if txt == "" && len(a.pendingAttachments) == 0 {
								return true
							}
							k.SetText("")
							a.submitPromptWithContext(c, txt)
							return true
						})

					// Bottom Composer Toolbar — Attachment Picker + Cascader Pill + Send/Steer
					ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
						attBtn := ui.ButtonBase(c).Size(26, 26).Radius(RadiusPill).
							Border(1, p.BorderSubtle).AlignItems(ui.Center).Justify(ui.Center).
							Tooltip("添加图片或代码文件附件（支持拖拽文件或粘贴截图）").Label("添加附件")
						attBg := p.InputBg
						if attBtn.Hovered() || attBtn.Pressed() {
							attBg = p.BgHover
						}
						attBtn.Background(attBg).Children(func() {
							ui.Icon(c, IconPaperclip).Size(12.5, 12.5).TextColor(p.TextSecondary)
						})
						if attBtn.Clicked() {
							a.TriggerPickAttachments()
						}

						if prof != nil {
							a.renderUnifiedCascaderPill(c, p, prof)
						}

						ui.Spacer(c)

						hasInput := strings.TrimSpace(a.input) != "" || len(a.pendingAttachments) > 0
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
							a.submitPromptWithContext(c, a.input)
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
			ui.Text(c, "本地项目与远程服务器，同一套原生工作台").
				FontWeight(WeightSemibold).FontSize(FontHero).TextColor(p.TextPrimary)
			ui.Text(c, "支持打开本地代码仓库或连接远程 SSH 开发机。各工作区拥有独立隔离的树状会话、专属 AGENTS.md 与 Skills 上下文，并内置高危操作审批拦截。").
				FontSize(FontBody).LineHeight(1.6).TextColor(p.TextMuted).TextAlign(ui.Center)
		})

		// Interactive Quick-Start Action Pills
		ui.Row(c).Gap(8).Wrap().Justify(ui.Center).Padding(6, 0, 0, 0).Children(func() {
			renderQuickActionChip(c, p, IconCompass, "扫描当前项目架构", func() {
				a.input = "请先扫描当前工作区的目录结构与关键模块，给出一份简明的架构概览。"
			})
			renderQuickActionChip(c, p, IconServer, "切换本地 / SSH 工作区", func() {
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
