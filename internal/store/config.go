package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// APIFormat defines the wire protocol format for the custom LLM endpoint.
type APIFormat string

const (
	FormatAnthropicMessages APIFormat = "anthropic-messages"
	FormatOpenAICompletions APIFormat = "openai-completions"
	FormatOpenAIResponses   APIFormat = "openai-responses"
)

// SafetyPolicy controls when Human-in-the-Loop approval prompts intercept tool execution.
type SafetyPolicy string

const (
	// SafetyPolicyRemoteHighRisk intercepts high-risk bash/write/edit on remote SSH hosts,
	// and critical destructive commands (e.g. rm -rf /, mkfs, dd, shutdown) locally.
	SafetyPolicyRemoteHighRisk SafetyPolicy = "remote-high-risk"
	// SafetyPolicyAlwaysHighRisk intercepts high-risk bash/write/edit in both local and remote environments.
	SafetyPolicyAlwaysHighRisk SafetyPolicy = "always-high-risk"
	// SafetyPolicyStrictRemote intercepts all file mutations and non-readonly commands on remote SSH hosts.
	SafetyPolicyStrictRemote SafetyPolicy = "strict-remote"
	// SafetyPolicyOff disables interactive approval prompts.
	SafetyPolicyOff SafetyPolicy = "off"
)

const (
	ThemeDark  = "dark"
	ThemeLight = "light"
)

// ProviderProfile holds connection settings for an OpenAI or Anthropic compatible endpoint.
type ProviderProfile struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Format        APIFormat `json:"format"`
	BaseURL       string    `json:"base_url"`
	APIKey        string    `json:"api_key"`
	ModelID       string    `json:"model_id"`
	Models        []string  `json:"models,omitempty"`
	ThinkingLevel string    `json:"thinking_level"` // "off", "low", "medium", "high"
}

// AvailableModels returns the deduplicated list of models configured for this channel,
// always including the active ModelID if non-empty.
func (p *ProviderProfile) AvailableModels() []string {
	if p == nil {
		return nil
	}
	seen := make(map[string]bool, len(p.Models)+1)
	var out []string
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, m := range p.Models {
		add(m)
	}
	add(p.ModelID)
	return out
}

// HasModel reports whether modelID is in this channel's configured Models list.
func (p *ProviderProfile) HasModel(modelID string) bool {
	if p == nil {
		return false
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return false
	}
	for _, m := range p.Models {
		if strings.TrimSpace(m) == modelID {
			return true
		}
	}
	return strings.TrimSpace(p.ModelID) == modelID
}

// AddModel adds modelID to the channel's Models list if not already present.
func (p *ProviderProfile) AddModel(modelID string) {
	if p == nil {
		return
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return
	}
	for _, m := range p.Models {
		if strings.TrimSpace(m) == modelID {
			return
		}
	}
	p.Models = append(p.Models, modelID)
	if strings.TrimSpace(p.ModelID) == "" {
		p.ModelID = modelID
	}
}

// RemoveModel removes modelID from the channel's Models list.
func (p *ProviderProfile) RemoveModel(modelID string) {
	if p == nil {
		return
	}
	modelID = strings.TrimSpace(modelID)
	filtered := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		if strings.TrimSpace(m) != modelID {
			filtered = append(filtered, strings.TrimSpace(m))
		}
	}
	p.Models = filtered
	if strings.TrimSpace(p.ModelID) == modelID {
		if len(p.Models) > 0 {
			p.ModelID = p.Models[0]
		}
	}
}

// WorkspaceTarget abstracts a local project directory or a remote SSH workspace.
type WorkspaceTarget struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsRemote bool   `json:"is_remote"`
	WorkDir  string `json:"work_dir"`
	SSHHost  string `json:"ssh_host,omitempty"` // ~/.ssh/config alias or user@host:port
}

// ShortName returns a concise target name suitable for badges and sidebar headers.
func (t WorkspaceTarget) ShortName() string {
	name := strings.TrimSpace(t.Name)
	if strings.HasPrefix(name, "本机当前目录 (") && strings.HasSuffix(name, ")") {
		inner := strings.TrimSuffix(strings.TrimPrefix(name, "本机当前目录 ("), ")")
		if inner != "" {
			return inner
		}
	}
	if strings.HasPrefix(name, "SSH: ") {
		inner := strings.TrimSpace(strings.TrimPrefix(name, "SSH: "))
		if inner != "" {
			return inner
		}
	}
	if name != "" {
		return name
	}
	if t.IsRemote {
		if t.SSHHost != "" {
			return t.SSHHost
		}
		return "remote"
	}
	if t.WorkDir != "" {
		return filepath.Base(t.WorkDir)
	}
	return "local"
}

// DisplaySub returns the secondary monospace path/host description for sidebar target cards.
func (t WorkspaceTarget) DisplaySub() string {
	if t.IsRemote {
		wd := strings.TrimSpace(t.WorkDir)
		if wd == "" {
			wd = "~"
		}
		return t.SSHHost + ":" + wd
	}
	return ShortenHomePath(t.WorkDir)
}

// TurnBadge returns the turn metadata label (e.g. "piui (Local)" or "prod-gateway (SSH)").
func (t WorkspaceTarget) TurnBadge() string {
	if t.IsRemote {
		return t.ShortName() + " (SSH)"
	}
	return t.ShortName() + " (Local)"
}

// ToolCardBadge returns the compact tool-card right badge (e.g. "Local · piui" or "SSH · prod-gateway").
func (t WorkspaceTarget) ToolCardBadge() string {
	if t.IsRemote {
		return "SSH · " + t.ShortName()
	}
	return "Local · " + t.ShortName()
}

// ShortenHomePath replaces the local user's home directory prefix with "~".
func ShortenHomePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "~"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if strings.HasPrefix(p, home+string(filepath.Separator)) {
			return "~" + p[len(home):]
		}
	}
	return p
}

// AppConfig is persisted to ~/.config/piui/config.json (or PIUI_CONFIG_PATH).
type AppConfig struct {
	ActiveProfileID string            `json:"active_profile_id"`
	Profiles        []ProviderProfile `json:"profiles"`
	ActiveTargetID  string            `json:"active_target_id"`
	Targets         []WorkspaceTarget `json:"targets"`
	SafetyPolicy    SafetyPolicy      `json:"safety_policy,omitempty"`
	Theme           string            `json:"theme,omitempty"` // "dark" (default) or "light"

	configPath  string `json:"-"`
	LoadWarning string `json:"-"`
	BackupPath  string `json:"-"`
}

// IsLightTheme reports whether the Daylight (light) theme is active.
func (c *AppConfig) IsLightTheme() bool {
	if c == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(c.Theme), ThemeLight)
}

// ToggleTheme switches between Dark ("dark") and Light ("light") themes and returns the new theme name.
func (c *AppConfig) ToggleTheme() string {
	if c == nil {
		return ThemeDark
	}
	if c.IsLightTheme() {
		c.Theme = ThemeDark
	} else {
		c.Theme = ThemeLight
	}
	return c.Theme
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

func defaultProfiles() []ProviderProfile {
	return []ProviderProfile{
		{
			ID:            "anthropic-default",
			Name:          "Anthropic / 兼容接口",
			Format:        FormatAnthropicMessages,
			BaseURL:       strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
			APIKey:        strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
			ModelID:       "claude-sonnet-4-5",
			Models:        []string{"claude-sonnet-4-5", "claude-opus-4-5", "claude-haiku-4-5"},
			ThinkingLevel: "medium",
		},
		{
			ID:            "openai-chat",
			Name:          "OpenAI Chat / 聚合网关",
			Format:        FormatOpenAICompletions,
			BaseURL:       strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
			APIKey:        strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
			ModelID:       "gpt-4.1",
			Models:        []string{"gpt-4.1", "gpt-4.1-mini", "gpt-4o", "o4-mini"},
			ThinkingLevel: "medium",
		},
		{
			ID:            "openai-responses",
			Name:          "OpenAI Responses API",
			Format:        FormatOpenAIResponses,
			BaseURL:       strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
			APIKey:        strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
			ModelID:       "gpt-5",
			Models:        []string{"gpt-5", "gpt-5-mini", "o3", "gpt-4.1"},
			ThinkingLevel: "medium",
		},
	}
}

func newDefaultConfig(path, defaultWorkDir string) AppConfig {
	cfg := AppConfig{
		configPath:      path,
		ActiveProfileID: "anthropic-default",
		SafetyPolicy:    SafetyPolicyRemoteHighRisk,
		Theme:           ThemeDark,
		Profiles:        defaultProfiles(),
	}
	cfg.ensureProfiles()
	cfg.ensureTargets(defaultWorkDir)
	return cfg
}

// LoadOrInit loads ~/.config/piui/config.json (or PIUI_CONFIG_PATH) or initializes defaults
// only when the file does not exist yet. If an existing file cannot be read or contains
// corrupted JSON, it preserves/backs up the file without overwriting it and records LoadWarning.
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
	if err != nil {
		cfg := newDefaultConfig(path, defaultWorkDir)
		if errors.Is(err, os.ErrNotExist) {
			if saveErr := cfg.Save(); saveErr != nil {
				cfg.LoadWarning = fmt.Sprintf("初始化保存配置文件 %s 失败: %v", ShortenHomePath(path), saveErr)
			}
			return &cfg
		}
		// Existing file could not be read (e.g. permission denied): do NOT overwrite it.
		cfg.LoadWarning = fmt.Sprintf("读取配置文件 %s 失败: %v（已使用临时默认配置启动，原文件未被覆盖）", ShortenHomePath(path), err)
		return &cfg
	}

	var cfg AppConfig
	if unmarshalErr := json.Unmarshal(data, &cfg); unmarshalErr != nil {
		fallback := newDefaultConfig(path, defaultWorkDir)
		backupPath := fmt.Sprintf("%s.corrupt-%s.bak", path, time.Now().Format("20060102-150405"))
		if bErr := os.WriteFile(backupPath, data, 0o600); bErr == nil {
			fallback.BackupPath = backupPath
			fallback.LoadWarning = fmt.Sprintf(
				"配置文件 %s JSON 解析失败 (%v)；已备份损坏文件至 %s 并以默认配置运行（未覆盖原文件）",
				ShortenHomePath(path), unmarshalErr, ShortenHomePath(backupPath),
			)
		} else {
			fallback.LoadWarning = fmt.Sprintf(
				"配置文件 %s JSON 解析失败 (%v)，且创建备份失败 (%v)；已以内存默认配置运行（未覆盖原文件）",
				ShortenHomePath(path), unmarshalErr, bErr,
			)
		}
		return &fallback
	}

	cfg.configPath = path
	cfg.ensureProfiles()
	cfg.ensureTargets(defaultWorkDir)
	return &cfg
}

// Clone returns a deep copy of AppConfig suitable for modal draft editing.
func (c *AppConfig) Clone() AppConfig {
	if c == nil {
		return AppConfig{}
	}
	cp := *c
	cp.Profiles = make([]ProviderProfile, len(c.Profiles))
	for i, p := range c.Profiles {
		pCopy := p
		if len(p.Models) > 0 {
			pCopy.Models = make([]string, len(p.Models))
			copy(pCopy.Models, p.Models)
		}
		cp.Profiles[i] = pCopy
	}
	cp.Targets = make([]WorkspaceTarget, len(c.Targets))
	copy(cp.Targets, c.Targets)
	return cp
}

func (c *AppConfig) ensureProfiles() {
	if len(c.Profiles) == 0 {
		c.Profiles = defaultProfiles()
	}
	for i := range c.Profiles {
		p := &c.Profiles[i]
		p.Models = p.AvailableModels()
		if strings.TrimSpace(p.ModelID) == "" && len(p.Models) > 0 {
			p.ModelID = p.Models[0]
		}
		if p.ThinkingLevel == "" {
			p.ThinkingLevel = "medium"
		}
	}
	validActive := false
	for i := range c.Profiles {
		if c.Profiles[i].ID == c.ActiveProfileID {
			validActive = true
			break
		}
	}
	if !validActive && len(c.Profiles) > 0 {
		c.ActiveProfileID = c.Profiles[0].ID
	}
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
	if c.SafetyPolicy == "" {
		c.SafetyPolicy = SafetyPolicyRemoteHighRisk
	}
	if !strings.EqualFold(c.Theme, ThemeLight) {
		c.Theme = ThemeDark
	}
}

// Save atomically writes the configuration to ~/.config/piui/config.json (or the configured path)
// via a same-directory 0600 temporary file, fsync, and atomic rename.
func (c *AppConfig) Save() error {
	path := c.configPath
	if path == "" {
		path = ConfigPath()
		c.configPath = path
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmpFile, err := os.CreateTemp(dir, ".piui-config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
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
