package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/sky-valley/pi/coding"
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
	// and critical destructive commands (e.g. rm -rf /, mkfs, dd, shutdown, ~/.ssh, /etc) locally.
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

// MCPTransport defines the wire transport for an external Model Context Protocol (MCP) server.
type MCPTransport string

const (
	MCPTransportStdio MCPTransport = "stdio"
	MCPTransportHTTP  MCPTransport = "http"
)

// MCPServerConfig describes a configured Stdio or Streamable HTTP MCP server.
type MCPServerConfig struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Transport      MCPTransport      `json:"transport"`
	Enabled        bool              `json:"enabled"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	BearerToken    string            `json:"bearer_token,omitempty"`
	KeychainStored bool              `json:"keychain_stored,omitempty"`
}

// DisplayEndpoint returns a concise monospace summary of the server's command or URL.
func (s MCPServerConfig) DisplayEndpoint() string {
	if s.Transport == MCPTransportHTTP {
		return strings.TrimSpace(s.URL)
	}
	parts := append([]string{strings.TrimSpace(s.Command)}, s.Args...)
	return strings.TrimSpace(strings.Join(parts, " "))
}

// Clone returns a deep copy of MCPServerConfig.
func (s MCPServerConfig) Clone() MCPServerConfig {
	cp := s
	if len(s.Args) > 0 {
		cp.Args = make([]string, len(s.Args))
		copy(cp.Args, s.Args)
	}
	if len(s.Env) > 0 {
		cp.Env = make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			cp.Env[k] = v
		}
	}
	if len(s.Headers) > 0 {
		cp.Headers = make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			cp.Headers[k] = v
		}
	}
	return cp
}

// ParseCommandLine splits a command line (supporting single/double quotes and backslash escapes)
// into an executable name and argument slice.
func ParseCommandLine(line string) (cmd string, args []string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", nil
	}
	var tokens []string
	var cur strings.Builder
	var quote rune
	inToken := false
	escaped := false

	for _, r := range line {
		if escaped {
			cur.WriteRune(r)
			escaped = false
			inToken = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			inToken = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			inToken = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			inToken = true
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if inToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inToken = false
			}
			continue
		}
		cur.WriteRune(r)
		inToken = true
	}
	if inToken {
		tokens = append(tokens, cur.String())
	}
	if len(tokens) == 0 {
		return "", nil
	}
	return tokens[0], tokens[1:]
}

// ParseKeyValuePairs parses KEY=VALUE entries separated by newlines or semicolons.
func ParseKeyValuePairs(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	for _, chunk := range strings.FieldsFunc(normalized, func(r rune) bool { return r == '\n' || r == ';' }) {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" || strings.HasPrefix(chunk, "#") {
			continue
		}
		k, v, ok := strings.Cut(chunk, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ProviderProfile holds connection settings for an OpenAI or Anthropic compatible endpoint.
type ProviderProfile struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Format         APIFormat `json:"format"`
	BaseURL        string    `json:"base_url"`
	APIKey         string    `json:"api_key,omitempty"`
	KeychainStored bool      `json:"keychain_stored,omitempty"`
	ModelID        string    `json:"model_id"`
	Models         []string  `json:"models,omitempty"`
	ThinkingLevel  string    `json:"thinking_level"` // "off", "low", "medium", "high"
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

// WorkspaceTarget abstracts a first-class Local project directory or Remote SSH project directory.
type WorkspaceTarget struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsRemote bool   `json:"is_remote"`
	WorkDir  string `json:"work_dir"`
	SSHHost  string `json:"ssh_host,omitempty"` // ~/.ssh/config alias or user@host
	Trusted  bool   `json:"trusted,omitempty"`  // Whether project-local .pi/skills are trusted
}

// ShortName returns a concise workspace name suitable for badges and sidebar headers.
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
		wd := strings.TrimSpace(t.WorkDir)
		if wd != "" && wd != "~" && wd != "/" {
			base := path.Base(strings.TrimRight(wd, "/"))
			if base != "" && base != "." && base != "~" {
				return base
			}
		}
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

// DisplaySub returns the secondary monospace path/host description for sidebar workspace cards.
func (t WorkspaceTarget) DisplaySub() string {
	if t.IsRemote {
		wd := strings.TrimSpace(t.WorkDir)
		if wd == "" {
			wd = "~"
		}
		return strings.TrimSpace(t.SSHHost) + ":" + wd
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

// SessionScopeCwd returns the deterministic scope path passed to coding.StartSession and
// coding.ListSessions so each Local or Remote SSH workspace owns an isolated session list.
// Local workspaces map directly to their absolute directory path (100% compatible with pi CLI).
// Remote SSH workspaces map to a synthetic rooted POSIX path "/__piui_ssh__/<host>/<workdir>"
// that coding.DefaultSessionDir encodes safely under ~/.pi/agent/sessions/ without touching "/".
func (t WorkspaceTarget) SessionScopeCwd() string {
	if !t.IsRemote {
		wd := ExpandLocalPath(strings.TrimSpace(t.WorkDir))
		if wd == "" {
			wd, _ = os.Getwd()
		}
		if abs, err := filepath.Abs(wd); err == nil {
			return abs
		}
		return wd
	}

	host := sanitizeScopeSegment(t.SSHHost)
	if host == "" {
		host = "remote"
	}
	wd := strings.TrimSpace(t.WorkDir)
	if wd == "" || wd == "~" {
		wd = "home"
	} else if strings.HasPrefix(wd, "~/") {
		wd = "home/" + strings.TrimPrefix(wd, "~/")
	} else {
		wd = strings.TrimPrefix(path.Clean(wd), "/")
	}
	if wd == "" || wd == "." {
		wd = "root"
	}
	return "/__piui_ssh__/" + host + "/" + wd
}

func sanitizeScopeSegment(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == '@':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// SameLocation reports whether two WorkspaceTargets refer to the same physical directory/host.
func (t WorkspaceTarget) SameLocation(other WorkspaceTarget) bool {
	if t.IsRemote != other.IsRemote {
		return false
	}
	if t.IsRemote {
		w1 := normalizeRemoteWorkDir(t.WorkDir)
		w2 := normalizeRemoteWorkDir(other.WorkDir)
		return strings.EqualFold(strings.TrimSpace(t.SSHHost), strings.TrimSpace(other.SSHHost)) && w1 == w2
	}
	p1 := filepath.Clean(ExpandLocalPath(t.WorkDir))
	p2 := filepath.Clean(ExpandLocalPath(other.WorkDir))
	return p1 == p2
}

func normalizeRemoteWorkDir(wd string) string {
	wd = strings.TrimSpace(wd)
	if wd == "" || wd == "~" || wd == "~/" {
		return "~"
	}
	if strings.HasPrefix(wd, "~/") {
		return "~/" + strings.TrimPrefix(path.Clean("/"+wd[2:]), "/")
	}
	return path.Clean(wd)
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

// LoadOptions controls startup workspace resolution behavior.
type LoadOptions struct {
	StartupDir         string
	ExplicitStartupDir bool // True when user explicitly passed `piui /path/to/dir` on CLI
}

// AppConfig is persisted to ~/.config/piui/config.json (or PIUI_CONFIG_PATH).
type AppConfig struct {
	ActiveProfileID string            `json:"active_profile_id"`
	Profiles        []ProviderProfile `json:"profiles"`
	ActiveTargetID  string            `json:"active_target_id"`
	Targets         []WorkspaceTarget `json:"targets"`
	MCPServers      []MCPServerConfig `json:"mcp_servers,omitempty"`
	SafetyPolicy    SafetyPolicy      `json:"safety_policy,omitempty"`
	Theme           string            `json:"theme,omitempty"`          // "dark" (default) or "light"
	SessionTitles   map[string]string `json:"session_titles,omitempty"` // Custom renamed titles keyed by session UUID

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

// SessionTitle returns the user's custom title for sessionID if set, or fallback otherwise.
func (c *AppConfig) SessionTitle(sessionID, fallback string) string {
	if c != nil && c.SessionTitles != nil {
		if custom := strings.TrimSpace(c.SessionTitles[strings.TrimSpace(sessionID)]); custom != "" {
			return custom
		}
	}
	return fallback
}

// SetSessionTitle sets or clears a custom title override for sessionID.
func (c *AppConfig) SetSessionTitle(sessionID, title string) {
	if c == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	title = strings.TrimSpace(title)
	if title == "" {
		delete(c.SessionTitles, sessionID)
		return
	}
	if c.SessionTitles == nil {
		c.SessionTitles = make(map[string]string)
	}
	c.SessionTitles[sessionID] = title
}

// DeleteSessionTitle removes any custom title stored for sessionID.
func (c *AppConfig) DeleteSessionTitle(sessionID string) {
	if c == nil || c.SessionTitles == nil {
		return
	}
	delete(c.SessionTitles, strings.TrimSpace(sessionID))
}

// UpsertMCPServer adds or updates an MCPServerConfig entry.
func (c *AppConfig) UpsertMCPServer(srv MCPServerConfig) MCPServerConfig {
	srv.Name = strings.TrimSpace(srv.Name)
	if srv.Transport != MCPTransportHTTP {
		srv.Transport = MCPTransportStdio
	}
	if srv.Name == "" {
		if srv.Transport == MCPTransportHTTP && strings.TrimSpace(srv.URL) != "" {
			srv.Name = strings.TrimSpace(srv.URL)
		} else if strings.TrimSpace(srv.Command) != "" {
			srv.Name = filepath.Base(strings.TrimSpace(srv.Command))
		} else {
			srv.Name = "mcp-server"
		}
	}
	if strings.TrimSpace(srv.ID) == "" {
		srv.ID = fmt.Sprintf("mcp-%d", time.Now().UnixNano()%1000000)
	}
	for i := range c.MCPServers {
		if c.MCPServers[i].ID == srv.ID {
			c.MCPServers[i] = srv.Clone()
			return c.MCPServers[i]
		}
	}
	c.MCPServers = append(c.MCPServers, srv.Clone())
	return srv
}

// RemoveMCPServer deletes an MCP server by ID and cleans up any Keychain secret associated with it.
func (c *AppConfig) RemoveMCPServer(id string) bool {
	if c == nil {
		return false
	}
	idx := -1
	for i := range c.MCPServers {
		if c.MCPServers[i].ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false
	}
	if sec := ActiveSecretStore(); sec != nil {
		_ = sec.Delete("mcp:" + id)
	}
	c.MCPServers = append(c.MCPServers[:idx], c.MCPServers[idx+1:]...)
	return true
}

// UpsertLocalTarget adds a local directory workspace (or updates an existing matching location)
// and optionally activates it.
func (c *AppConfig) UpsertLocalTarget(workDir, name string, makeActive bool) WorkspaceTarget {
	workDir = ExpandLocalPath(strings.TrimSpace(workDir))
	if abs, err := filepath.Abs(workDir); err == nil {
		workDir = abs
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = filepath.Base(workDir)
	}
	candidate := WorkspaceTarget{
		Name:     name,
		IsRemote: false,
		WorkDir:  workDir,
	}
	for i := range c.Targets {
		if c.Targets[i].SameLocation(candidate) {
			if strings.TrimSpace(name) != "" && c.Targets[i].ID != "local-cwd" {
				c.Targets[i].Name = name
			}
			if makeActive {
				c.ActiveTargetID = c.Targets[i].ID
			}
			return c.Targets[i]
		}
	}
	candidate.ID = fmt.Sprintf("local-%d", time.Now().UnixNano()%1000000)
	c.Targets = append(c.Targets, candidate)
	if makeActive {
		c.ActiveTargetID = candidate.ID
	}
	return candidate
}

// UpsertSSHTarget adds a remote SSH workspace (or selects an existing matching host:dir)
// and optionally activates it.
func (c *AppConfig) UpsertSSHTarget(sshHost, workDir, name string, makeActive bool) WorkspaceTarget {
	sshHost = strings.TrimSpace(sshHost)
	workDir = normalizeRemoteWorkDir(workDir)
	name = strings.TrimSpace(name)
	if name == "" {
		if workDir != "~" && workDir != "/" {
			base := path.Base(strings.TrimRight(workDir, "/"))
			if base != "" && base != "." {
				name = base + " (" + sshHost + ")"
			}
		}
		if name == "" {
			name = sshHost
		}
	}
	candidate := WorkspaceTarget{
		Name:     name,
		IsRemote: true,
		WorkDir:  workDir,
		SSHHost:  sshHost,
	}
	for i := range c.Targets {
		if c.Targets[i].SameLocation(candidate) {
			if strings.TrimSpace(name) != "" {
				c.Targets[i].Name = name
			}
			if makeActive {
				c.ActiveTargetID = c.Targets[i].ID
			}
			return c.Targets[i]
		}
	}
	candidate.ID = fmt.Sprintf("ssh-%d", time.Now().UnixNano()%1000000)
	c.Targets = append(c.Targets, candidate)
	if makeActive {
		c.ActiveTargetID = candidate.ID
	}
	return candidate
}

// RemoveTarget deletes a workspace target by ID while keeping at least one workspace present.
func (c *AppConfig) RemoveTarget(targetID string) bool {
	if c == nil || len(c.Targets) <= 1 {
		return false
	}
	idx := -1
	for i := range c.Targets {
		if c.Targets[i].ID == targetID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false
	}
	c.Targets = append(c.Targets[:idx], c.Targets[idx+1:]...)
	if c.ActiveTargetID == targetID {
		c.ActiveTargetID = c.Targets[0].ID
	}
	return true
}

// SetTargetTrusted updates the Trusted flag on the given workspace target ID.
func (c *AppConfig) SetTargetTrusted(targetID string, trusted bool) bool {
	if c == nil {
		return false
	}
	for i := range c.Targets {
		if c.Targets[i].ID == targetID {
			c.Targets[i].Trusted = trusted
			return true
		}
	}
	return false
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
			APIKey:        "",
			ModelID:       "claude-sonnet-4-5",
			Models:        []string{"claude-sonnet-4-5", "claude-opus-4-5", "claude-haiku-4-5"},
			ThinkingLevel: "medium",
		},
		{
			ID:            "openai-chat",
			Name:          "OpenAI Chat / 聚合网关",
			Format:        FormatOpenAICompletions,
			BaseURL:       strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
			APIKey:        "",
			ModelID:       "gpt-4.1",
			Models:        []string{"gpt-4.1", "gpt-4.1-mini", "gpt-4o", "o4-mini"},
			ThinkingLevel: "medium",
		},
		{
			ID:            "openai-responses",
			Name:          "OpenAI Responses API",
			Format:        FormatOpenAIResponses,
			BaseURL:       strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
			APIKey:        "",
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
		SessionTitles:   map[string]string{},
	}
	cfg.ensureProfiles()
	cfg.ensureTargets(defaultWorkDir, true)
	return cfg
}

// LoadOrInit loads ~/.config/piui/config.json (or PIUI_CONFIG_PATH) using defaultWorkDir
// as an explicit CLI startup workspace.
func LoadOrInit(defaultWorkDir string) *AppConfig {
	return LoadOrInitWithOptions(LoadOptions{
		StartupDir:         defaultWorkDir,
		ExplicitStartupDir: strings.TrimSpace(defaultWorkDir) != "",
	})
}

// LoadOrInitWithOptions loads ~/.config/piui/config.json (or PIUI_CONFIG_PATH) or initializes
// defaults only when the file does not exist yet. If an existing file cannot be read or contains
// corrupted JSON, it preserves/backs up the file without overwriting it and records LoadWarning.
func LoadOrInitWithOptions(opts LoadOptions) *AppConfig {
	defaultWorkDir := strings.TrimSpace(opts.StartupDir)
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
	cfg.hydrateSecrets()
	cfg.ensureProfiles()
	pruned := cfg.ensureTargets(defaultWorkDir, opts.ExplicitStartupDir)
	if pruned {
		_ = cfg.Save()
	}
	return &cfg
}

func (c *AppConfig) hydrateSecrets() {
	sec := ActiveSecretStore()
	if sec == nil {
		return
	}
	for i := range c.Profiles {
		p := &c.Profiles[i]
		if p.KeychainStored && strings.TrimSpace(p.APIKey) == "" {
			if secretVal, err := sec.Get(p.ID); err == nil && strings.TrimSpace(secretVal) != "" {
				p.APIKey = strings.TrimSpace(secretVal)
			} else if errors.Is(err, ErrSecretNotFound) {
				p.KeychainStored = false
			}
		}
	}
	for i := range c.MCPServers {
		ms := &c.MCPServers[i]
		if ms.KeychainStored && strings.TrimSpace(ms.BearerToken) == "" {
			if secretVal, err := sec.Get("mcp:" + ms.ID); err == nil && strings.TrimSpace(secretVal) != "" {
				ms.BearerToken = strings.TrimSpace(secretVal)
			} else if errors.Is(err, ErrSecretNotFound) {
				ms.KeychainStored = false
			}
		}
	}
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
	if len(c.MCPServers) > 0 {
		cp.MCPServers = make([]MCPServerConfig, len(c.MCPServers))
		for i, ms := range c.MCPServers {
			cp.MCPServers[i] = ms.Clone()
		}
	}
	if len(c.SessionTitles) > 0 {
		cp.SessionTitles = make(map[string]string, len(c.SessionTitles))
		for k, v := range c.SessionTitles {
			cp.SessionTitles[k] = v
		}
	}
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

// isLegacyAutoScannedSSHTarget identifies SSH entries that an earlier version of PiUI
// automatically populated from ~/.ssh/config (ID="ssh-<host>", Name="SSH: <host>", WorkDir="~",
// untrusted, and with zero recorded sessions). SSH workspaces are strictly manual-only now.
func isLegacyAutoScannedSSHTarget(t WorkspaceTarget) bool {
	host := strings.TrimSpace(t.SSHHost)
	if !t.IsRemote || host == "" || t.Trusted {
		return false
	}
	if t.ID != "ssh-"+host || strings.TrimSpace(t.Name) != "SSH: "+host {
		return false
	}
	if normalizeRemoteWorkDir(t.WorkDir) != "~" {
		return false
	}
	sessions := coding.ListSessions(t.SessionScopeCwd(), strings.TrimSpace(os.Getenv("PIUI_SESSION_DIR")))
	return len(sessions) == 0
}

func (c *AppConfig) ensureTargets(defaultWorkDir string, explicitStartupDir bool) bool {
	prunedLegacy := false
	cleaned := make([]WorkspaceTarget, 0, len(c.Targets))
	for i := range c.Targets {
		t := c.Targets[i]
		if !t.IsRemote && t.WorkDir != "" {
			t.WorkDir = ExpandLocalPath(t.WorkDir)
		} else if t.IsRemote {
			t.WorkDir = normalizeRemoteWorkDir(t.WorkDir)
		}
		if isLegacyAutoScannedSSHTarget(t) {
			prunedLegacy = true
			continue
		}
		cleaned = append(cleaned, t)
	}
	c.Targets = cleaned

	if defaultWorkDir != "" && (explicitStartupDir || len(c.Targets) == 0) {
		localCwdTarget := WorkspaceTarget{
			ID:       "local-cwd",
			Name:     "本机当前目录 (" + filepath.Base(defaultWorkDir) + ")",
			IsRemote: false,
			WorkDir:  defaultWorkDir,
		}
		foundIdx := -1
		for i := range c.Targets {
			if c.Targets[i].ID == "local-cwd" {
				// Preserve user's Trusted flag if reopening the exact same directory!
				if c.Targets[i].SameLocation(localCwdTarget) {
					localCwdTarget.Trusted = c.Targets[i].Trusted
				}
				c.Targets[i] = localCwdTarget
				foundIdx = i
				break
			}
			if c.Targets[i].SameLocation(localCwdTarget) {
				foundIdx = i
				break
			}
		}
		if foundIdx == -1 {
			c.Targets = append([]WorkspaceTarget{localCwdTarget}, c.Targets...)
		}
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
	return prunedLegacy
}

// Save atomically writes the configuration to ~/.config/piui/config.json (or the configured path)
// via a same-directory 0600 temporary file, fsync, and atomic rename.
// When a SecretStore (OS Keychain) is active, API keys are stored in the OS Keychain and scrubbed
// from the serialized JSON file on disk while remaining populated in memory.
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

	diskCopy := c.Clone()
	if sec := ActiveSecretStore(); sec != nil {
		for i := range diskCopy.Profiles {
			dp := &diskCopy.Profiles[i]
			memKey := strings.TrimSpace(c.Profiles[i].APIKey)
			if memKey != "" {
				if err := sec.Set(dp.ID, memKey); err == nil {
					dp.APIKey = ""
					dp.KeychainStored = true
					c.Profiles[i].KeychainStored = true
				} else {
					// Graceful fallback (e.g. headless Linux server with no D-Bus keyring): keep in 0600 JSON.
					dp.KeychainStored = false
					c.Profiles[i].KeychainStored = false
				}
			} else {
				_ = sec.Delete(dp.ID)
				dp.APIKey = ""
				dp.KeychainStored = false
				c.Profiles[i].KeychainStored = false
			}
		}
		for i := range diskCopy.MCPServers {
			dms := &diskCopy.MCPServers[i]
			memTok := strings.TrimSpace(c.MCPServers[i].BearerToken)
			secKey := "mcp:" + dms.ID
			if memTok != "" {
				if err := sec.Set(secKey, memTok); err == nil {
					dms.BearerToken = ""
					dms.KeychainStored = true
					c.MCPServers[i].KeychainStored = true
				} else {
					dms.KeychainStored = false
					c.MCPServers[i].KeychainStored = false
				}
			} else {
				_ = sec.Delete(secKey)
				dms.BearerToken = ""
				dms.KeychainStored = false
				c.MCPServers[i].KeychainStored = false
			}
		}
	}

	data, err := json.MarshalIndent(&diskCopy, "", "  ")
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
