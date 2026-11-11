package harness

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	_ "github.com/sky-valley/pi/ai/providers"
	"github.com/sky-valley/pi/coding"

	"piui/internal/diag"
	"piui/internal/mcp"
	"piui/internal/store"
)

// Manager coordinates the sky-valley/pi coding.Session, EnvironmentRouter,
// SafetyGate, MCP Hub, per-profile isolated LLM providers, workspace-scoped JSONL
// session persistence, project trust gating, and remote SSH context bootstrap.
type Manager struct {
	mu                 sync.Mutex
	baseCwd            string
	router             *EnvironmentRouter
	safetyGate         *SafetyGate
	mcpHub             *mcp.Hub
	mcpConfigs         []store.MCPServerConfig
	activeTools        []agent.AgentTool
	sess               *coding.Session
	rec                *coding.SessionRecorder
	recorderErr        error
	unsub              func()
	listener           agent.Listener
	credentials        *coding.RuntimeCredentials
	models             ai.MutableModels
	activePath         string
	activeLeafID       string
	sshStatus          SSHConnectionStatus
	remoteBoot         *RemoteWorkspaceBootstrap
	forcePromptRefresh bool
	gitStatus          GitWorkspaceStatus
	workspaceFiles     []string
	filesTargetKey     string
	filesCachedAt      time.Time
	titleMu            sync.Mutex
	titleCache         map[string]cachedSessionTitle
}

type cachedSessionTitle struct {
	modNano int64
	size    int64
	title   string
}

// NewManager initializes the harness manager for the given base directory and config.
func NewManager(baseCwd string, cfg *store.AppConfig, termSink TerminalStreamSink, listener agent.Listener) (*Manager, error) {
	if baseCwd == "" {
		baseCwd, _ = os.Getwd()
	}
	baseCwd = store.ExpandLocalPath(baseCwd)

	initialTarget := store.WorkspaceTarget{
		ID:      "local-cwd",
		Name:    "Local",
		WorkDir: baseCwd,
	}
	var policy store.SafetyPolicy
	var profile *store.ProviderProfile
	var mcpServers []store.MCPServerConfig
	if cfg != nil {
		if t := cfg.ActiveTarget(); t != nil {
			initialTarget = *t
		}
		policy = cfg.SafetyPolicy
		profile = cfg.ActiveProfile()
		if len(cfg.MCPServers) > 0 {
			mcpServers = make([]store.MCPServerConfig, len(cfg.MCPServers))
			for i, ms := range cfg.MCPServers {
				mcpServers[i] = ms.Clone()
			}
		}
	}

	router := NewEnvironmentRouter(initialTarget, termSink)
	safetyGate := NewSafetyGate(router, policy, termSink)
	mcpHub := mcp.NewHub()

	m := &Manager{
		baseCwd:     baseCwd,
		router:      router,
		safetyGate:  safetyGate,
		mcpHub:      mcpHub,
		mcpConfigs:  mcpServers,
		listener:    listener,
		credentials: coding.NewRuntimeCredentials(nil),
		titleCache:  map[string]cachedSessionTitle{},
	}
	if initialTarget.IsRemote {
		m.sshStatus = SSHConnectionStatus{
			SSHHost:  initialTarget.SSHHost,
			Checking: true,
		}
	}

	if len(mcpServers) > 0 {
		mcpCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		statuses := mcpHub.Sync(mcpCtx, mcpServers, m.localMCPWorkDir())
		cancel()
		diag.Infof("MCP initial sync completed: %d servers, %d active tools", len(statuses), mcpHub.ActiveToolCount())
	}

	if err := m.InitOrResetSession(profile); err != nil {
		_ = mcpHub.Close()
		return nil, err
	}
	return m, nil
}

// Close cleanly unsubscribes listeners, shuts down MCP servers, and closes the active JSONL SessionRecorder.
func (m *Manager) Close() error {
	if m.mcpHub != nil {
		_ = m.mcpHub.Close()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unsub != nil {
		m.unsub()
		m.unsub = nil
	}
	if m.rec != nil {
		err := m.rec.Close()
		m.rec = nil
		return err
	}
	return nil
}

// Router returns the dynamic EnvironmentRouter.
func (m *Manager) Router() *EnvironmentRouter {
	return m.router
}

// SafetyGate returns the Human-in-the-Loop tool call interceptor.
func (m *Manager) SafetyGate() *SafetyGate {
	return m.safetyGate
}

// SetSafetyPolicy updates the active safety interception policy.
func (m *Manager) SetSafetyPolicy(policy store.SafetyPolicy) {
	if m.safetyGate != nil {
		m.safetyGate.SetPolicy(policy)
	}
}

// activeScopeCwd returns the isolated session storage scope key for the active workspace.
func (m *Manager) activeScopeCwd() string {
	if m.router != nil {
		if scope := strings.TrimSpace(m.router.Target().SessionScopeCwd()); scope != "" {
			return scope
		}
	}
	return m.baseCwd
}

// ActiveSessionID returns the UUID of the active session recorder, if any.
func (m *Manager) ActiveSessionID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rec != nil {
		return m.rec.ID()
	}
	return ""
}

// ActiveSessionPath returns the file path of the active JSONL session.
func (m *Manager) ActiveSessionPath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activePath
}

// RecorderWarning returns a human-readable warning when session persistence failed to initialize or flush.
func (m *Manager) RecorderWarning() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.recorderErr != nil {
		return m.recorderErr.Error()
	}
	return ""
}

// LastRecordedEntryID returns the latest JSONL entry ID written by the active SessionRecorder.
func (m *Manager) LastRecordedEntryID() string {
	m.mu.Lock()
	rec := m.rec
	m.mu.Unlock()
	if rec != nil {
		return rec.LastEntryID()
	}
	return ""
}

// SSHStatus returns the latest SSH connection health snapshot for the active workspace.
func (m *Manager) SSHStatus() SSHConnectionStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sshStatus
}

// SetSSHChecking marks the active SSH status as in-progress before launching an async probe.
func (m *Manager) SetSSHChecking(sshHost string) {
	m.mu.Lock()
	m.sshStatus = SSHConnectionStatus{
		SSHHost:   strings.TrimSpace(sshHost),
		Checking:  true,
		CheckedAt: time.Now(),
	}
	m.mu.Unlock()
}

// RefreshSSHWorkspace probes the active Remote SSH workspace (optionally tearing down any stale
// ControlMaster socket first), loads remote AGENTS.md / SKILL.md context according to workspace
// trust, and updates the active session's projected system prompt without blocking the UI lock.
func (m *Manager) RefreshSSHWorkspace(ctx context.Context, reconnect bool) SSHConnectionStatus {
	target := m.router.Target()
	if !target.IsRemote || strings.TrimSpace(target.SSHHost) == "" {
		m.mu.Lock()
		m.sshStatus = SSHConnectionStatus{}
		m.remoteBoot = nil
		m.mu.Unlock()
		return SSHConnectionStatus{}
	}

	m.SetSSHChecking(target.SSHHost)

	if reconnect {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		args := []string{"-O", "exit", "-o", "ControlPath=" + sshControlSocketPattern(), "--", target.SSHHost}
		_ = exec.CommandContext(cctx, sshBinary, args...).Run()
		cancel()
	}

	boot := LoadRemoteWorkspaceBootstrap(ctx, target.SSHHost, target.WorkDir, target.Trusted)

	m.mu.Lock()
	cur := m.router.Target()
	if cur.IsRemote && cur.SSHHost == target.SSHHost && cur.WorkDir == target.WorkDir {
		m.sshStatus = boot.Status
		m.remoteBoot = &boot
		m.forcePromptRefresh = true
	}
	st := m.sshStatus
	m.mu.Unlock()
	return st
}

// HasLocalProjectSkills reports whether a local directory contains project-scoped skills
// (.pi/skills or .agents/skills) that require workspace trust to load.
func HasLocalProjectSkills(workDir string) bool {
	workDir = store.ExpandLocalPath(strings.TrimSpace(workDir))
	if workDir == "" {
		return false
	}
	patterns := []string{
		filepath.Join(workDir, ".pi", "skills", "*", "SKILL.md"),
		filepath.Join(workDir, ".pi", "skills", "*.md"),
		filepath.Join(workDir, ".agents", "skills", "*", "SKILL.md"),
	}
	for _, pat := range patterns {
		matches, err := filepath.Glob(pat)
		if err != nil {
			continue
		}
		for _, match := range matches {
			if info, statErr := os.Stat(match); statErr == nil && !info.IsDir() {
				return true
			}
		}
	}
	return false
}

// HasUntrustedProjectSkills reports whether the active workspace contains project-scoped
// skills that are currently withheld because the workspace has not been trusted yet.
func (m *Manager) HasUntrustedProjectSkills() bool {
	t := m.router.Target()
	if t.Trusted {
		return false
	}
	if t.IsRemote {
		m.mu.Lock()
		has := m.sshStatus.HasProjectSkills || (m.remoteBoot != nil && m.remoteBoot.Status.HasProjectSkills)
		m.mu.Unlock()
		return has
	}
	targetCwd := m.baseCwd
	if t.WorkDir != "" {
		targetCwd = store.ExpandLocalPath(t.WorkDir)
	}
	return HasLocalProjectSkills(targetCwd)
}

// SetWorkspaceTrust updates the Trusted flag on the active workspace and refreshes
// the projected system prompt (including remote AGENTS.md / .pi/skills if on SSH)
// without resetting any in-progress conversation history.
func (m *Manager) SetWorkspaceTrust(ctx context.Context, trusted bool) {
	t := m.router.Target()
	t.Trusted = trusted
	m.router.UpdateTargetMetadata(t)

	m.mu.Lock()
	m.forcePromptRefresh = true
	m.mu.Unlock()

	if t.IsRemote && strings.TrimSpace(t.SSHHost) != "" {
		m.RefreshSSHWorkspace(ctx, false)
	}
}

// DiscoverSkills loads active SKILL.md definitions for the current workspace,
// strictly honoring workspace trust and avoiding local/remote cross-contamination.
func (m *Manager) DiscoverSkills() []coding.Skill {
	t := m.router.Target()
	if t.IsRemote {
		m.mu.Lock()
		boot := m.remoteBoot
		m.mu.Unlock()
		if boot != nil && boot.Status.Connected {
			out := make([]coding.Skill, len(boot.Skills))
			copy(out, boot.Skills)
			return out
		}
		if t.SSHHost != "" && CheckMultiplexStatus(context.Background(), t.SSHHost) {
			loaded := LoadRemoteWorkspaceBootstrap(context.Background(), t.SSHHost, t.WorkDir, t.Trusted)
			m.mu.Lock()
			m.sshStatus = loaded.Status
			m.remoteBoot = &loaded
			m.mu.Unlock()
			return loaded.Skills
		}
		return nil
	}

	targetCwd := m.baseCwd
	if t.WorkDir != "" {
		targetCwd = store.ExpandLocalPath(t.WorkDir)
	}
	skills, _ := coding.LoadSkillsWithTrust(targetCwd, t.Trusted)
	return skills
}

// ContextUsageStats describes the active session's context window budget and cumulative token usage.
type ContextUsageStats struct {
	ContextTokens          int
	ContextWindow          int
	Percent                float64
	SessionInputTokens     int
	SessionOutputTokens    int
	SessionCacheReadTokens int
	TurnCount              int
}

// CompactionResult summarizes the outcome of a manual /compact checkpoint operation.
type CompactionResult struct {
	BeforeTokens int
	AfterTokens  int
	Summary      string
	Projected    []ProjectedTurnMessage
	TreeSnap     *SessionTreeSnapshot
}

// EstimateContextTokens returns the usage-anchored estimated token count of the active session's conversation history.
func (m *Manager) EstimateContextTokens() int {
	return m.ContextUsageSnapshot().ContextTokens
}

// ContextUsageSnapshot computes usage-anchored context tokens, model window budget, and cumulative token totals.
func (m *Manager) ContextUsageSnapshot() ContextUsageStats {
	m.mu.Lock()
	sess := m.sess
	m.mu.Unlock()
	if sess == nil {
		return ContextUsageStats{}
	}

	msgs := sess.History()
	win := 200000
	if sess.Model != nil && sess.Model.ContextWindow > 0 {
		win = sess.Model.ContextWindow
	}

	var stats ContextUsageStats
	stats.ContextWindow = win

	lastUsageIdx := -1
	var lastUsageTokens int
	for i, msg := range msgs {
		switch am := msg.(type) {
		case ai.AssistantMessage:
			accumulateAssistantUsage(&stats, &am, i, &lastUsageIdx, &lastUsageTokens)
		case *ai.AssistantMessage:
			if am != nil {
				accumulateAssistantUsage(&stats, am, i, &lastUsageIdx, &lastUsageTokens)
			}
		}
	}

	if lastUsageIdx >= 0 && lastUsageTokens > 0 {
		total := lastUsageTokens
		for _, msg := range msgs[lastUsageIdx+1:] {
			total += coding.EstimateMessageTokens(msg)
		}
		stats.ContextTokens = total
	} else {
		stats.ContextTokens = coding.EstimateContextTokens(msgs)
	}

	if win > 0 && stats.ContextTokens > 0 {
		stats.Percent = float64(stats.ContextTokens) * 100.0 / float64(win)
	}
	return stats
}

func accumulateAssistantUsage(stats *ContextUsageStats, am *ai.AssistantMessage, idx int, lastIdx *int, lastTokens *int) {
	stats.TurnCount++
	stats.SessionInputTokens += am.Usage.Input
	stats.SessionOutputTokens += am.Usage.Output
	stats.SessionCacheReadTokens += am.Usage.CacheRead
	if am.StopReason == ai.StopAborted || am.StopReason == ai.StopError {
		return
	}
	tot := am.Usage.TotalTokens
	if tot <= 0 {
		tot = am.Usage.Input + am.Usage.Output + am.Usage.CacheRead + am.Usage.CacheWrite
	}
	if tot > 0 {
		*lastIdx = idx
		*lastTokens = tot
	}
}

// WorkspaceFiles returns the cached relative file list for the active workspace (if indexed).
func (m *Manager) WorkspaceFiles() []string {
	t := m.router.Target()
	key := t.SessionScopeCwd()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.filesTargetKey != key || len(m.workspaceFiles) == 0 {
		return nil
	}
	cp := make([]string, len(m.workspaceFiles))
	copy(cp, m.workspaceFiles)
	return cp
}

// WorkspaceFilesFresh reports whether the active workspace's file cache was populated within maxAge.
func (m *Manager) WorkspaceFilesFresh(maxAge time.Duration) bool {
	t := m.router.Target()
	key := t.SessionScopeCwd()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filesTargetKey == key && len(m.workspaceFiles) > 0 && time.Since(m.filesCachedAt) < maxAge
}

// RefreshWorkspaceFiles indexes relative file paths in the active Local or SSH workspace and updates the cache.
func (m *Manager) RefreshWorkspaceFiles(ctx context.Context) ([]string, error) {
	t := m.router.Target()
	key := t.SessionScopeCwd()
	files, err := ListWorkspaceFiles(ctx, t, defaultMaxIndexedFiles)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.router.Target().SessionScopeCwd() == key {
		m.workspaceFiles = files
		m.filesTargetKey = key
		m.filesCachedAt = time.Now()
	}
	m.mu.Unlock()
	return files, nil
}

// GitStatus returns the latest cached Git status for the active workspace.
func (m *Manager) GitStatus() GitWorkspaceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gitStatus
}

// RefreshGitStatus probes the active Local or Remote SSH workspace's Git branch and dirty files.
func (m *Manager) RefreshGitStatus(ctx context.Context) GitWorkspaceStatus {
	t := m.router.Target()
	key := t.SessionScopeCwd()
	st := ProbeGitStatus(ctx, t)
	m.mu.Lock()
	if m.router.Target().SessionScopeCwd() == key {
		m.gitStatus = st
	}
	m.mu.Unlock()
	return st
}

// GetFileMutation returns the recorded pre/post snapshot and unified diff for an edit/write tool call.
func (m *Manager) GetFileMutation(toolCallID string) (*FileMutationSnapshot, bool) {
	if m.router == nil || m.router.Changes() == nil {
		return nil, false
	}
	return m.router.Changes().Get(toolCallID)
}

// RevertFileMutation reverts the file modified or created by toolCallID back to its pre-call state.
func (m *Manager) RevertFileMutation(ctx context.Context, toolCallID string) (*FileMutationSnapshot, error) {
	if m.router == nil || m.router.Changes() == nil {
		return nil, fmt.Errorf("变更追踪器不可用")
	}
	return m.router.Changes().RevertMutation(ctx, toolCallID)
}

// ExecDirectBash executes an explicit user "! <command>" directly on the active Local or Remote SSH workspace.
func (m *Manager) ExecDirectBash(ctx context.Context, command string, onUpdate func(partial string)) (string, int64, error) {
	if m.router == nil {
		return "", 0, fmt.Errorf("工作区路由器未初始化")
	}
	return m.router.ExecDirectBash(ctx, command, onUpdate)
}

func (m *Manager) localMCPWorkDir() string {
	if m.router != nil {
		t := m.router.Target()
		if !t.IsRemote && strings.TrimSpace(t.WorkDir) != "" {
			return store.ExpandLocalPath(t.WorkDir)
		}
	}
	return m.baseCwd
}

func (m *Manager) buildSessionTools(targetCwd string) []agent.AgentTool {
	tools := m.router.BuildTools(targetCwd)
	if m.mcpHub != nil {
		mcpTools := m.mcpHub.BuildAgentTools()
		if len(mcpTools) > 0 {
			tools = append(tools, mcpTools...)
		}
	}
	return tools
}

// SyncMCPServers reconciles active MCP stdio/HTTP connections and hot-updates the current session's
// available tool set and system prompt without resetting conversation history.
func (m *Manager) SyncMCPServers(ctx context.Context, servers []store.MCPServerConfig) []mcp.ServerStatus {
	if m.mcpHub == nil {
		return nil
	}
	workDir := m.localMCPWorkDir()
	statuses := m.mcpHub.Sync(ctx, servers, workDir)

	m.mu.Lock()
	m.mcpConfigs = make([]store.MCPServerConfig, len(servers))
	for i, ms := range servers {
		m.mcpConfigs[i] = ms.Clone()
	}
	target := m.router.Target()
	targetCwd := m.baseCwd
	if !target.IsRemote && target.WorkDir != "" {
		targetCwd = store.ExpandLocalPath(target.WorkDir)
	}
	allTools := m.buildSessionTools(targetCwd)
	m.activeTools = allTools
	if m.sess != nil && m.sess.Agent != nil {
		m.sess.Agent.SetTools(allTools)
	}
	m.forcePromptRefresh = true
	m.mu.Unlock()

	diag.Infof("MCP synced: %d servers configured, %d tools active", len(statuses), m.mcpHub.ActiveToolCount())
	return statuses
}

// MCPStatuses returns connection health and discovered tool specs for all configured MCP servers.
func (m *Manager) MCPStatuses() []mcp.ServerStatus {
	if m.mcpHub == nil {
		return nil
	}
	return m.mcpHub.Statuses()
}

// MCPActiveToolCount returns the total number of live MCP tools currently exposed to the Agent.
func (m *Manager) MCPActiveToolCount() int {
	if m.mcpHub == nil {
		return 0
	}
	return m.mcpHub.ActiveToolCount()
}

// ProbeMCPServer runs a standalone connection & tool-discovery check for a candidate MCP server config.
func (m *Manager) ProbeMCPServer(ctx context.Context, srv store.MCPServerConfig) mcp.ServerStatus {
	return mcp.ProbeServer(ctx, srv, m.localMCPWorkDir())
}

func allWireStreamsMap() map[ai.Api]ai.ProviderStreams {
	allStreams := map[ai.Api]ai.ProviderStreams{}
	for _, api := range []ai.Api{
		ai.APIOpenAICompletions,
		ai.APIOpenAIResponses,
		ai.APIAnthropicMessages,
	} {
		if ap, ok := ai.GetApiProvider(api); ok {
			allStreams[api] = ai.ProviderStreams{
				Stream:         ap.Stream,
				StreamSimple:   ap.StreamSimple,
				FetchDeferred:  ap.FetchDeferred,
				CancelDeferred: ap.CancelDeferred,
			}
		}
	}
	return allStreams
}

// ensureMultiAPIProviders upgrades built-in providers (such as "openai" and "anthropic")
// in the MutableModels runtime so that they support all wire formats (including
// "openai-completions", which is omitted from the default "openai" catalog in pi v1.5.29).
func ensureMultiAPIProviders(modelsRuntime ai.MutableModels, extraModel *ai.Model) {
	if modelsRuntime == nil {
		return
	}

	allStreams := allWireStreamsMap()
	if len(allStreams) == 0 {
		return
	}

	targetProviders := []string{"openai", "anthropic"}
	if extraModel != nil && extraModel.Provider != "" {
		found := false
		for _, id := range targetProviders {
			if id == extraModel.Provider {
				found = true
				break
			}
		}
		if !found {
			targetProviders = append(targetProviders, extraModel.Provider)
		}
	}

	for _, pid := range targetProviders {
		orig := modelsRuntime.GetProvider(pid)
		if orig == nil {
			continue
		}
		apiMap := make(map[ai.Api]ai.ProviderStreams, len(allStreams)+2)
		for k, v := range allStreams {
			apiMap[k] = v
		}
		origModels := orig.GetModels()
		for _, mod := range origModels {
			if mod == nil {
				continue
			}
			if _, exists := apiMap[mod.Api]; !exists {
				if ap, ok := ai.GetApiProvider(mod.Api); ok {
					apiMap[mod.Api] = ai.ProviderStreams{
						Stream:         ap.Stream,
						StreamSimple:   ap.StreamSimple,
						FetchDeferred:  ap.FetchDeferred,
						CancelDeferred: ap.CancelDeferred,
					}
				}
			}
		}

		mergedModels := make([]*ai.Model, 0, len(origModels)+1)
		seenCustom := false
		for _, mod := range origModels {
			if extraModel != nil && extraModel.Provider == pid && mod != nil && mod.ID == extraModel.ID {
				mergedModels = append(mergedModels, extraModel)
				seenCustom = true
			} else {
				mergedModels = append(mergedModels, mod)
			}
		}
		if extraModel != nil && extraModel.Provider == pid && !seenCustom {
			mergedModels = append(mergedModels, extraModel)
		}

		modelsRuntime.SetProvider(ai.CreateProvider(ai.CreateProviderOptions{
			ID:           orig.ID(),
			Name:         orig.Name(),
			BaseURL:      orig.BaseURL(),
			Headers:      orig.Headers(),
			Auth:         orig.Auth(),
			Models:       mergedModels,
			FilterModels: orig.FilterModels,
			APIByApi:     apiMap,
		}))
	}
}

func sanitizeProfileProviderID(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// buildModelAndRuntime resolves a ProviderProfile into an isolated *ai.Model and ai.MutableModels
// runtime so multiple custom endpoints never overwrite each other's BaseURL or API key.
func (m *Manager) buildModelAndRuntime(profile *store.ProviderProfile) (*ai.Model, ai.MutableModels) {
	if m.models == nil {
		m.models = ai.BuiltinModels(&ai.CreateModelsOptions{
			Credentials: m.credentials,
		})
		ensureMultiAPIProviders(m.models, nil)
	}

	if profile == nil {
		model, _ := coding.ResolveModel(coding.DefaultModelSpec)
		ensureMultiAPIProviders(m.models, model)
		return model, m.models
	}

	var baseProviderID string
	var apiShape ai.Api
	var defaultBaseURL string
	var envBaseURLKey string

	switch profile.Format {
	case store.FormatAnthropicMessages:
		baseProviderID = "anthropic"
		apiShape = ai.APIAnthropicMessages
		defaultBaseURL = "https://api.anthropic.com"
		envBaseURLKey = "ANTHROPIC_BASE_URL"
	case store.FormatOpenAIResponses:
		baseProviderID = "openai"
		apiShape = ai.APIOpenAIResponses
		defaultBaseURL = "https://api.openai.com/v1"
		envBaseURLKey = "OPENAI_BASE_URL"
	default:
		baseProviderID = "openai"
		apiShape = ai.APIOpenAICompletions
		defaultBaseURL = "https://api.openai.com/v1"
		envBaseURLKey = "OPENAI_BASE_URL"
	}

	providerID := baseProviderID
	if slug := sanitizeProfileProviderID(profile.ID); slug != "" {
		providerID = "piui-" + slug
	}

	modelID := strings.TrimSpace(profile.ModelID)
	if modelID == "" {
		if baseProviderID == "anthropic" {
			modelID = "claude-sonnet-4-5"
		} else {
			modelID = "gpt-4.1"
		}
	}

	// Inherit accurate token/context limits from the upstream catalog when modelID matches.
	var resolved *ai.Model
	if cat := ai.GetModel(baseProviderID, modelID); cat != nil {
		clone := *cat
		resolved = &clone
	} else if r, err := coding.ResolveModel(baseProviderID + "/" + modelID); err == nil && r != nil {
		clone := *r
		resolved = &clone
	} else {
		resolved = &ai.Model{
			ID:            modelID,
			Name:          modelID,
			ContextWindow: 200000,
			MaxTokens:     16384,
			Reasoning:     true,
			Input:         []string{"text", "image"},
		}
	}

	resolved.Provider = providerID
	resolved.Api = apiShape
	if base := strings.TrimSpace(profile.BaseURL); base != "" {
		resolved.BaseURL = strings.TrimRight(base, "/")
	} else if envBase := strings.TrimSpace(os.Getenv(envBaseURLKey)); envBase != "" {
		resolved.BaseURL = strings.TrimRight(envBase, "/")
	} else {
		resolved.BaseURL = defaultBaseURL
	}

	if key := strings.TrimSpace(profile.APIKey); key != "" {
		m.credentials.SetRuntimeApiKey(providerID, key)
	} else {
		m.credentials.RemoveRuntimeApiKey(providerID)
	}

	if providerID != baseProviderID {
		if orig := m.models.GetProvider(baseProviderID); orig != nil {
			displayName := strings.TrimSpace(profile.Name)
			if displayName == "" {
				displayName = orig.Name()
			}
			var existingModels []*ai.Model
			if prev := m.models.GetProvider(providerID); prev != nil {
				for _, pm := range prev.GetModels() {
					if pm != nil && pm.ID != resolved.ID {
						existingModels = append(existingModels, pm)
					}
				}
			}
			existingModels = append(existingModels, resolved)
			m.models.SetProvider(ai.CreateProvider(ai.CreateProviderOptions{
				ID:           providerID,
				Name:         displayName,
				BaseURL:      resolved.BaseURL,
				Headers:      orig.Headers(),
				Auth:         orig.Auth(),
				Models:       existingModels,
				FilterModels: orig.FilterModels,
				APIByApi:     allWireStreamsMap(),
			}))
			return resolved, m.models
		}
	}

	ensureMultiAPIProviders(m.models, resolved)
	return resolved, m.models
}

// InitOrResetSession creates a fresh coding.Session wired to the active workspace and starts a new JSONL recorder.
func (m *Manager) InitOrResetSession(profile *store.ProviderProfile) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initSessionLocked(profile, nil)
}

// SwitchWorkspace cleanly transitions the Manager to a different Local or Remote SSH workspace,
// closing the previous workspace's recorder and initializing an isolated session for the new workspace.
func (m *Manager) SwitchWorkspace(target store.WorkspaceTarget, profile *store.ProviderProfile) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.resetWorkspaceStateLocked(target)
	return m.initSessionLocked(profile, nil)
}

// SwitchWorkspaceAndResumeSession atomically switches the active workspace target and opens a saved
// JSONL session belonging to that workspace without creating an empty intermediate session file.
func (m *Manager) SwitchWorkspaceAndResumeSession(
	target store.WorkspaceTarget,
	sessionPath string,
	profile *store.ProviderProfile,
) ([]ProjectedTurnMessage, *SessionTreeSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tree, err := coding.LoadSessionTree(sessionPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load session tree: %w", err)
	}
	rec, err := coding.ResumeSession(sessionPath)
	if err != nil {
		return nil, nil, fmt.Errorf("resume session recorder: %w", err)
	}

	m.resetWorkspaceStateLocked(target)
	if err := m.initSessionLocked(profile, rec); err != nil {
		_ = rec.Close()
		return nil, nil, err
	}

	proj := tree.BuildProjection()
	m.sess.LoadBranch(proj)
	m.activePath = sessionPath
	m.activeLeafID = tree.LeafID

	turnMsgs := ExtractProjectedTurnMessages(tree, proj)
	snap := BuildSessionTreeSnapshot(tree, m.activeLeafID)
	return turnMsgs, snap, nil
}

func (m *Manager) resetWorkspaceStateLocked(target store.WorkspaceTarget) {
	m.router.SetTargetQuiet(target)
	m.remoteBoot = nil
	m.gitStatus = GitWorkspaceStatus{}
	m.workspaceFiles = nil
	m.filesTargetKey = ""
	m.filesCachedAt = time.Time{}
	if target.IsRemote {
		m.sshStatus = SSHConnectionStatus{
			SSHHost:   target.SSHHost,
			Checking:  true,
			CheckedAt: time.Now(),
		}
	} else {
		m.sshStatus = SSHConnectionStatus{}
	}
	m.forcePromptRefresh = false
}

func (m *Manager) initSessionLocked(profile *store.ProviderProfile, existingRec *coding.SessionRecorder) error {
	if m.unsub != nil {
		m.unsub()
		m.unsub = nil
	}
	if m.rec != nil && m.rec != existingRec {
		_ = m.rec.Close()
		m.rec = nil
	}
	if m.safetyGate != nil {
		m.safetyGate.ClearTrusted()
	}
	if m.router != nil {
		m.router.ClearPinnedTargets()
		if m.router.Changes() != nil {
			m.router.Changes().Clear()
		}
	}

	model, modelsRuntime := m.buildModelAndRuntime(profile)
	m.models = modelsRuntime

	thinking := agent.ThinkMedium
	if profile != nil && profile.ThinkingLevel != "" {
		thinking = agent.ThinkingLevel(profile.ThinkingLevel)
	}

	target := m.router.Target()
	targetCwd := m.baseCwd
	if !target.IsRemote && target.WorkDir != "" {
		targetCwd = store.ExpandLocalPath(target.WorkDir)
	}

	var beforeHook agent.BeforeToolCallFunc
	if m.safetyGate != nil {
		beforeHook = m.safetyGate.BeforeToolCall
	}

	tools := m.buildSessionTools(targetCwd)
	m.activeTools = tools
	opts := coding.SessionOptions{
		Model:          model,
		Cwd:            targetCwd,
		Tools:          tools,
		ThinkingLevel:  thinking,
		TrustProject:   target.Trusted,
		Models:         modelsRuntime,
		Compaction:     coding.DefaultCompactionSettings(),
		BeforeToolCall: beforeHook,
	}

	sess, err := coding.NewSession(opts)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	m.installWorkspacePromptProjection(sess)

	m.recorderErr = nil
	rec := existingRec
	if rec == nil {
		scopeCwd := m.activeScopeCwd()
		started, startErr := coding.StartSession(scopeCwd, model, string(thinking))
		if startErr == nil {
			rec = started
		} else {
			m.recorderErr = fmt.Errorf("会话持久化记录器创建失败（当前对话仅保存在内存中，不会落盘）: %w", startErr)
		}
	}
	if rec != nil {
		// Register recorder BEFORE UI listener so rec.LastEntryID() is updated
		// synchronously before UI handleAgentEvent reads it on EvMessageEnd.
		sess.Record(rec)
		m.rec = rec
		m.activePath = rec.Path()
		m.activeLeafID = rec.LastEntryID()
	} else {
		m.rec = nil
		m.activePath = ""
		m.activeLeafID = ""
	}

	if m.listener != nil {
		m.unsub = sess.Subscribe(m.listener)
	}
	m.sess = sess
	return nil
}

// installWorkspacePromptProjection wraps sess.Agent.TransformContext so that:
//  1. Remote SSH workspaces receive an authentic structured system prompt containing
//     the remote Cwd, remote OS/host metadata, remote AGENTS.md/CLAUDE.md, and remote
//     trusted SKILL.md files (instead of local host paths/files).
//  2. Mid-session workspace trust updates (SetWorkspaceTrust) immediately project the
//     updated skills/context without discarding the active conversation.
func (m *Manager) installWorkspacePromptProjection(sess *coding.Session) {
	if sess == nil || sess.Agent == nil {
		return
	}

	previous := sess.Agent.TransformContext
	sess.Agent.TransformContext = func(ctx context.Context, messages []agent.AgentMessage) []agent.AgentMessage {
		transformed := messages
		if previous != nil {
			transformed = previous(ctx, messages)
		}

		m.mu.Lock()
		tools := make([]agent.AgentTool, len(m.activeTools))
		copy(tools, m.activeTools)
		m.mu.Unlock()

		toolNames := make([]string, 0, len(tools))
		toolGuidelines := make(map[string][]string, len(tools))
		snippets := maps.Clone(coding.ToolSnippets)
		if snippets == nil {
			snippets = map[string]string{}
		}
		snippets["workspace_probe"] = "Inspect host OS, CPU/RAM/disk, listening ports, and git status"

		for _, tl := range tools {
			toolNames = append(toolNames, tl.Name)
			if _, builtin := snippets[tl.Name]; !builtin && tl.Description != "" {
				snippets[tl.Name] = tl.Description
			}
			if len(tl.PromptGuidelines) > 0 {
				cp := make([]string, len(tl.PromptGuidelines))
				copy(cp, tl.PromptGuidelines)
				toolGuidelines[tl.Name] = cp
			}
		}

		projectedPrompt, ok := m.buildProjectedWorkspacePrompt(ctx, toolNames, snippets, toolGuidelines)
		if !ok {
			return transformed
		}

		head := ai.NewSystemText(projectedPrompt, time.Now().UnixMilli())
		if current, found := ai.GetCurrentSystemMessage(transformed); found {
			head.ToolsAdded = current.ToolsAdded
			head.Timestamp = current.Timestamp
		}
		out := make([]agent.AgentMessage, 0, len(transformed)+1)
		out = append(out, head)
		for _, msg := range transformed {
			if msg != nil && msg.MessageRole() == ai.RoleSystem {
				continue
			}
			out = append(out, msg)
		}
		return out
	}
}

func (m *Manager) buildProjectedWorkspacePrompt(
	ctx context.Context,
	toolNames []string,
	snippets map[string]string,
	toolGuidelines map[string][]string,
) (string, bool) {
	target := m.router.Target()

	m.mu.Lock()
	boot := m.remoteBoot
	forceLocal := m.forcePromptRefresh
	m.mu.Unlock()

	if target.IsRemote {
		// Lazy bootstrap on first prompt turn if not yet loaded or trust changed.
		if boot == nil && strings.TrimSpace(target.SSHHost) != "" {
			loaded := LoadRemoteWorkspaceBootstrap(ctx, target.SSHHost, target.WorkDir, target.Trusted)
			m.mu.Lock()
			if cur := m.router.Target(); cur.IsRemote && cur.SSHHost == target.SSHHost && cur.WorkDir == target.WorkDir {
				m.sshStatus = loaded.Status
				m.remoteBoot = &loaded
				boot = &loaded
			}
			m.mu.Unlock()
		}

		remoteCwd := strings.TrimSpace(target.WorkDir)
		if remoteCwd == "" {
			remoteCwd = "~"
		}
		var ctxFiles []coding.ContextFile
		var skills []coding.Skill
		var envAddendum string

		if boot != nil && boot.Status.Connected {
			if boot.Status.ResolvedWorkDir != "" {
				remoteCwd = boot.Status.ResolvedWorkDir
			}
			ctxFiles = boot.ContextFiles
			skills = boot.Skills
			osDesc := boot.Status.RemoteOS
			if osDesc == "" {
				osDesc = "POSIX/Linux"
			}
			envAddendum = fmt.Sprintf(
				"Active Workspace: Remote SSH (%s) — host %q, remote OS %q.\nAll file and shell tools (read, write, edit, bash, ls, find, grep, workspace_probe) execute directly on this remote SSH host.",
				target.ShortName(),
				target.SSHHost,
				osDesc,
			)
		} else {
			errNote := ""
			if boot != nil && boot.Status.Error != "" {
				errNote = " (last connection check error: " + boot.Status.Error + ")"
			}
			envAddendum = fmt.Sprintf(
				"Active Workspace: Remote SSH (%s) — host %q%s.\nAll file and shell tools route to this remote SSH host.",
				target.ShortName(),
				target.SSHHost,
				errNote,
			)
		}

		prompt, err := coding.BuildSystemPrompt(coding.BuildSystemPromptOptions{
			SelectedTools:      toolNames,
			ToolSnippets:       snippets,
			ToolGuidelines:     toolGuidelines,
			AppendSystemPrompt: envAddendum,
			Cwd:                remoteCwd,
			ContextFiles:       ctxFiles,
			Skills:             skills,
		})
		if err != nil {
			return "", false
		}
		return prompt, true
	}

	if !forceLocal && m.MCPActiveToolCount() == 0 {
		return "", false
	}

	localCwd := m.baseCwd
	if target.WorkDir != "" {
		localCwd = store.ExpandLocalPath(target.WorkDir)
	}
	skills, _ := coding.LoadSkillsWithTrust(localCwd, target.Trusted)
	prompt, err := coding.BuildSystemPrompt(coding.BuildSystemPromptOptions{
		SelectedTools:  toolNames,
		ToolSnippets:   snippets,
		ToolGuidelines: toolGuidelines,
		Cwd:            localCwd,
		ContextFiles:   coding.LoadProjectContextFiles(localCwd),
		Skills:         skills,
	})
	if err != nil {
		return "", false
	}
	return prompt, true
}

// ApplyProfile updates the active session's model, thinking level, and runtime API key in place.
func (m *Manager) ApplyProfile(profile *store.ProviderProfile) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.sess == nil {
		return m.initSessionLocked(profile, nil)
	}

	model, _ := m.buildModelAndRuntime(profile)
	m.sess.SetModel(model)
	if profile != nil && profile.ThinkingLevel != "" {
		m.sess.SetThinkingLevel(agent.ThinkingLevel(profile.ThinkingLevel))
	}
	return nil
}

// SwitchTarget hot-swaps the active WorkspaceTarget on the router without dropping the conversation.
func (m *Manager) SwitchTarget(t store.WorkspaceTarget) {
	m.router.SetTarget(t)
}

// ListSavedSessions lists JSONL sessions stored for the active workspace (newest first).
func (m *Manager) ListSavedSessions() []coding.SessionInfo {
	return coding.ListSessions(m.activeScopeCwd(), strings.TrimSpace(os.Getenv("PIUI_SESSION_DIR")))
}

// ListSavedSessionSummaries lists JSONL sessions for the active workspace enriched with
// their first user prompt title and formatted "YYYY-MM-DD HH:MM" timestamp.
func (m *Manager) ListSavedSessionSummaries() []SavedSessionSummary {
	return m.ListSavedSessionSummariesWithTitles(nil)
}

func (m *Manager) cachedExtractTitle(sessionPath string) string {
	info, err := os.Stat(sessionPath)
	if err != nil {
		return ExtractSessionTitle(sessionPath)
	}
	modNano := info.ModTime().UnixNano()
	size := info.Size()

	m.titleMu.Lock()
	if m.titleCache != nil {
		if hit, ok := m.titleCache[sessionPath]; ok && hit.modNano == modNano && hit.size == size && hit.title != "" {
			m.titleMu.Unlock()
			return hit.title
		}
	}
	m.titleMu.Unlock()

	title := ExtractSessionTitle(sessionPath)

	m.titleMu.Lock()
	if m.titleCache == nil {
		m.titleCache = map[string]cachedSessionTitle{}
	}
	m.titleCache[sessionPath] = cachedSessionTitle{
		modNano: modNano,
		size:    size,
		title:   title,
	}
	m.titleMu.Unlock()
	return title
}

// ListWorkspaceSessionSummaries lists JSONL sessions belonging to target (Local or Remote SSH),
// enriched with cached/custom conversation titles and compact relative age strings ("4m", "2d").
func (m *Manager) ListWorkspaceSessionSummaries(target store.WorkspaceTarget, titleOverrides map[string]string) []SavedSessionSummary {
	scopeCwd := strings.TrimSpace(target.SessionScopeCwd())
	if scopeCwd == "" {
		scopeCwd = m.baseCwd
	}
	raw := coding.ListSessions(scopeCwd, strings.TrimSpace(os.Getenv("PIUI_SESSION_DIR")))
	now := time.Now()
	out := make([]SavedSessionSummary, 0, len(raw))
	for _, s := range raw {
		title := ""
		if titleOverrides != nil {
			title = strings.TrimSpace(titleOverrides[s.ID])
		}
		if title == "" {
			title = m.cachedExtractTitle(s.Path)
		}

		var updatedAt time.Time
		if info, statErr := os.Stat(s.Path); statErr == nil {
			updatedAt = info.ModTime()
		} else if parsed, pErr := time.Parse(time.RFC3339, strings.TrimSpace(s.Timestamp)); pErr == nil {
			updatedAt = parsed
		}

		out = append(out, SavedSessionSummary{
			ID:           s.ID,
			Path:         s.Path,
			Title:        title,
			Timestamp:    FormatSessionTimestamp(s.Timestamp),
			RelativeTime: FormatRelativeAge(updatedAt, now),
			UpdatedAt:    updatedAt,
			Messages:     s.Messages,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return false
	})
	return out
}

// ListSavedSessionSummariesWithTitles lists JSONL sessions for the active workspace,
// applying any user-defined custom titles from titleOverrides keyed by session ID.
func (m *Manager) ListSavedSessionSummariesWithTitles(titleOverrides map[string]string) []SavedSessionSummary {
	if m.router != nil {
		return m.ListWorkspaceSessionSummaries(m.router.Target(), titleOverrides)
	}
	return m.ListWorkspaceSessionSummaries(store.WorkspaceTarget{WorkDir: m.baseCwd}, titleOverrides)
}

// DeleteSavedSession deletes a saved JSONL session file from disk.
// If the deleted session is currently open, Manager resets cleanly to a fresh session.
func (m *Manager) DeleteSavedSession(sessionPath string, profile *store.ProviderProfile) (resetActive bool, err error) {
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" {
		return false, fmt.Errorf("empty session path")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	isActive := m.activePath != "" && filepath.Clean(m.activePath) == filepath.Clean(sessionPath)
	if isActive {
		if m.rec != nil {
			_ = m.rec.Close()
			m.rec = nil
		}
		m.activePath = ""
		m.activeLeafID = ""
	}

	if rmErr := os.Remove(sessionPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return false, rmErr
	}

	if isActive {
		if initErr := m.initSessionLocked(profile, nil); initErr != nil {
			return true, initErr
		}
		return true, nil
	}
	return false, nil
}

// ResumeSavedSession loads a historical JSONL session into the current session and continues recording to it.
func (m *Manager) ResumeSavedSession(path string, profile *store.ProviderProfile) ([]agent.AgentMessage, error) {
	projected, _, err := m.ResumeSavedSessionProjected(path, profile)
	if err != nil {
		return nil, err
	}
	msgs := make([]agent.AgentMessage, 0, len(projected))
	for _, pm := range projected {
		msgs = append(msgs, pm.Message)
	}
	return msgs, nil
}

// ResumeSavedSessionProjected loads a historical JSONL session and returns both per-message
// tree provenance (ProjectedTurnMessage) and the visual SessionTreeSnapshot.
func (m *Manager) ResumeSavedSessionProjected(path string, profile *store.ProviderProfile) ([]ProjectedTurnMessage, *SessionTreeSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tree, err := coding.LoadSessionTree(path)
	if err != nil {
		return nil, nil, fmt.Errorf("load session tree: %w", err)
	}
	rec, err := coding.ResumeSession(path)
	if err != nil {
		return nil, nil, fmt.Errorf("resume session recorder: %w", err)
	}

	if err := m.initSessionLocked(profile, rec); err != nil {
		_ = rec.Close()
		return nil, nil, err
	}

	proj := tree.BuildProjection()
	m.sess.LoadBranch(proj)
	m.activePath = path
	m.activeLeafID = tree.LeafID

	turnMsgs := ExtractProjectedTurnMessages(tree, proj)
	snap := BuildSessionTreeSnapshot(tree, m.activeLeafID)
	return turnMsgs, snap, nil
}

// SessionTreeSnapshot parses the active session's JSONL file and returns its visual branch tree.
// Returns nil if the active session has not yet flushed any messages to disk.
func (m *Manager) SessionTreeSnapshot() *SessionTreeSnapshot {
	m.mu.Lock()
	path := m.activePath
	leafID := m.activeLeafID
	if m.rec != nil && m.rec.LastEntryID() != "" {
		leafID = m.rec.LastEntryID()
	}
	m.mu.Unlock()

	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	tree, err := coding.LoadSessionTree(path)
	if err != nil {
		return nil
	}
	return BuildSessionTreeSnapshot(tree, leafID)
}

// CheckoutBranch switches the active conversation context and recorder parent pointer
// to the specified entryID in the current JSONL session tree.
// Passing entryID == "" rewinds to the root (before the first message in the session).
func (m *Manager) CheckoutBranch(entryID string, profile *store.ProviderProfile) ([]ProjectedTurnMessage, *SessionTreeSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.activePath == "" {
		return nil, nil, fmt.Errorf("no active session file to branch from")
	}
	tree, err := coding.LoadSessionTree(m.activePath)
	if err != nil {
		return nil, nil, fmt.Errorf("load session tree: %w", err)
	}

	if m.sess == nil {
		rec, err := coding.ResumeSession(m.activePath)
		if err != nil {
			return nil, nil, fmt.Errorf("resume session recorder: %w", err)
		}
		if err := m.initSessionLocked(profile, rec); err != nil {
			_ = rec.Close()
			return nil, nil, err
		}
	}

	if entryID == "" {
		if err := m.sess.Reset(); err != nil {
			return nil, nil, err
		}
		if m.rec != nil {
			m.rec.ForkFrom("")
		}
		m.activeLeafID = ""
		return nil, BuildSessionTreeSnapshot(tree, ""), nil
	}

	proj := tree.BuildProjection(entryID)
	m.sess.LoadBranch(proj)
	if m.rec != nil {
		m.rec.ForkFrom(entryID)
	}
	m.activeLeafID = entryID

	turnMsgs := ExtractProjectedTurnMessages(tree, proj)
	snap := BuildSessionTreeSnapshot(tree, entryID)
	return turnMsgs, snap, nil
}

// Run executes a user prompt turn (expanding @file references if present).
func (m *Manager) Run(ctx context.Context, prompt string) (*coding.RunResult, error) {
	return m.RunWithAttachments(ctx, prompt, nil)
}

// RunWithAttachments executes a user prompt turn with optional multimodal images, file attachments,
// and automatic workspace `@path` reference context resolution.
func (m *Manager) RunWithAttachments(ctx context.Context, prompt string, attachments []PromptAttachment) (*coding.RunResult, error) {
	m.mu.Lock()
	sess := m.sess
	m.mu.Unlock()
	if sess == nil {
		return nil, fmt.Errorf("session not initialized")
	}

	target := m.router.Target()
	actualPrompt, attImages := BuildAttachmentPayload(prompt, attachments)
	actualPrompt, refImages := ExpandAtMentionsContext(ctx, target, actualPrompt)

	if notice := m.router.ConsumeSwitchNotice(); notice != "" {
		actualPrompt = notice + "\n\n" + actualPrompt
	}

	allImages := make([]ai.ImageContent, 0, len(attImages)+len(refImages))
	allImages = append(allImages, attImages...)
	allImages = append(allImages, refImages...)

	res, err := sess.Run(ctx, actualPrompt, allImages...)

	m.mu.Lock()
	if m.rec != nil && m.rec.LastEntryID() != "" {
		m.activeLeafID = m.rec.LastEntryID()
	}
	m.mu.Unlock()
	return res, err
}

// Steer injects an in-flight steering instruction into the active agent loop.
func (m *Manager) Steer(prompt string) {
	m.SteerWithAttachments(prompt, nil)
}

// SteerWithAttachments injects an in-flight steering instruction (with optional images/files) into the active agent loop.
func (m *Manager) SteerWithAttachments(prompt string, attachments []PromptAttachment) {
	m.mu.Lock()
	sess := m.sess
	m.mu.Unlock()
	if sess == nil {
		return
	}
	actualPrompt, attImages := BuildAttachmentPayload(prompt, attachments)
	if notice := m.router.ConsumeSwitchNotice(); notice != "" {
		actualPrompt = notice + "\n\n" + actualPrompt
	}
	content := ai.ContentList{ai.TextContent{Text: actualPrompt}}
	for _, img := range attImages {
		content = append(content, img)
	}
	sess.Steer(ai.UserMessage{
		Content:   content,
		Timestamp: time.Now().UnixMilli(),
	})
}

// CompactSession summarizes the active conversation history into a concise structured checkpoint,
// preserves the full pre-compaction history as a branch in the JSONL session tree, and loads the
// compacted context into the active session.
func (m *Manager) CompactSession(ctx context.Context, focusHint string, profile *store.ProviderProfile) (*CompactionResult, error) {
	m.mu.Lock()
	sess := m.sess
	modelsRuntime := m.models
	m.mu.Unlock()

	if sess == nil {
		return nil, fmt.Errorf("当前会话尚未初始化")
	}

	history := sess.History()
	var nonSystem []agent.AgentMessage
	for _, msg := range history {
		if msg != nil && msg.MessageRole() != ai.RoleSystem {
			nonSystem = append(nonSystem, msg)
		}
	}
	if len(nonSystem) == 0 {
		return nil, fmt.Errorf("当前会话暂无可压缩的历史消息")
	}

	beforeTokens := m.EstimateContextTokens()
	transcriptText, readPaths, modPaths := serializeMessagesForCompaction(nonSystem)

	sysPrompt := "You are a context summarization assistant. Produce ONLY a concise structured checkpoint summary in Markdown preserving exact file paths, symbols, key decisions, completed work, and next steps."
	userReq := "Summarize the following coding conversation into a structured checkpoint (`## Goal`, `## Progress`, `## Key Decisions`, `## Next Steps`, `## Critical Context`):\n\n<conversation>\n" + transcriptText + "\n</conversation>"
	if strings.TrimSpace(focusHint) != "" {
		userReq += "\n\nAdditional user focus instructions: " + strings.TrimSpace(focusHint)
	}

	var summary string
	if modelsRuntime != nil && sess.Model != nil {
		compCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		resp := modelsRuntime.CompleteSimple(compCtx, sess.Model, ai.Context{
			SystemPrompt: sysPrompt,
			Messages: []ai.Message{
				ai.NewUserText(userReq, time.Now().UnixMilli()),
			},
		}, nil)
		cancel()
		if resp != nil && resp.ErrorMessage == "" {
			var parts []string
			for _, blk := range resp.Content {
				if tc, ok := blk.(ai.TextContent); ok && strings.TrimSpace(tc.Text) != "" {
					parts = append(parts, strings.TrimSpace(tc.Text))
				}
			}
			summary = strings.Join(parts, "\n\n")
		}
	}

	if strings.TrimSpace(summary) == "" {
		// Deterministic local checkpoint fallback when provider summary is unavailable
		summary = buildDeterministicCheckpointSummary(nonSystem)
	}

	if len(readPaths) > 0 {
		summary += "\n\n<read-files>\n" + strings.Join(readPaths, "\n") + "\n</read-files>"
	}
	if len(modPaths) > 0 {
		summary += "\n\n<modified-files>\n" + strings.Join(modPaths, "\n") + "\n</modified-files>"
	}

	nowMs := time.Now().UnixMilli()
	checkpointUser := ai.NewUserText(
		"[Compacted Context Checkpoint]\n以下是前序长对话压缩后的上下文检查点摘要，请基于此上下文继续后续开发任务：\n\n"+summary,
		nowMs,
	)
	checkpointAck := ai.NewAssistantMessageLiteral(ai.AssistantMessage{
		Content: ai.ContentList{
			ai.TextContent{Text: "✅ 已完成上下文压缩检查点：\n\n" + summary},
		},
		Api:        sess.Model.Api,
		Provider:   sess.Model.Provider,
		Model:      sess.Model.ID,
		StopReason: ai.StopStop,
		Timestamp:  nowMs + 1,
	})

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := sess.Reset(); err != nil {
		return nil, fmt.Errorf("reset session for compaction: %w", err)
	}
	sess.LoadHistory([]agent.AgentMessage{checkpointUser, *checkpointAck})

	var projected []ProjectedTurnMessage
	var treeSnap *SessionTreeSnapshot
	if m.rec != nil {
		// Fork from root ("") so all pre-compaction messages remain intact on a sibling branch in the JSONL tree!
		m.rec.ForkFrom("")
		uID := m.rec.RecordMessage(checkpointUser)
		aID := m.rec.RecordMessage(*checkpointAck)
		m.activeLeafID = aID

		if m.activePath != "" {
			if tree, err := coding.LoadSessionTree(m.activePath); err == nil {
				proj := tree.BuildProjection(aID)
				projected = ExtractProjectedTurnMessages(tree, proj)
				treeSnap = BuildSessionTreeSnapshot(tree, aID)
			}
		}
		if len(projected) == 0 {
			projected = []ProjectedTurnMessage{
				{Message: checkpointUser, EntryID: uID, ParentEntryID: ""},
				{Message: *checkpointAck, EntryID: aID, ParentEntryID: uID},
			}
		}
	} else {
		projected = []ProjectedTurnMessage{
			{Message: checkpointUser},
			{Message: *checkpointAck},
		}
	}

	afterTokens := coding.EstimateContextTokens(sess.History())
	return &CompactionResult{
		BeforeTokens: beforeTokens,
		AfterTokens:  afterTokens,
		Summary:      summary,
		Projected:    projected,
		TreeSnap:     treeSnap,
	}, nil
}

func serializeMessagesForCompaction(msgs []agent.AgentMessage) (transcript string, readFiles, modifiedFiles []string) {
	var b strings.Builder
	readSet := map[string]bool{}
	modSet := map[string]bool{}

	for _, m := range msgs {
		switch v := m.(type) {
		case ai.UserMessage:
			txt := cleanCompactionUserText(ai.ContentText(v.Content, "\n"))
			if txt != "" {
				b.WriteString("\n[User]:\n")
				b.WriteString(truncateCompactionText(txt, 2400))
				b.WriteByte('\n')
			}
		case *ai.UserMessage:
			if v != nil {
				txt := cleanCompactionUserText(ai.ContentText(v.Content, "\n"))
				if txt != "" {
					b.WriteString("\n[User]:\n")
					b.WriteString(truncateCompactionText(txt, 2400))
					b.WriteByte('\n')
				}
			}
		case ai.AssistantMessage:
			writeAssistantCompactionBlock(&b, &v, readSet, modSet)
		case *ai.AssistantMessage:
			if v != nil {
				writeAssistantCompactionBlock(&b, v, readSet, modSet)
			}
		case ai.ToolResultMessage:
			out := strings.TrimSpace(ai.ContentText(v.Content, "\n"))
			if out != "" {
				b.WriteString("[ToolResult ")
				b.WriteString(v.ToolName)
				b.WriteString("]: ")
				b.WriteString(truncateCompactionText(out, 1200))
				b.WriteByte('\n')
			}
		case *ai.ToolResultMessage:
			if v != nil {
				out := strings.TrimSpace(ai.ContentText(v.Content, "\n"))
				if out != "" {
					b.WriteString("[ToolResult ")
					b.WriteString(v.ToolName)
					b.WriteString("]: ")
					b.WriteString(truncateCompactionText(out, 1200))
					b.WriteByte('\n')
				}
			}
		}
	}

	for p := range modSet {
		modifiedFiles = append(modifiedFiles, p)
	}
	for p := range readSet {
		if !modSet[p] {
			readFiles = append(readFiles, p)
		}
	}
	return strings.TrimSpace(b.String()), readFiles, modifiedFiles
}

func writeAssistantCompactionBlock(b *strings.Builder, am *ai.AssistantMessage, readSet, modSet map[string]bool) {
	var texts []string
	var calls []string
	for _, blk := range am.Content {
		switch c := blk.(type) {
		case ai.TextContent:
			if strings.TrimSpace(c.Text) != "" {
				texts = append(texts, strings.TrimSpace(c.Text))
			}
		case ai.ToolCall:
			p, _ := c.Arguments["path"].(string)
			if p != "" {
				switch c.Name {
				case "read":
					readSet[p] = true
				case "edit", "write":
					modSet[p] = true
				}
			}
			calls = append(calls, c.Name)
		}
	}
	if len(texts) > 0 || len(calls) > 0 {
		b.WriteString("\n[Assistant")
		if len(calls) > 0 {
			b.WriteString(" tools=" + strings.Join(calls, ","))
		}
		b.WriteString("]:\n")
		if len(texts) > 0 {
			b.WriteString(truncateCompactionText(strings.Join(texts, "\n"), 2400))
			b.WriteByte('\n')
		}
	}
}

func buildDeterministicCheckpointSummary(msgs []agent.AgentMessage) string {
	var goals []string
	var assistantConclusions []string
	for _, m := range msgs {
		switch v := m.(type) {
		case ai.UserMessage:
			t := cleanCompactionUserText(ai.ContentText(v.Content, "\n"))
			if t != "" && len(goals) < 5 {
				goals = append(goals, "- "+truncateCompactionText(strings.ReplaceAll(t, "\n", " "), 180))
			}
		case ai.AssistantMessage:
			var parts []string
			for _, blk := range v.Content {
				if tc, ok := blk.(ai.TextContent); ok && strings.TrimSpace(tc.Text) != "" {
					parts = append(parts, strings.TrimSpace(tc.Text))
				}
			}
			if len(parts) > 0 {
				assistantConclusions = append(assistantConclusions, "- "+truncateCompactionText(strings.ReplaceAll(strings.Join(parts, " "), "\n", " "), 220))
			}
		case *ai.AssistantMessage:
			if v != nil {
				var parts []string
				for _, blk := range v.Content {
					if tc, ok := blk.(ai.TextContent); ok && strings.TrimSpace(tc.Text) != "" {
						parts = append(parts, strings.TrimSpace(tc.Text))
					}
				}
				if len(parts) > 0 {
					assistantConclusions = append(assistantConclusions, "- "+truncateCompactionText(strings.ReplaceAll(strings.Join(parts, " "), "\n", " "), 220))
				}
			}
		}
	}
	if len(assistantConclusions) > 5 {
		assistantConclusions = assistantConclusions[len(assistantConclusions)-5:]
	}
	var out strings.Builder
	out.WriteString("## Goal\n")
	if len(goals) > 0 {
		out.WriteString(strings.Join(goals, "\n"))
	} else {
		out.WriteString("- 延续前序会话开发任务")
	}
	out.WriteString("\n\n## Progress\n")
	if len(assistantConclusions) > 0 {
		out.WriteString(strings.Join(assistantConclusions, "\n"))
	} else {
		out.WriteString("- 已完成前序工具执行与上下文归档")
	}
	return out.String()
}

func cleanCompactionUserText(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "\n\n"+attachedContextMarkerStart); idx != -1 {
		s = strings.TrimSpace(s[:idx])
	}
	return s
}

func truncateCompactionText(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "...[truncated]"
}

// Abort immediately cancels the in-flight agent run and any active tool execution.
func (m *Manager) Abort() {
	m.mu.Lock()
	sess := m.sess
	m.mu.Unlock()
	if sess != nil {
		sess.Abort()
	}
}
