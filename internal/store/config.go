package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// APIFormat defines the wire protocol format for the custom LLM endpoint.
type APIFormat string

const (
	FormatAnthropicMessages APIFormat = "anthropic-messages"
	FormatOpenAICompletions APIFormat = "openai-completions"
	FormatOpenAIResponses   APIFormat = "openai-responses"
)

// ProviderProfile holds connection settings for an OpenAI or Anthropic compatible endpoint.
type ProviderProfile struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Format        APIFormat `json:"format"`
	BaseURL       string    `json:"base_url"`
	APIKey        string    `json:"api_key"`
	ModelID       string    `json:"model_id"`
	ThinkingLevel string    `json:"thinking_level"` // "off", "low", "medium", "high"
}

// WorkspaceTarget abstracts a local project directory or a remote SSH workspace.
type WorkspaceTarget struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsRemote bool   `json:"is_remote"`
	WorkDir  string `json:"work_dir"`
	SSHHost  string `json:"ssh_host,omitempty"` // ~/.ssh/config alias or user@host:port
}

// AppConfig is persisted to ~/.config/piui/config.json (or PIUI_CONFIG_PATH).
type AppConfig struct {
	ActiveProfileID string            `json:"active_profile_id"`
	Profiles        []ProviderProfile `json:"profiles"`
	ActiveTargetID  string            `json:"active_target_id"`
	Targets         []WorkspaceTarget `json:"targets"`

	configPath string `json:"-"`
}

// ConfigPath returns the path to ~/.config/piui/config.json, honoring PIUI_CONFIG_PATH if set.
func ConfigPath() string {
	if envPath := strings.TrimSpace(os.Getenv("PIUI_CONFIG_PATH")); envPath != "" {
		return ExpandLocalPath(envPath)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "piui-config.json"
	}
	return filepath.Join(home, ".config", "piui", "config.json")
}

// ExpandLocalPath expands a leading "~" or "~/" to the local user's home directory.
func ExpandLocalPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// LoadOrInit loads ~/.config/piui/config.json (or PIUI_CONFIG_PATH) or creates sensible defaults.
func LoadOrInit(defaultWorkDir string) *AppConfig {
	if defaultWorkDir == "" {
		defaultWorkDir, _ = os.Getwd()
	}
	defaultWorkDir = ExpandLocalPath(defaultWorkDir)
	if abs, err := filepath.Abs(defaultWorkDir); err == nil {
		defaultWorkDir = abs
	}

	path := ConfigPath()
	data, err := os.ReadFile(path)
	var cfg AppConfig
	if err == nil && json.Unmarshal(data, &cfg) == nil && len(cfg.Profiles) > 0 {
		cfg.configPath = path
		cfg.ensureTargets(defaultWorkDir)
		return &cfg
	}

	cfg = AppConfig{
		configPath:      path,
		ActiveProfileID: "anthropic-default",
		Profiles: []ProviderProfile{
			{
				ID:            "anthropic-default",
				Name:          "Anthropic / 兼容接口",
				Format:        FormatAnthropicMessages,
				BaseURL:       strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
				APIKey:        strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
				ModelID:       "claude-sonnet-4-5",
				ThinkingLevel: "medium",
			},
			{
				ID:            "openai-chat",
				Name:          "OpenAI Chat / 聚合网关",
				Format:        FormatOpenAICompletions,
				BaseURL:       strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
				APIKey:        strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
				ModelID:       "gpt-4.1",
				ThinkingLevel: "medium",
			},
			{
				ID:            "openai-responses",
				Name:          "OpenAI Responses API",
				Format:        FormatOpenAIResponses,
				BaseURL:       strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
				APIKey:        strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
				ModelID:       "gpt-5",
				ThinkingLevel: "medium",
			},
		},
	}
	cfg.ensureTargets(defaultWorkDir)
	_ = cfg.Save()
	return &cfg
}

// Clone returns a deep copy of AppConfig suitable for modal draft editing.
func (c *AppConfig) Clone() AppConfig {
	if c == nil {
		return AppConfig{}
	}
	cp := *c
	cp.Profiles = make([]ProviderProfile, len(c.Profiles))
	copy(cp.Profiles, c.Profiles)
	cp.Targets = make([]WorkspaceTarget, len(c.Targets))
	copy(cp.Targets, c.Targets)
	return cp
}

func (c *AppConfig) ensureTargets(defaultWorkDir string) {
	localCwdTarget := WorkspaceTarget{
		ID:       "local-cwd",
		Name:     "本机当前目录 (" + filepath.Base(defaultWorkDir) + ")",
		IsRemote: false,
		WorkDir:  defaultWorkDir,
	}

	foundLocalCwd := false
	for i := range c.Targets {
		if !c.Targets[i].IsRemote && c.Targets[i].WorkDir != "" {
			c.Targets[i].WorkDir = ExpandLocalPath(c.Targets[i].WorkDir)
		}
		if c.Targets[i].ID == "local-cwd" {
			c.Targets[i] = localCwdTarget
			foundLocalCwd = true
		}
	}
	if !foundLocalCwd {
		c.Targets = append([]WorkspaceTarget{localCwdTarget}, c.Targets...)
	}

	// Merge SSH hosts discovered from ~/.ssh/config if not already present.
	existingSSH := map[string]bool{}
	for _, t := range c.Targets {
		if t.IsRemote && t.SSHHost != "" {
			existingSSH[t.SSHHost] = true
		}
	}
	for _, host := range DiscoverSSHConfigHosts() {
		if existingSSH[host] {
			continue
		}
		c.Targets = append(c.Targets, WorkspaceTarget{
			ID:       "ssh-" + host,
			Name:     "SSH: " + host,
			IsRemote: true,
			WorkDir:  "~",
			SSHHost:  host,
		})
	}

	// Ensure ActiveTargetID references an existing target.
	validActive := false
	for _, t := range c.Targets {
		if t.ID == c.ActiveTargetID {
			validActive = true
			break
		}
	}
	if !validActive && len(c.Targets) > 0 {
		c.ActiveTargetID = c.Targets[0].ID
	}
}

// Save writes the configuration to ~/.config/piui/config.json (or the configured path).
func (c *AppConfig) Save() error {
	path := c.configPath
	if path == "" {
		path = ConfigPath()
		c.configPath = path
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// ActiveProfile returns a pointer to the currently selected ProviderProfile.
func (c *AppConfig) ActiveProfile() *ProviderProfile {
	for i := range c.Profiles {
		if c.Profiles[i].ID == c.ActiveProfileID {
			return &c.Profiles[i]
		}
	}
	if len(c.Profiles) > 0 {
		c.ActiveProfileID = c.Profiles[0].ID
		return &c.Profiles[0]
	}
	return nil
}

// ActiveTarget returns a pointer to the currently selected WorkspaceTarget.
func (c *AppConfig) ActiveTarget() *WorkspaceTarget {
	for i := range c.Targets {
		if c.Targets[i].ID == c.ActiveTargetID {
			return &c.Targets[i]
		}
	}
	if len(c.Targets) > 0 {
		c.ActiveTargetID = c.Targets[0].ID
		return &c.Targets[0]
	}
	return nil
}

// DiscoverSSHConfigHosts parses concrete Host aliases from ~/.ssh/config.
func DiscoverSSHConfigHosts() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return nil
	}
	defer f.Close()

	var hosts []string
	seen := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "Host") {
			for _, alias := range fields[1:] {
				if strings.ContainsAny(alias, "*?!") {
					continue
				}
				if !seen[alias] {
					seen[alias] = true
					hosts = append(hosts, alias)
				}
			}
		}
	}
	return hosts
}
