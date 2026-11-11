package ui

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/sky-valley/pi/coding"

	"piui/internal/harness"
	"piui/internal/mcp"
	"piui/internal/store"
)

var (
	formatLabels = []string{"Anthropic Messages", "OpenAI Chat Completions", "OpenAI Responses"}
	formatValues = []store.APIFormat{
		store.FormatAnthropicMessages,
		store.FormatOpenAICompletions,
		store.FormatOpenAIResponses,
	}
	thinkingLabels = []string{"Off", "Low", "Medium", "High"}
	thinkingValues = []string{"off", "low", "medium", "high"}

	safetyLabels = []string{"远程高危拦截 (推荐)", "全局高危拦截", "远程严格模式", "关闭拦截"}
	safetyValues = []store.SafetyPolicy{
		store.SafetyPolicyRemoteHighRisk,
		store.SafetyPolicyAlwaysHighRisk,
		store.SafetyPolicyStrictRemote,
		store.SafetyPolicyOff,
	}
)

// SettingsModal manages the state of the API, Workspace, and Safety configuration dialog.
// It edits an isolated draft copy of AppConfig so cancelling the dialog does not
// leave unsaved mutations in live application state.
type SettingsModal struct {
	Open       bool
	ActiveTab  int
	ShowAPIKey bool

	editing   bool
	draft     store.AppConfig
	saveError string

	// Model fetching & live connection test state (per profile ID)
	fetchingModels bool
	testingConn    bool
	fetchStatus    string
	fetchError     string
	fetchedModels  map[string][]string
	modelSearch    string

	// New workspace target draft & SSH browser state
	NewTargetRemote   bool
	NewTargetName     string
	NewTargetWorkDir  string
	NewTargetSSHHost  string
	browsingRemote    bool
	remoteDirsLoaded  bool
	remoteResolvedDir string
	remoteSubdirs     []string
	remoteProbeStatus string
	remoteProbeError  string

	// MCP server draft & live tool discovery state
	NewMCPHTTP    bool
	NewMCPName    string
	NewMCPCmdLine string
	NewMCPEnv     string
	NewMCPURL     string
	NewMCPBearer  string
	probingMCP    bool
	mcpProbeNote  string
	mcpProbeError string
	mcpProbeCache map[string]mcp.ServerStatus
}

// Render draws the Antigravity settings modal with fixed height so tab switches never jump.
func (s *SettingsModal) Render(
	c *ui.Context,
	p Palette,
	win *mygo.Window,
	cfg *store.AppConfig,
	mgr *harness.Manager,
	updateUI func(func()),
	onApply func() error,
) {
	if !s.Open {
		s.editing = false
		s.saveError = ""
		return
	}
	if !s.editing {
		s.draft = cfg.Clone()
		if s.fetchedModels == nil {
			s.fetchedModels = map[string][]string{}
		}
		if s.mcpProbeCache == nil {
			s.mcpProbeCache = map[string]mcp.ServerStatus{}
		}
		if mgr != nil {
			for _, st := range mgr.MCPStatuses() {
				s.mcpProbeCache[st.ID] = st
			}
		}
		s.fetchStatus = ""
		s.fetchError = ""
		s.saveError = ""
		s.remoteProbeStatus = ""
		s.remoteProbeError = ""
		s.mcpProbeNote = ""
		s.mcpProbeError = ""
		s.remoteDirsLoaded = false
		s.editing = true
	}

	var skills []coding.Skill
	if mgr != nil {
		skills = mgr.DiscoverSkills()
	}

	AppDialog(c, p, &s.Open, 640, 560, func() {
		ui.Column(c).Fill().Children(func() {
			// Edge-to-Edge Modal Header (48px)
			ui.Row(c).FillWidth().Height(48).Padding(0, 14, 0, 20).Shrink(0).
				AlignItems(ui.Center).Gap(8).
				BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).Children(func() {
				ui.Icon(c, IconSettings).Size(15, 15).TextColor(p.TextSecondary)
				ui.Text(c, "偏好设置").FontWeight(WeightSemibold).FontSize(FontTitle).TextColor(p.TextPrimary)
				ui.Text(c, "渠道模型 · 工作区 · 安全 · MCP").FontSize(FontSmall).TextColor(p.TextMuted).Padding(0, 4)
				ui.Spacer(c)
				IconTipBtn(c, p, IconCross, 28, 14, "关闭设置 (Esc)", func() {
					s.Open = false
					s.editing = false
				})
			})

			// Fixed 4-Segment Navigation Bar
			ui.Column(c).FillWidth().Shrink(0).Padding(12, 20, 6, 20).Children(func() {
				tabTitles := []string{
					"渠道与模型",
					fmt.Sprintf("工作区 (%d)", len(s.draft.Targets)),
					fmt.Sprintf("安全与 Skills (%d)", len(skills)),
					fmt.Sprintf("MCP 扩展 (%d)", len(s.draft.MCPServers)),
				}
				SegmentedBar(c, p, &s.ActiveTab, tabTitles)
			})

			// Scrollable Tab Content Body (absorbs height differences so modal never jumps!)
			ui.Scroll(c).Grow(1).FillWidth().Children(func() {
				ui.Column(c).FillWidth().Padding(8, 20, 16, 20).Children(func() {
					switch s.ActiveTab {
					case 0:
						s.renderProviderTab(c, p, &s.draft, updateUI)
					case 1:
						s.renderTargetsTab(c, p, win, &s.draft, cfg, updateUI, onApply)
					case 2:
						s.renderSafetyAndSkillsTab(c, p, &s.draft, mgr, skills)
					default:
						s.renderMCPTab(c, p, &s.draft, cfg, mgr, updateUI, onApply)
					}
				})
			})

			if s.saveError != "" {
				ui.Row(c).FillWidth().Shrink(0).Margin(0, 20, 8, 20).Padding(7, 10).Radius(RadiusSM).
					Background(p.DangerSoft).Border(1, p.DangerColor.Alpha(0.4)).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.Icon(c, IconShieldAlert).Size(13.5, 13.5).TextColor(p.DangerColor).Shrink(0)
					ui.Text(c, s.saveError).FontSize(FontCaption).TextColor(p.DangerColor).Grow(1).MinWidth(0).SingleLine().Ellipsis("...")
				})
			}

			// Edge-to-Edge Modal Footer (52px)
			ui.Row(c).FillWidth().Height(52).Padding(0, 20).Shrink(0).
				AlignItems(ui.Center).Gap(10).Background(p.BgSidebar).
				BorderWidth(1, 0, 0, 0).BorderColor(p.BorderSubtle).Children(func() {
				ui.Text(c, store.ShortenHomePath(store.ConfigPath())).
					Font(FontMono).FontSize(FontCaption).TextColor(p.TextMuted).
					Grow(1).MinWidth(0).SingleLine().Ellipsis("...")
				GhostBtn(c, p, nil, "取消", "放弃未保存的更改", func() {
					s.Open = false
					s.editing = false
				})
				PrimaryBtn(c, p, nil, "保存更改", "保存并立即应用当前配置", func() {
					if err := s.commitToConfig(cfg, onApply); err != nil {
						s.saveError = err.Error()
						c.Toast(s.saveError)
						return
					}
					s.saveError = ""
					s.Open = false
					s.editing = false
					c.Toast("配置已保存并立即生效")
				})
			})
		})
	})

	if !s.Open {
		s.editing = false
	}
}

func (s *SettingsModal) commitToConfig(cfg *store.AppConfig, onApply func() error) error {
	for i := range s.draft.Profiles {
		p := &s.draft.Profiles[i]
		p.Models = p.AvailableModels()
		if strings.TrimSpace(p.ModelID) == "" && len(p.Models) > 0 {
			p.ModelID = p.Models[0]
		}
	}

	cfg.ActiveProfileID = s.draft.ActiveProfileID
	cfg.Profiles = make([]store.ProviderProfile, len(s.draft.Profiles))
	for i, p := range s.draft.Profiles {
		pCopy := p
		if len(p.Models) > 0 {
			pCopy.Models = make([]string, len(p.Models))
			copy(pCopy.Models, p.Models)
		}
		cfg.Profiles[i] = pCopy
	}

	cfg.ActiveTargetID = s.draft.ActiveTargetID
	cfg.Targets = make([]store.WorkspaceTarget, len(s.draft.Targets))
	copy(cfg.Targets, s.draft.Targets)

	cfg.MCPServers = make([]store.MCPServerConfig, len(s.draft.MCPServers))
	for i, ms := range s.draft.MCPServers {
		cfg.MCPServers[i] = ms.Clone()
	}

	cfg.SafetyPolicy = s.draft.SafetyPolicy
	if cfg.SafetyPolicy == "" {
		cfg.SafetyPolicy = store.SafetyPolicyRemoteHighRisk
	}

	_ = cfg.ActiveProfile()
	_ = cfg.ActiveTarget()
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("保存配置文件失败: %w", err)
	}
	if onApply != nil {
		if err := onApply(); err != nil {
			return fmt.Errorf("应用配置失败: %w", err)
		}
	}
	return nil
}

func (s *SettingsModal) triggerTestConnection(prof store.ProviderProfile, updateUI func(func())) {
	if s.testingConn {
		return
	}
	s.testingConn = true
	s.fetchStatus = ""
	s.fetchError = ""

	runUpdate := func(fn func()) {
		if updateUI != nil {
			updateUI(fn)
		} else {
			fn()
		}
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
		defer cancel()

		res, err := harness.TestProviderConnection(ctx, prof)
		runUpdate(func() {
			s.testingConn = false
			if err != nil {
				s.fetchError = "连接测试失败: " + err.Error()
				return
			}
			s.fetchStatus = "✓ " + res.Message
		})
	}()
}

func (s *SettingsModal) triggerFetchModels(prof store.ProviderProfile, updateUI func(func())) {
	if s.fetchingModels {
		return
	}
	s.fetchingModels = true
	s.fetchStatus = ""
	s.fetchError = ""
	profID := prof.ID

	runUpdate := func(fn func()) {
		if updateUI != nil {
			updateUI(fn)
		} else {
			fn()
		}
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		models, err := harness.FetchModels(ctx, prof)
		runUpdate(func() {
			s.fetchingModels = false
			if err != nil {
				s.fetchError = "拉取模型失败: " + err.Error()
				return
			}
			if s.fetchedModels == nil {
				s.fetchedModels = map[string][]string{}
			}
			s.fetchedModels[profID] = models
			s.fetchStatus = fmt.Sprintf("已从 /v1/models 拉取 %d 个模型，勾选后即可在主界面快捷切换", len(models))

			for i := range s.draft.Profiles {
				if s.draft.Profiles[i].ID == profID {
					dp := &s.draft.Profiles[i]
					if strings.TrimSpace(dp.ModelID) == "" && len(models) > 0 {
						dp.ModelID = models[0]
					}
					if len(dp.Models) <= 1 {
						for _, m := range models {
							dp.AddModel(m)
						}
					} else {
						dp.AddModel(dp.ModelID)
					}
					break
				}
			}
		})
	}()
}

func (s *SettingsModal) triggerBrowseLocalDir(win *mygo.Window, updateUI func(func())) {
	runUpdate := func(fn func()) {
		if updateUI != nil {
			updateUI(fn)
		} else {
			fn()
		}
	}
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent:            win,
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
		runUpdate(func() {
			s.NewTargetWorkDir = picked
			if strings.TrimSpace(s.NewTargetName) == "" {
				s.NewTargetName = filepath.Base(picked)
			}
		})
	}()
}

func (s *SettingsModal) triggerBrowseRemoteDir(targetDir string, updateUI func(func())) {
	sshHost := strings.TrimSpace(s.NewTargetSSHHost)
	if sshHost == "" || s.browsingRemote {
		if sshHost == "" {
			s.remoteProbeError = "请先手动填写 SSH 主机地址"
		}
		return
	}
	if strings.TrimSpace(targetDir) == "" {
		targetDir = strings.TrimSpace(s.NewTargetWorkDir)
	}
	if targetDir == "" {
		targetDir = "~"
	}

	s.browsingRemote = true
	s.remoteProbeError = ""
	s.remoteProbeStatus = ""

	runUpdate := func(fn func()) {
		if updateUI != nil {
			updateUI(fn)
		} else {
			fn()
		}
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		st := harness.ProbeSSHWorkspace(ctx, sshHost, targetDir)
		if !st.Connected {
			runUpdate(func() {
				s.browsingRemote = false
				s.remoteProbeError = "SSH 连接或目录检测失败: " + st.Error
			})
			return
		}
		resolved, dirs, listErr := harness.ListRemoteDirectories(ctx, sshHost, st.ResolvedWorkDir)
		runUpdate(func() {
			s.browsingRemote = false
			if listErr != nil {
				s.remoteProbeError = "列出远端目录失败: " + listErr.Error()
				return
			}
			s.remoteDirsLoaded = true
			s.remoteResolvedDir = resolved
			s.remoteSubdirs = dirs
			s.NewTargetWorkDir = resolved
			osInfo := st.RemoteOS
			if osInfo == "" {
				osInfo = "SSH"
			}
			s.remoteProbeStatus = fmt.Sprintf("✓ 已连接 %s (%s · %dms)", sshHost, osInfo, st.LatencyMs)
		})
	}()
}

func (s *SettingsModal) candidateModelsFor(prof *store.ProviderProfile) []string {
	if prof == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, m := range prof.AvailableModels() {
		add(m)
	}
	if s.fetchedModels != nil {
		for _, m := range s.fetchedModels[prof.ID] {
			add(m)
		}
	}
	return out
}

func (s *SettingsModal) renderProviderTab(c *ui.Context, p Palette, draft *store.AppConfig, updateUI func(func())) {
	ui.Column(c).FillWidth().Gap(12).Children(func() {
		// Channel pill strip — neutral by default, crisp active indicator
		ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
			for i := range draft.Profiles {
				profItem := &draft.Profiles[i]
				active := profItem.ID == draft.ActiveProfileID
				profID := profItem.ID
				profName := profItem.Name

				b := ui.ButtonBase(c).Height(28).Padding(0, 11).Radius(RadiusSM).Gap(6).AlignItems(ui.Center)
				bg := p.InputBg
				fg := p.TextSecondary
				borderCol := p.BorderSubtle
				if active {
					bg = p.BgElevated
					if p.IsLight {
						bg = p.BgSurface
					}
					fg = p.TextPrimary
					borderCol = p.BorderStrong
				} else if b.Hovered() {
					bg = p.BgHover
					fg = p.TextPrimary
				}
				b.Background(bg).Border(1, borderCol).Children(func() {
					if active {
						ui.Box(c).Size(5, 5).Radius(RadiusPill).Background(p.Accent)
					}
					el := ui.Text(c, profName).FontSize(FontSmall).TextColor(fg).SingleLine()
					if active {
						el.FontWeight(WeightMedium)
					}
				})
				if b.Clicked() {
					draft.ActiveProfileID = profID
					s.fetchStatus = ""
					s.fetchError = ""
				}
			}

			ui.Spacer(c)

			ToolbarIconBtn(c, p, IconPlus, 28, 14, false, ui.Transparent, "新增模型渠道", func() {
				id := fmt.Sprintf("channel-%d", time.Now().Unix()%10000)
				draft.Profiles = append(draft.Profiles, store.ProviderProfile{
					ID:            id,
					Name:          "新建渠道",
					Format:        store.FormatOpenAICompletions,
					BaseURL:       "https://api.openai.com/v1",
					ModelID:       "",
					Models:        nil,
					ThinkingLevel: "medium",
				})
				draft.ActiveProfileID = id
				s.fetchStatus = ""
				s.fetchError = ""
			})

			if len(draft.Profiles) > 1 {
				DangerIconBtn(c, p, IconTrash, 28, 13.5, "删除当前渠道", func() {
					for i := range draft.Profiles {
						if draft.Profiles[i].ID == draft.ActiveProfileID {
							draft.Profiles = append(draft.Profiles[:i], draft.Profiles[i+1:]...)
							break
						}
					}
					_ = draft.ActiveProfile()
					s.fetchStatus = ""
					s.fetchError = ""
				})
			}
		})

		prof := draft.ActiveProfile()
		if prof == nil {
			return
		}

		fmtIdx := 0
		for i, fv := range formatValues {
			if prof.Format == fv {
				fmtIdx = i
				break
			}
		}

		thinkIdx := 2
		for i, tv := range thinkingValues {
			if prof.ThinkingLevel == tv {
				thinkIdx = i
				break
			}
		}

		candidates := s.candidateModelsFor(prof)

		// 2-column Name & BaseURL
		ui.Row(c).FillWidth().Gap(12).Children(func() {
			ui.Column(c).Grow(1).Gap(5).Children(func() {
				ui.Text(c, "渠道名称").FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextSecondary)
				ui.TextInput(c, &prof.Name).FillWidth().Placeholder("例如：OpenRouter / DeepSeek / 聚合网关")
			})
			ui.Column(c).Grow(1).Gap(5).Children(func() {
				basePlaceholder := "https://api.openai.com/v1"
				if prof.Format == store.FormatAnthropicMessages {
					basePlaceholder = "https://api.anthropic.com"
				}
				ui.Text(c, "接口地址 (Base URL)").FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextSecondary)
				ui.TextInput(c, &prof.BaseURL).FillWidth().Font(FontMono).Placeholder(basePlaceholder)
			})
		})

		// Wire format 3-way segmented bar
		ui.Column(c).FillWidth().Gap(5).Children(func() {
			ui.Text(c, "接口协议格式").FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextSecondary)
			prevFmt := fmtIdx
			SegmentedBar(c, p, &fmtIdx, formatLabels)
			if fmtIdx != prevFmt {
				prof.Format = formatValues[fmtIdx]
			}
		})

		// API Key input + System Keychain indicator
		ui.Column(c).FillWidth().Gap(5).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(6).Children(func() {
				ui.Text(c, "API Key").FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextSecondary)
				if prof.KeychainStored || (store.ActiveSecretStore() != nil && strings.TrimSpace(prof.APIKey) != "") {
					ui.Row(c).Padding(1, 6).Radius(RadiusSM).Background(p.LocalSoft).Gap(4).AlignItems(ui.Center).Children(func() {
						ui.Icon(c, IconShieldCheck).Size(10.5, 10.5).TextColor(p.LocalColor)
						ui.Text(c, "系统钥匙串加密保护").FontSize(FontCaption).TextColor(p.LocalColor)
					})
				} else {
					ui.Text(c, "留空则自动读取环境变量 ANTHROPIC_API_KEY / OPENAI_API_KEY").FontSize(FontCaption).TextColor(p.TextMuted)
				}
			})
			ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Children(func() {
				in := ui.TextInput(c, &prof.APIKey).Grow(1).Font(FontMono).Placeholder("sk-...")
				if !s.ShowAPIKey {
					in.Password()
				}
				eyeIcon := IconEye
				eyeTip := "显示 API Key"
				if s.ShowAPIKey {
					eyeIcon = IconEyeOff
					eyeTip = "隐藏 API Key"
				}
				ToolbarIconBtn(c, p, eyeIcon, 30, 14, s.ShowAPIKey, ui.Transparent, eyeTip, func() {
					s.ShowAPIKey = !s.ShowAPIKey
				})
			})
		})

		// Models Management Card
		ui.Column(c).FillWidth().Padding(10, 12).Radius(RadiusMD).
			Background(p.BgSurface).Border(1, p.BorderSubtle).Gap(8).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(6).Children(func() {
				ui.Text(c, fmt.Sprintf("启用模型 (%d/%d)", len(prof.AvailableModels()), len(candidates))).
					FontWeight(WeightMedium).FontSize(FontSmall).FontFeatures("tnum").TextColor(p.TextPrimary)
				ui.Spacer(c)

				if len(candidates) > 0 {
					IconTipBtn(c, p, IconCheckAll, 26, 13.5, "全选候选模型", func() {
						q := strings.ToLower(strings.TrimSpace(s.modelSearch))
						for _, cand := range candidates {
							if q == "" || strings.Contains(strings.ToLower(cand), q) {
								prof.AddModel(cand)
							}
						}
					})
					IconTipBtn(c, p, IconFilter, 26, 13.5, "仅保留当前默认模型", func() {
						cur := strings.TrimSpace(prof.ModelID)
						if cur != "" {
							prof.Models = []string{cur}
						} else {
							prof.Models = nil
						}
					})
				}

				// One-click Live Connection Test Button
				tb := ui.ButtonBase(c).Height(26).Padding(0, 9).Radius(RadiusSM).
					Border(1, p.BorderSubtle).Gap(5).
					AlignItems(ui.Center).Justify(ui.Center).Disabled(s.testingConn).
					Tooltip("发送最小探测请求验证接口地址、协议与密钥").Label("测试连接")
				tBg := p.InputBg
				if tb.Hovered() && !s.testingConn {
					tBg = p.BgHover
				}
				tb.Background(tBg).Children(func() {
					if s.testingConn {
						ui.Spinner(c).Size(11.5, 11.5)
					} else {
						ui.Icon(c, IconBolt).Size(12, 12).TextColor(p.LocalColor)
					}
					ui.Text(c, "测试连接").FontSize(FontCaption).FontWeight(WeightMedium).TextColor(p.TextSecondary)
				})
				if tb.Clicked() {
					s.triggerTestConnection(*prof, updateUI)
				}

				fb := ui.ButtonBase(c).Height(26).Padding(0, 9).Radius(RadiusSM).
					Border(1, p.BorderSubtle).Gap(5).
					AlignItems(ui.Center).Justify(ui.Center).Disabled(s.fetchingModels).
					Tooltip("从 /v1/models 拉取模型列表").Label("从 /v1/models 拉取模型列表")
				fBg := p.InputBg
				if fb.Hovered() && !s.fetchingModels {
					fBg = p.BgHover
				}
				fb.Background(fBg).Children(func() {
					if s.fetchingModels {
						ui.Spinner(c).Size(11.5, 11.5)
					} else {
						ui.Icon(c, IconRefresh).Size(12, 12).TextColor(p.Accent)
					}
					ui.Text(c, "拉取模型").FontSize(FontCaption).FontWeight(WeightMedium).TextColor(p.TextSecondary)
				})
				if fb.Clicked() {
					s.triggerFetchModels(*prof, updateUI)
				}
			})

			// Active Model ID input + Search Filter
			ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Children(func() {
				ui.TextInput(c, &prof.ModelID).Grow(1).Font(FontMono).Placeholder("当前默认模型 ID (如 claude-sonnet-4-5 / gpt-4.1)")
				if trimmed := strings.TrimSpace(prof.ModelID); trimmed != "" && !prof.HasModel(trimmed) {
					ToolbarIconBtn(c, p, IconPlus, 28, 14, false, p.Accent, "加入候选模型列表", func() {
						prof.AddModel(trimmed)
					})
				}
				if len(candidates) > 3 {
					ui.TextInput(c, &s.modelSearch).Width(160).Placeholder("过滤模型...")
				}
			})

			if s.fetchError != "" {
				ui.Text(c, s.fetchError).FontSize(FontCaption).TextColor(p.DangerColor)
			} else if s.fetchStatus != "" {
				ui.Text(c, s.fetchStatus).FontSize(FontCaption).TextColor(p.LocalColor)
			}

			// Model Checklist — Unified AppCheckbox + Star/Default Badge
			if len(candidates) > 0 {
				q := strings.ToLower(strings.TrimSpace(s.modelSearch))
				ui.Scroll(c).MaxHeight(130).FillWidth().Children(func() {
					ui.Column(c).FillWidth().Gap(2).Children(func() {
						shownCount := 0
						for _, cand := range candidates {
							modelName := cand
							if q != "" && !strings.Contains(strings.ToLower(modelName), q) {
								continue
							}
							shownCount++
							checked := prof.HasModel(modelName)
							isCurrent := strings.TrimSpace(prof.ModelID) == modelName

							rowBg := ui.Transparent
							if isCurrent {
								rowBg = p.BgHover
							}
							ui.Row(c).FillWidth().Height(30).Padding(0, 8).Radius(RadiusSM).Background(rowBg).
								AlignItems(ui.Center).Gap(8).Children(func() {
								toggled := checked
								AppCheckbox(c, p, &toggled, modelName, true)
								if toggled != checked {
									if toggled {
										prof.AddModel(modelName)
									} else {
										prof.RemoveModel(modelName)
									}
								}

								ui.Spacer(c)

								if isCurrent {
									ui.Row(c).Padding(2, 7).Radius(RadiusSM).Background(p.AccentSoft).
										AlignItems(ui.Center).Gap(4).
										Tooltip("当前默认使用模型").Children(func() {
										ui.Icon(c, IconStar).Size(10.5, 10.5).TextColor(p.Accent)
										ui.Text(c, "默认").FontSize(FontCaption).FontWeight(WeightMedium).TextColor(p.Accent)
									})
								} else {
									IconTipBtn(c, p, IconStar, 24, 12.5, "设为当前默认模型", func() {
										prof.ModelID = modelName
										prof.AddModel(modelName)
									})
								}
							})
						}
						if shownCount == 0 {
							ui.Text(c, "没有匹配的模型").FontSize(FontCaption).TextColor(p.TextMuted).Padding(4, 8)
						}
					})
				})
			}
		})

		// Thinking Level 4-way segmented bar
		ui.Column(c).FillWidth().Gap(5).Children(func() {
			ui.Text(c, "默认推理思考深度").FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextSecondary)
			prevThink := thinkIdx
			SegmentedBar(c, p, &thinkIdx, thinkingLabels)
			if thinkIdx != prevThink {
				prof.ThinkingLevel = thinkingValues[thinkIdx]
			}
		})
	})
}

func (s *SettingsModal) renderTargetsTab(
	c *ui.Context,
	p Palette,
	win *mygo.Window,
	draft *store.AppConfig,
	liveCfg *store.AppConfig,
	updateUI func(func()),
	onApply func() error,
) {
	ui.Column(c).FillWidth().Gap(12).Children(func() {
		ui.Text(c, "已配置的工作区（支持本地代码仓库与远程 SSH 开发机，各工作区会话独立隔离）：").
			FontSize(FontSmall).TextColor(p.TextSecondary)

		ui.Column(c).FillWidth().Gap(5).Children(func() {
			for i := range draft.Targets {
				wt := &draft.Targets[i]
				isCurrent := wt.ID == draft.ActiveTargetID
				icon := IconFolder
				iconBg := p.InputBg
				iconFg := p.TextSecondary
				if wt.IsRemote {
					icon = IconCloud
					iconBg = p.SSHSoft
					iconFg = p.SSHColor
				}
				ui.Row(c).FillWidth().Padding(8, 10).Radius(RadiusMD).
					Background(p.BgSurface).Border(1, p.BorderSubtle).
					AlignItems(ui.Center).Gap(10).Children(func() {
					ui.Row(c).Size(28, 28).Shrink(0).Radius(RadiusSM).Background(iconBg).
						AlignItems(ui.Center).Justify(ui.Center).Children(func() {
						ui.Icon(c, icon).Size(14, 14).TextColor(iconFg)
					})
					ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
						ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
							ui.Text(c, wt.ShortName()).FontWeight(WeightMedium).FontSize(FontSmall).TextColor(p.TextPrimary).SingleLine().Ellipsis("...")
							if isCurrent {
								ui.Row(c).Padding(1, 5).Radius(RadiusSM).Background(p.AccentSoft).Children(func() {
									ui.Text(c, "当前").FontSize(10).FontWeight(WeightMedium).TextColor(p.Accent)
								})
							}
						})
						ui.Text(c, wt.DisplaySub()).Font(FontMono).FontSize(FontCaption).TextColor(p.TextMuted).SingleLine().Ellipsis("...")
					})

					// Trust toggle button for project .pi/skills & AGENTS.md
					trustBtn := ui.ButtonBase(c).Height(22).Padding(0, 7).Radius(RadiusSM).Gap(4).AlignItems(ui.Center).Shrink(0).
						Tooltip("切换是否信任加载该工作区下的项目级 .pi/skills 与 AGENTS.md")
					if wt.Trusted {
						trustBtn.Background(p.LocalSoft).Children(func() {
							ui.Icon(c, IconShieldCheck).Size(10.5, 10.5).TextColor(p.LocalColor)
							ui.Text(c, "已信任").FontSize(10).FontWeight(WeightMedium).TextColor(p.LocalColor)
						})
					} else {
						tBg := p.InputBg
						if trustBtn.Hovered() {
							tBg = p.BgHover
						}
						trustBtn.Background(tBg).Border(1, p.BorderSubtle).Children(func() {
							ui.Text(c, "未信任").FontSize(10).TextColor(p.TextMuted)
						})
					}
					if trustBtn.Clicked() {
						wt.Trusted = !wt.Trusted
					}

					if wt.IsRemote {
						ui.Row(c).Padding(2, 7).Radius(RadiusSM).Background(p.SSHSoft).Shrink(0).Children(func() {
							ui.Text(c, "SSH").Font(FontMono).FontWeight(WeightMedium).FontSize(10).TextColor(p.SSHColor)
						})
					} else {
						ui.Text(c, "本地").FontSize(FontCaption).TextColor(p.TextMuted).Shrink(0)
					}

					if len(draft.Targets) > 1 {
						idx := i
						DangerIconBtn(c, p, IconTrash, 26, 13.5, "移除该工作区", func() {
							removedID := draft.Targets[idx].ID
							_ = draft.RemoveTarget(removedID)
						})
					}
				})
			}
		})

		// Add New Workspace Card (Local Folder Picker + Manual SSH Entry & Remote Browser)
		ui.Column(c).FillWidth().Padding(12, 14).Radius(RadiusMD).
			Background(p.BgSurface).Border(1, p.BorderSubtle).Gap(10).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
				ui.Text(c, "添加工作区").FontWeight(WeightMedium).FontSize(FontSmall).TextColor(p.TextPrimary)
				ui.Spacer(c)
				prevRemote := s.NewTargetRemote
				AppCheckbox(c, p, &s.NewTargetRemote, "远程 SSH 服务器 (手动填写主机与目录)", false)
				if s.NewTargetRemote != prevRemote {
					s.remoteDirsLoaded = false
					s.remoteProbeError = ""
					s.remoteProbeStatus = ""
				}
			})

			ui.Row(c).FillWidth().Gap(8).Children(func() {
				ui.TextInput(c, &s.NewTargetName).Grow(1).Placeholder("显示名称 (可选，如 gateway-prod)")
				if s.NewTargetRemote {
					ui.TextInput(c, &s.NewTargetSSHHost).Grow(1).Font(FontMono).Placeholder("SSH Host 别名或 user@ip")
				}
			})

			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				dirHint := "本地目录路径 (如 ~/Documents/code/myproj)"
				if s.NewTargetRemote {
					dirHint = "远端项目目录 (如 /srv/gateway 或 ~)"
				}
				ui.TextInput(c, &s.NewTargetWorkDir).Grow(1).Font(FontMono).Placeholder(dirHint)

				if !s.NewTargetRemote {
					GhostBtn(c, p, IconFolder, "浏览...", "打开原生文件夹选择器", func() {
						s.triggerBrowseLocalDir(win, updateUI)
					})
				} else {
					bb := ui.ButtonBase(c).Height(30).Padding(0, 10).Radius(RadiusSM).
						Border(1, p.BorderSubtle).Gap(5).AlignItems(ui.Center).Disabled(s.browsingRemote).
						Tooltip("测试 SSH 连通性并浏览远端目录")
					bBg := p.InputBg
					if bb.Hovered() && !s.browsingRemote {
						bBg = p.BgHover
					}
					bb.Background(bBg).Children(func() {
						if s.browsingRemote {
							ui.Spinner(c).Size(12, 12)
						} else {
							ui.Icon(c, IconFolder).Size(13, 13).TextColor(p.SSHColor)
						}
						ui.Text(c, "浏览远端").FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextSecondary)
					})
					if bb.Clicked() {
						s.triggerBrowseRemoteDir("", updateUI)
					}
				}

				PrimaryBtn(c, p, IconPlus, "添加", "添加工作区目标", func() {
					workDir := strings.TrimSpace(s.NewTargetWorkDir)
					sshHost := strings.TrimSpace(s.NewTargetSSHHost)
					name := strings.TrimSpace(s.NewTargetName)
					if s.NewTargetRemote {
						if sshHost == "" {
							s.saveError = "请先手动输入远程 SSH 主机地址"
							return
						}
						if workDir == "" {
							workDir = "~"
						}
						added := draft.UpsertSSHTarget(sshHost, workDir, name, false)
						_ = liveCfg.UpsertSSHTarget(sshHost, workDir, added.Name, false)
						name = added.ShortName()
					} else {
						if workDir == "" {
							s.saveError = "请先选择或输入本地项目目录路径"
							return
						}
						added := draft.UpsertLocalTarget(workDir, name, false)
						_ = liveCfg.UpsertLocalTarget(workDir, added.Name, false)
						name = added.ShortName()
					}

					s.NewTargetName = ""
					s.NewTargetWorkDir = ""
					s.NewTargetSSHHost = ""
					s.remoteDirsLoaded = false
					s.remoteProbeStatus = ""
					s.remoteProbeError = ""

					if err := liveCfg.Save(); err != nil {
						s.saveError = "保存新增工作区失败: " + err.Error()
						c.Toast(s.saveError)
					} else if onApply != nil {
						if err := onApply(); err != nil {
							s.saveError = "应用工作区失败: " + err.Error()
							c.Toast(s.saveError)
						} else {
							s.saveError = ""
							c.Toast("已添加工作区: " + name)
						}
					}
				})
			})

			if s.NewTargetRemote {
				if s.remoteProbeError != "" {
					ui.Text(c, s.remoteProbeError).FontSize(FontCaption).TextColor(p.DangerColor)
				} else if s.remoteProbeStatus != "" {
					ui.Text(c, s.remoteProbeStatus).FontSize(FontCaption).TextColor(p.LocalColor)
				}

				if s.remoteDirsLoaded {
					ui.Column(c).FillWidth().Padding(8, 10).Radius(RadiusSM).
						Background(p.InputBg).Border(1, p.BorderSubtle).Gap(5).Children(func() {
						ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(6).Children(func() {
							ui.Icon(c, IconServer).Size(12, 12).TextColor(p.SSHColor)
							ui.Text(c, s.remoteResolvedDir).Font(FontMono).FontSize(FontCaption).FontWeight(WeightMedium).
								TextColor(p.TextPrimary).Grow(1).MinWidth(0).SingleLine().Ellipsis("...")
							if s.remoteResolvedDir != "/" && s.remoteResolvedDir != "" {
								parentDir := path.Dir(strings.TrimRight(s.remoteResolvedDir, "/"))
								if parentDir == "" || parentDir == "." {
									parentDir = "/"
								}
								upBtn := ui.ButtonBase(c).Height(22).Padding(0, 7).Radius(RadiusSM).
									Border(1, p.BorderSubtle).AlignItems(ui.Center)
								if upBtn.Hovered() {
									upBtn.Background(p.BgHover)
								}
								upBtn.Children(func() {
									ui.Text(c, "↑ 上级目录").FontSize(FontCaption).TextColor(p.TextSecondary)
								})
								if upBtn.Clicked() {
									s.triggerBrowseRemoteDir(parentDir, updateUI)
								}
							}
						})

						ui.Scroll(c).MaxHeight(110).FillWidth().Children(func() {
							ui.Row(c).FillWidth().Gap(4).Wrap().Children(func() {
								if len(s.remoteSubdirs) == 0 {
									ui.Text(c, "当前目录下无子文件夹，可直接点击右侧「添加」使用此目录").
										FontSize(FontCaption).TextColor(p.TextMuted)
								}
								for _, sub := range s.remoteSubdirs {
									subName := sub
									nextPath := path.Join(s.remoteResolvedDir, subName)
									db := ui.ButtonBase(c).Height(24).Padding(0, 8).Radius(RadiusSM).Gap(4).AlignItems(ui.Center)
									dBg := p.BgSurface
									if db.Hovered() {
										dBg = p.BgHover
									}
									db.Background(dBg).Border(1, p.BorderSubtle).Children(func() {
										ui.Icon(c, IconFolder).Size(11, 11).TextColor(p.TextMuted)
										ui.Text(c, subName+"/").Font(FontMono).FontSize(FontCaption).TextColor(p.TextPrimary)
									})
									if db.Clicked() {
										s.triggerBrowseRemoteDir(nextPath, updateUI)
									}
								}
							})
						})
					})
				}
			}
		})
	})
}

func (s *SettingsModal) renderSafetyAndSkillsTab(c *ui.Context, p Palette, draft *store.AppConfig, mgr *harness.Manager, skills []coding.Skill) {
	policyIdx := 0
	for i, pv := range safetyValues {
		if draft.SafetyPolicy == pv {
			policyIdx = i
			break
		}
	}

	ui.Column(c).FillWidth().Gap(12).Children(func() {
		ui.Column(c).FillWidth().Gap(6).Children(func() {
			ui.Text(c, "高危操作弹窗拦截策略 (Human-in-the-Loop Safety Gate)").
				FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextSecondary)
			prevPol := policyIdx
			SegmentedBar(c, p, &policyIdx, safetyLabels)
			if policyIdx != prevPol {
				draft.SafetyPolicy = safetyValues[policyIdx]
			}
		})

		policyDesc := "远程高危拦截（推荐）：在远程 SSH 服务器上拦截 rm -rf、systemctl 关停、kill -9、防火墙/数据库破坏操作以及 /etc、~/.ssh、.env 等敏感文件修改（含重定向与内联脚本绕过检测）；在本地环境仅拦截 rm -rf /、mkfs、敏感系统路径写入等高危指令。"
		accentCol := p.LocalColor
		switch draft.SafetyPolicy {
		case store.SafetyPolicyAlwaysHighRisk:
			policyDesc = "全局高危拦截：无论在本地目录还是远程 SSH 服务器，遇到高危 bash 命令与敏感配置文件修改均弹窗要求人工确认。"
			accentCol = p.Accent
		case store.SafetyPolicyStrictRemote:
			policyDesc = "远程严格模式：在远程 SSH 服务器上，除 ls/cat/grep/git status 等纯只读查询命令外，所有文件写入/修改 (write/edit) 与变更命令均需弹窗审批。"
			accentCol = p.SSHColor
		case store.SafetyPolicyOff:
			policyDesc = "关闭拦截：所有工具调用均自动放行，不弹出安全确认框（请谨慎在生产服务器开启）。"
			accentCol = p.WarnColor
		}
		ui.Row(c).FillWidth().Radius(RadiusMD).
			Background(p.BgSurface).Border(1, p.BorderSubtle).Clip().Children(func() {
			ui.Box(c).Width(3).FillHeight().Shrink(0).Background(accentCol)
			ui.Column(c).Grow(1).MinWidth(0).Padding(10, 14).Children(func() {
				ui.Text(c, policyDesc).FontSize(FontSmall).LineHeight(1.55).TextColor(p.TextSecondary)
			})
		})

		if mgr != nil && mgr.SafetyGate() != nil {
			trustedCount := mgr.SafetyGate().TrustedCount()
			ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
				ui.Text(c, fmt.Sprintf("当前会话已记住免审的操作签名数：%d", trustedCount)).
					FontSize(FontSmall).FontFeatures("tnum").TextColor(p.TextSecondary)
				ui.Spacer(c)
				if trustedCount > 0 {
					DangerIconBtn(c, p, IconTrash, 26, 13.5, "清空本会话免审白名单", func() {
						mgr.SafetyGate().ClearTrusted()
						c.Toast("已清空本会话免审白名单")
					})
				}
			})
		}

		// Active Skills discovery display (Local or Remote SSH workspace)
		isRemoteTarget := mgr != nil && mgr.Router().Target().IsRemote
		ui.Column(c).FillWidth().Gap(6).Padding(4, 0, 0, 0).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf("当前工作区已加载的 Skills (%d)", len(skills))).
					FontWeight(WeightMedium).FontSize(FontSmall).FontFeatures("tnum").TextColor(p.TextPrimary)
				ui.Spacer(c)
				ui.Text(c, ".pi/skills · ~/.pi/agent/skills").Font(FontMono).FontSize(FontCaption).TextColor(p.TextMuted)
			})

			if len(skills) == 0 {
				hint := "当前工作区未加载自定义 SKILL.md，可在项目 .pi/skills/<name>/SKILL.md 中添加专属技能。"
				if mgr != nil && mgr.HasUntrustedProjectSkills() {
					hint = "检测到当前工作区存在项目级 .pi/skills，但工作区尚未标记为「已信任」，可在「工作区」页签或主界面横幅中开启信任后加载。"
				}
				ui.Column(c).FillWidth().Padding(12, 14).Radius(RadiusMD).
					Background(p.BgSurface).Border(1, p.BorderSubtle).Children(func() {
					ui.Text(c, hint).FontSize(FontSmall).LineHeight(1.5).TextColor(p.TextMuted)
				})
			} else {
				for _, sk := range skills {
					ui.Column(c).FillWidth().Padding(8, 12).Radius(RadiusMD).
						Background(p.BgSurface).Border(1, p.BorderSubtle).Gap(4).Children(func() {
						ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(6).Children(func() {
							if isRemoteTarget {
								ui.Row(c).Padding(1, 6).Radius(RadiusSM).Background(p.SSHSoft).Children(func() {
									ui.Text(c, "SSH").Font(FontMono).FontWeight(WeightMedium).FontSize(10).TextColor(p.SSHColor)
								})
							}
							ui.Text(c, sk.Name).FontWeight(WeightMedium).FontSize(FontSmall).TextColor(p.TextPrimary)
							ui.Spacer(c)
							ui.Text(c, filepath.Base(filepath.Dir(sk.FilePath))+"/"+filepath.Base(sk.FilePath)).
								Font(FontMono).FontSize(FontCaption).TextColor(p.TextMuted)
						})
						if sk.Description != "" {
							ui.Text(c, sk.Description).FontSize(FontCaption + 0.5).LineHeight(1.45).TextColor(p.TextSecondary).SingleLine().Ellipsis("...")
						}
					})
				}
			}
		})
	})
}

func (s *SettingsModal) buildCandidateMCPConfig() (store.MCPServerConfig, error) {
	name := strings.TrimSpace(s.NewMCPName)
	if s.NewMCPHTTP {
		u := strings.TrimSpace(s.NewMCPURL)
		if u == "" {
			return store.MCPServerConfig{}, fmt.Errorf("请输入远程 MCP HTTP 接口地址 (URL)")
		}
		if name == "" {
			name = "mcp-http"
		}
		return store.MCPServerConfig{
			Name:        name,
			Transport:   store.MCPTransportHTTP,
			Enabled:     true,
			URL:         u,
			BearerToken: strings.TrimSpace(s.NewMCPBearer),
		}, nil
	}
	cmd, args := store.ParseCommandLine(s.NewMCPCmdLine)
	if cmd == "" {
		return store.MCPServerConfig{}, fmt.Errorf("请输入本地 MCP 启动命令（例如 npx -y @modelcontextprotocol/server-filesystem .）")
	}
	if name == "" {
		name = filepath.Base(cmd)
	}
	return store.MCPServerConfig{
		Name:      name,
		Transport: store.MCPTransportStdio,
		Enabled:   true,
		Command:   cmd,
		Args:      args,
		Env:       store.ParseKeyValuePairs(s.NewMCPEnv),
	}, nil
}

func (s *SettingsModal) triggerProbeMCP(srv store.MCPServerConfig, mgr *harness.Manager, updateUI func(func())) {
	if s.probingMCP {
		return
	}
	s.probingMCP = true
	s.mcpProbeNote = ""
	s.mcpProbeError = ""

	runUpdate := func(fn func()) {
		if updateUI != nil {
			updateUI(fn)
		} else {
			fn()
		}
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 14*time.Second)
		defer cancel()
		var st mcp.ServerStatus
		if mgr != nil {
			st = mgr.ProbeMCPServer(ctx, srv)
		} else {
			st = mcp.ProbeServer(ctx, srv, "")
		}
		runUpdate(func() {
			s.probingMCP = false
			if srv.ID != "" {
				if s.mcpProbeCache == nil {
					s.mcpProbeCache = map[string]mcp.ServerStatus{}
				}
				s.mcpProbeCache[srv.ID] = st
			}
			if !st.Connected {
				s.mcpProbeError = fmt.Sprintf("MCP 连接检测失败 (%s): %s", srv.Name, st.Error)
				return
			}
			var toolNames []string
			for _, tl := range st.Tools {
				toolNames = append(toolNames, tl.Name)
			}
			preview := strings.Join(toolNames, ", ")
			if preview == "" {
				preview = "无工具返回"
			}
			s.mcpProbeNote = fmt.Sprintf("✓ %s 已连接 (%dms) · 发现 %d 个工具: %s", srv.Name, st.LatencyMs, len(st.Tools), singleLinePreview(preview, 80))
		})
	}()
}

func (s *SettingsModal) renderMCPTab(
	c *ui.Context,
	p Palette,
	draft *store.AppConfig,
	liveCfg *store.AppConfig,
	mgr *harness.Manager,
	updateUI func(func()),
	onApply func() error,
) {
	ui.Column(c).FillWidth().Gap(12).Children(func() {
		ui.Text(c, "支持挂载外部 Model Context Protocol (MCP) 工具服务（Stdio 本地进程与 Streamable HTTP 远程接口），发现的工具将自动注入 Agent 上下文并纳入高危安全拦截。").
			FontSize(FontSmall).LineHeight(1.5).TextColor(p.TextMuted)

		// Existing MCP servers list
		if len(draft.MCPServers) > 0 {
			ui.Column(c).FillWidth().Gap(6).Children(func() {
				for idx := range draft.MCPServers {
					i := idx
					ms := &draft.MCPServers[i]
					st, hasSt := s.mcpProbeCache[ms.ID]

					ui.Column(c).FillWidth().Padding(9, 12).Radius(RadiusMD).
						Background(p.BgSurface).Border(1, p.BorderSubtle).Gap(5).Children(func() {
						ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
							ui.Icon(c, IconPlug).Size(14, 14).TextColor(map[bool]ui.Color{true: p.Accent, false: p.TextMuted}[ms.Enabled]).Shrink(0)
							ui.Text(c, ms.Name).FontWeight(WeightMedium).FontSize(FontSmall).TextColor(p.TextPrimary).SingleLine()

							trLabel := "STDIO"
							trBg := p.InputBg
							trFg := p.TextSecondary
							if ms.Transport == store.MCPTransportHTTP {
								trLabel = "HTTP"
								trBg = p.SSHSoft
								trFg = p.SSHColor
							}
							ui.Row(c).Padding(1, 6).Radius(RadiusSM).Background(trBg).Shrink(0).Children(func() {
								ui.Text(c, trLabel).Font(FontMono).FontWeight(WeightMedium).FontSize(10).TextColor(trFg)
							})

							if ms.KeychainStored {
								ui.Row(c).Padding(1, 6).Radius(RadiusSM).Background(p.LocalSoft).Shrink(0).Children(func() {
									ui.Text(c, "Keychain").FontSize(10).TextColor(p.LocalColor)
								})
							}

							if hasSt && ms.Enabled {
								if st.Connected {
									ui.Row(c).Padding(1, 6).Radius(RadiusSM).Background(p.LocalSoft).Shrink(0).Children(func() {
										ui.Textf(c, "%d tools", len(st.Tools)).Font(FontMono).FontFeatures("tnum").FontSize(10).TextColor(p.LocalColor)
									})
								} else if st.Error != "" {
									ui.Row(c).Padding(1, 6).Radius(RadiusSM).Background(p.DangerSoft).Shrink(0).
										Tooltip(st.Error).Children(func() {
										ui.Text(c, "连接失败").FontSize(10).TextColor(p.DangerColor)
									})
								}
							}

							ui.Spacer(c)

							enBtn := ui.ButtonBase(c).Height(22).Padding(0, 8).Radius(RadiusSM).AlignItems(ui.Center)
							if ms.Enabled {
								enBtn.Background(p.AccentSoft).Children(func() {
									ui.Text(c, "已启用").FontSize(10.5).FontWeight(WeightMedium).TextColor(p.Accent)
								})
							} else {
								enBtn.Background(p.InputBg).Border(1, p.BorderSubtle).Children(func() {
									ui.Text(c, "已停用").FontSize(10.5).TextColor(p.TextMuted)
								})
							}
							if enBtn.Clicked() {
								ms.Enabled = !ms.Enabled
							}

							IconTipBtn(c, p, IconRefresh, 24, 12.5, "检测连接并刷新工具列表", func() {
								s.triggerProbeMCP(*ms, mgr, updateUI)
							})
							DangerIconBtn(c, p, IconTrash, 24, 12.5, "移除该 MCP 服务", func() {
								removedID := draft.MCPServers[i].ID
								_ = draft.RemoveMCPServer(removedID)
							})
						})

						ui.Text(c, ms.DisplayEndpoint()).Font(FontMono).FontSize(FontCaption).TextColor(p.TextMuted).SingleLine().Ellipsis("...")
					})
				}
			})
		}

		// Add New MCP Server Card
		ui.Column(c).FillWidth().Padding(12, 14).Radius(RadiusMD).
			Background(p.BgSurface).Border(1, p.BorderSubtle).Gap(9).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
				ui.Text(c, "添加 MCP 工具服务").FontWeight(WeightMedium).FontSize(FontSmall).TextColor(p.TextPrimary)
				ui.Spacer(c)
				AppCheckbox(c, p, &s.NewMCPHTTP, "远程 Streamable HTTP 接口", false)
			})

			ui.Row(c).FillWidth().Gap(8).Children(func() {
				ui.TextInput(c, &s.NewMCPName).Width(170).Placeholder("服务名称 (如 github)")
				if s.NewMCPHTTP {
					ui.TextInput(c, &s.NewMCPURL).Grow(1).Font(FontMono).Placeholder("HTTP Endpoint (如 https://example.com/mcp)")
				} else {
					ui.TextInput(c, &s.NewMCPCmdLine).Grow(1).Font(FontMono).Placeholder("命令与参数 (如 npx -y @modelcontextprotocol/server-filesystem .)")
				}
			})

			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				if s.NewMCPHTTP {
					ui.TextInput(c, &s.NewMCPBearer).Grow(1).Font(FontMono).Password().Placeholder("Bearer Token 凭证 (可选，自动存入系统钥匙串)")
				} else {
					ui.TextInput(c, &s.NewMCPEnv).Grow(1).Font(FontMono).Placeholder("环境变量 (可选，格式 KEY=VAL;TOKEN=xxx)")
				}

				GhostBtn(c, p, IconRefresh, "测试发现", "连接该 MCP 服务并探测可用工具", func() {
					cand, err := s.buildCandidateMCPConfig()
					if err != nil {
						s.mcpProbeError = err.Error()
						return
					}
					s.triggerProbeMCP(cand, mgr, updateUI)
				})

				PrimaryBtn(c, p, IconPlus, "添加", "添加并启用该 MCP 服务", func() {
					cand, err := s.buildCandidateMCPConfig()
					if err != nil {
						s.mcpProbeError = err.Error()
						return
					}
					added := draft.UpsertMCPServer(cand)
					_ = liveCfg.UpsertMCPServer(added)

					s.NewMCPName = ""
					s.NewMCPCmdLine = ""
					s.NewMCPEnv = ""
					s.NewMCPURL = ""
					s.NewMCPBearer = ""
					s.mcpProbeError = ""

					if err := liveCfg.Save(); err != nil {
						s.saveError = "保存 MCP 配置失败: " + err.Error()
						c.Toast(s.saveError)
						return
					}
					if onApply != nil {
						_ = onApply()
					}
					c.Toast("已添加 MCP 服务: " + added.Name)
				})
			})

			if s.probingMCP {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.Spinner(c).Size(12, 12)
					ui.Text(c, "正在握手连接并获取 tools/list...").FontSize(FontCaption).TextColor(p.TextMuted)
				})
			} else if s.mcpProbeError != "" {
				ui.Text(c, s.mcpProbeError).FontSize(FontCaption).TextColor(p.DangerColor)
			} else if s.mcpProbeNote != "" {
				ui.Text(c, s.mcpProbeNote).FontSize(FontCaption).TextColor(p.LocalColor)
			}
		})
	})
}
