package harness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	_ "github.com/sky-valley/pi/ai/providers"
	"github.com/sky-valley/pi/coding"

	"piui/internal/store"
)

// Manager coordinates the sky-valley/pi coding.Session, EnvironmentRouter,
// SafetyGate, custom LLM provider endpoints, and JSONL session tree persistence.
type Manager struct {
	mu           sync.Mutex
	baseCwd      string
	router       *EnvironmentRouter
	safetyGate   *SafetyGate
	sess         *coding.Session
	rec          *coding.SessionRecorder
	recorderErr  error
	unsub        func()
	listener     agent.Listener
	credentials  *coding.RuntimeCredentials
	models       ai.MutableModels
	activePath   string
	activeLeafID string
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
	if cfg != nil {
		if t := cfg.ActiveTarget(); t != nil {
			initialTarget = *t
		}
		policy = cfg.SafetyPolicy
		profile = cfg.ActiveProfile()
	}

	router := NewEnvironmentRouter(initialTarget, termSink)
	safetyGate := NewSafetyGate(router, policy, termSink)

	m := &Manager{
		baseCwd:     baseCwd,
		router:      router,
		safetyGate:  safetyGate,
		listener:    listener,
		credentials: coding.NewRuntimeCredentials(nil),
	}

	if err := m.InitOrResetSession(profile); err != nil {
		return nil, err
	}
	return m, nil
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

// DiscoverSkills loads active SKILL.md definitions from the current local workspace and global config,
// plus remote SSH skills if a warm multiplexed SSH connection is active.
func (m *Manager) DiscoverSkills() []coding.Skill {
	targetCwd := m.baseCwd
	t := m.router.Target()
	if !t.IsRemote && t.WorkDir != "" {
		targetCwd = store.ExpandLocalPath(t.WorkDir)
	}
	skills, _ := coding.LoadSkillsWithTrust(targetCwd, true)
	if t.IsRemote && t.SSHHost != "" && CheckMultiplexStatus(context.Background(), t.SSHHost) {
		remoteSkills := DiscoverRemoteSkills(context.Background(), t.SSHHost, t.WorkDir)
		seen := make(map[string]bool, len(skills))
		for _, s := range skills {
			seen[s.Name] = true
		}
		for _, rs := range remoteSkills {
			if !seen[rs.Name] {
				seen[rs.Name] = true
				skills = append(skills, rs)
			}
		}
	}
	return skills
}

// EstimateContextTokens returns the estimated token count of the active session's conversation history.
func (m *Manager) EstimateContextTokens() int {
	m.mu.Lock()
	sess := m.sess
	m.mu.Unlock()
	if sess == nil {
		return 0
	}
	return coding.EstimateContextTokens(sess.History())
}

// ensureMultiAPIProviders upgrades built-in providers (such as "openai" and "anthropic")
// in the MutableModels runtime so that they support all wire formats (including
// "openai-completions", which is omitted from the default "openai" catalog in pi v1.5.29
// because built-in openai catalog entries only declare "openai-responses").
func ensureMultiAPIProviders(modelsRuntime ai.MutableModels, extraModel *ai.Model) {
	if modelsRuntime == nil {
		return
	}

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

// buildModelAndRuntime resolves a ProviderProfile into an *ai.Model and ai.Models runtime.
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

	var providerID string
	var apiShape ai.Api
	var defaultBaseURL string
	var envBaseURLKey string

	switch profile.Format {
	case store.FormatAnthropicMessages:
		providerID = "anthropic"
		apiShape = ai.APIAnthropicMessages
		defaultBaseURL = "https://api.anthropic.com"
		envBaseURLKey = "ANTHROPIC_BASE_URL"
	case store.FormatOpenAIResponses:
		providerID = "openai"
		apiShape = ai.APIOpenAIResponses
		defaultBaseURL = "https://api.openai.com/v1"
		envBaseURLKey = "OPENAI_BASE_URL"
	default:
		providerID = "openai"
		apiShape = ai.APIOpenAICompletions
		defaultBaseURL = "https://api.openai.com/v1"
		envBaseURLKey = "OPENAI_BASE_URL"
	}

	modelID := strings.TrimSpace(profile.ModelID)
	if modelID == "" {
		if providerID == "anthropic" {
			modelID = "claude-sonnet-4-5"
		} else {
			modelID = "gpt-4.1"
		}
	}

	// Try resolving from catalog first to inherit accurate token/context limits.
	var resolved *ai.Model
	if cat := ai.GetModel(providerID, modelID); cat != nil {
		clone := *cat
		resolved = &clone
	} else if r, err := coding.ResolveModel(providerID + "/" + modelID); err == nil && r != nil {
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

	ensureMultiAPIProviders(m.models, resolved)
	return resolved, m.models
}

// InitOrResetSession creates a fresh coding.Session wired to the EnvironmentRouter and starts a new JSONL recorder.
func (m *Manager) InitOrResetSession(profile *store.ProviderProfile) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initSessionLocked(profile, nil)
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
	}

	model, modelsRuntime := m.buildModelAndRuntime(profile)
	m.models = modelsRuntime

	thinking := agent.ThinkMedium
	if profile != nil && profile.ThinkingLevel != "" {
		thinking = agent.ThinkingLevel(profile.ThinkingLevel)
	}

	targetCwd := m.baseCwd
	if t := m.router.Target(); !t.IsRemote && t.WorkDir != "" {
		targetCwd = store.ExpandLocalPath(t.WorkDir)
	}

	var beforeHook agent.BeforeToolCallFunc
	if m.safetyGate != nil {
		beforeHook = m.safetyGate.BeforeToolCall
	}

	opts := coding.SessionOptions{
		Model:          model,
		Cwd:            targetCwd,
		Tools:          m.router.BuildTools(targetCwd),
		ThinkingLevel:  thinking,
		TrustProject:   true,
		Models:         modelsRuntime,
		Compaction:     coding.DefaultCompactionSettings(),
		BeforeToolCall: beforeHook,
	}

	sess, err := coding.NewSession(opts)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	m.recorderErr = nil
	rec := existingRec
	if rec == nil {
		started, startErr := coding.StartSession(m.baseCwd, model, string(thinking))
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

// ListSavedSessions lists JSONL sessions stored for baseCwd (newest first).
func (m *Manager) ListSavedSessions() []coding.SessionInfo {
	return coding.ListSessions(m.baseCwd, strings.TrimSpace(os.Getenv("PIUI_SESSION_DIR")))
}

// ListSavedSessionSummaries lists JSONL sessions enriched with their first user prompt title
// and formatted "YYYY-MM-DD HH:MM" timestamp.
func (m *Manager) ListSavedSessionSummaries() []SavedSessionSummary {
	raw := m.ListSavedSessions()
	out := make([]SavedSessionSummary, 0, len(raw))
	for _, s := range raw {
		out = append(out, SavedSessionSummary{
			ID:        s.ID,
			Path:      s.Path,
			Title:     ExtractSessionTitle(s.Path),
			Timestamp: FormatSessionTimestamp(s.Timestamp),
			Messages:  s.Messages,
		})
	}
	return out
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

// Run executes a user prompt turn (prepending a target-switch context note if the user switched workspace targets).
func (m *Manager) Run(ctx context.Context, prompt string) (*coding.RunResult, error) {
	m.mu.Lock()
	sess := m.sess
	m.mu.Unlock()
	if sess == nil {
		return nil, fmt.Errorf("session not initialized")
	}

	actualPrompt := prompt
	if notice := m.router.ConsumeSwitchNotice(); notice != "" {
		actualPrompt = notice + "\n\n" + prompt
	}
	res, err := sess.Run(ctx, actualPrompt)

	m.mu.Lock()
	if m.rec != nil && m.rec.LastEntryID() != "" {
		m.activeLeafID = m.rec.LastEntryID()
	}
	m.mu.Unlock()
	return res, err
}

// Steer injects an in-flight steering instruction into the active agent loop.
func (m *Manager) Steer(prompt string) {
	m.mu.Lock()
	sess := m.sess
	m.mu.Unlock()
	if sess == nil {
		return
	}
	actualPrompt := prompt
	if notice := m.router.ConsumeSwitchNotice(); notice != "" {
		actualPrompt = notice + "\n\n" + prompt
	}
	sess.Steer(ai.NewUserText(actualPrompt, time.Now().UnixMilli()))
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
