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
// custom LLM provider endpoints, and JSONL session persistence.
type Manager struct {
	mu          sync.Mutex
	baseCwd     string
	router      *EnvironmentRouter
	sess        *coding.Session
	unsub       func()
	listener    agent.Listener
	credentials *coding.RuntimeCredentials
	models      ai.Models
	activePath  string
}

// NewManager initializes the harness manager for the given base directory and config.
func NewManager(baseCwd string, cfg *store.AppConfig, termSink TerminalStreamSink, listener agent.Listener) (*Manager, error) {
	if baseCwd == "" {
		baseCwd, _ = os.Getwd()
	}
	initialTarget := store.WorkspaceTarget{
		ID:      "local-cwd",
		Name:    "Local",
		WorkDir: baseCwd,
	}
	if t := cfg.ActiveTarget(); t != nil {
		initialTarget = *t
	}

	router := NewEnvironmentRouter(initialTarget, termSink)
	m := &Manager{
		baseCwd:     baseCwd,
		router:      router,
		listener:    listener,
		credentials: coding.NewRuntimeCredentials(nil),
	}

	if err := m.InitOrResetSession(cfg.ActiveProfile()); err != nil {
		return nil, err
	}
	return m, nil
}

// Router returns the dynamic EnvironmentRouter.
func (m *Manager) Router() *EnvironmentRouter {
	return m.router
}

// buildModelAndRuntime resolves a ProviderProfile into an *ai.Model and ai.Models runtime.
func (m *Manager) buildModelAndRuntime(profile *store.ProviderProfile) (*ai.Model, ai.Models) {
	if profile == nil {
		model, _ := coding.ResolveModel(coding.DefaultModelSpec)
		return model, ai.BuiltinModels(&ai.CreateModelsOptions{Credentials: m.credentials})
	}

	var providerID string
	var apiShape ai.Api
	var defaultBaseURL string

	switch profile.Format {
	case store.FormatAnthropicMessages:
		providerID = "anthropic"
		apiShape = "anthropic-messages"
		defaultBaseURL = "https://api.anthropic.com"
	case store.FormatOpenAIResponses:
		providerID = "openai"
		apiShape = "openai-responses"
		defaultBaseURL = "https://api.openai.com/v1"
	default:
		providerID = "openai"
		apiShape = ai.APIOpenAICompletions
		defaultBaseURL = "https://api.openai.com/v1"
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
	} else if resolved.BaseURL == "" {
		resolved.BaseURL = defaultBaseURL
	}

	if key := strings.TrimSpace(profile.APIKey); key != "" {
		m.credentials.SetRuntimeApiKey(providerID, key)
	} else {
		m.credentials.RemoveRuntimeApiKey(providerID)
	}

	modelsRuntime := ai.BuiltinModels(&ai.CreateModelsOptions{
		Credentials: m.credentials,
	})
	return resolved, modelsRuntime
}

// InitOrResetSession creates a fresh coding.Session wired to the EnvironmentRouter and starts a new JSONL recorder.
func (m *Manager) InitOrResetSession(profile *store.ProviderProfile) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.unsub != nil {
		m.unsub()
		m.unsub = nil
	}

	model, modelsRuntime := m.buildModelAndRuntime(profile)
	m.models = modelsRuntime

	thinking := agent.ThinkMedium
	if profile != nil && profile.ThinkingLevel != "" {
		thinking = agent.ThinkingLevel(profile.ThinkingLevel)
	}

	targetCwd := m.baseCwd
	if t := m.router.Target(); !t.IsRemote && t.WorkDir != "" {
		targetCwd = t.WorkDir
	}

	opts := coding.SessionOptions{
		Model:         model,
		Cwd:           targetCwd,
		Tools:         m.router.BuildTools(targetCwd),
		ThinkingLevel: thinking,
		TrustProject:  true,
		Models:        modelsRuntime,
		Compaction:    coding.DefaultCompactionSettings(),
	}

	sess, err := coding.NewSession(opts)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	if rec, err := coding.StartSession(m.baseCwd, model, string(thinking)); err == nil {
		sess.Record(rec)
		m.activePath = ""
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
		m.mu.Unlock()
		err := m.InitOrResetSession(profile)
		m.mu.Lock()
		return err
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
	return coding.ListSessions(m.baseCwd, "")
}

// ResumeSavedSession loads a historical JSONL session into the current session and continues recording to it.
func (m *Manager) ResumeSavedSession(path string, profile *store.ProviderProfile) ([]agent.AgentMessage, error) {
	if err := m.InitOrResetSession(profile); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	tree, err := coding.LoadSessionTree(path)
	if err != nil {
		return nil, fmt.Errorf("load session tree: %w", err)
	}
	proj := tree.BuildProjection()
	m.sess.LoadBranch(proj)

	rec, err := coding.ResumeSession(path)
	if err != nil {
		return nil, fmt.Errorf("resume session recorder: %w", err)
	}
	m.sess.Record(rec)
	m.activePath = path
	return proj.Messages, nil
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
	return sess.Run(ctx, actualPrompt)
}

// Steer injects an in-flight steering instruction into the active agent loop.
func (m *Manager) Steer(prompt string) {
	m.mu.Lock()
	sess := m.sess
	m.mu.Unlock()
	if sess == nil {
		return
	}
	sess.Steer(ai.NewUserText(prompt, time.Now().UnixMilli()))
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
