package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

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
)

// SettingsModal manages the state of the API & Workspace configuration dialog.
// It edits an isolated draft copy of AppConfig so cancelling the dialog does not
// leave unsaved mutations in live application state.
type SettingsModal struct {
	Open       bool
	ActiveTab  int
	ShowAPIKey bool

	editing bool
	draft   store.AppConfig

	// New target draft fields
	NewTargetRemote  bool
	NewTargetName    string
	NewTargetWorkDir string
	NewTargetSSHHost string
}

// Render draws the settings modal when Open is true.
func (s *SettingsModal) Render(c *ui.Context, cfg *store.AppConfig, onApply func()) {
	if !s.Open {
		s.editing = false
		return
	}
	if !s.editing {
		s.draft = cfg.Clone()
		s.editing = true
	}

	ui.Modal(c, &s.Open, func() {
		t := c.Theme()
		ui.Column(c).Width(580).Gap(14).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
				ui.Text(c, "⚙️ 模型接口与工作区设置").Bold().FontSize(17)
				ui.Spacer(c)
				if ui.Button(c, "✕").Clicked() {
					s.Open = false
					s.editing = false
				}
			})

			ui.Tabs(c, &s.ActiveTab, "🔑 模型与自定义 API", "🌐 工作区与 SSH 目标")
			ui.Divider(c)

			if s.ActiveTab == 0 {
				s.renderProviderTab(c, &s.draft)
			} else {
				s.renderTargetsTab(c, &s.draft, cfg, onApply)
			}

			ui.Divider(c)
			ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
				ui.Text(c, "配置文件: "+store.ConfigPath()).FontSize(11).TextColor(t.TextMuted)
				ui.Spacer(c)
				if ui.PrimaryButton(c, "保存并生效").Clicked() {
					s.commitToConfig(cfg, onApply)
					s.Open = false
					s.editing = false
					c.Toast("配置已保存并立即生效")
				}
			})
		})
	})

	if !s.Open {
		s.editing = false
	}
}

func (s *SettingsModal) commitToConfig(cfg *store.AppConfig, onApply func()) {
	cfg.ActiveProfileID = s.draft.ActiveProfileID
	cfg.Profiles = make([]store.ProviderProfile, len(s.draft.Profiles))
	copy(cfg.Profiles, s.draft.Profiles)

	cfg.ActiveTargetID = s.draft.ActiveTargetID
	cfg.Targets = make([]store.WorkspaceTarget, len(s.draft.Targets))
	copy(cfg.Targets, s.draft.Targets)

	_ = cfg.ActiveProfile()
	_ = cfg.ActiveTarget()
	_ = cfg.Save()
	if onApply != nil {
		onApply()
	}
}

func (s *SettingsModal) renderProviderTab(c *ui.Context, draft *store.AppConfig) {
	t := c.Theme()

	// Profile selector row
	ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Children(func() {
		for i := range draft.Profiles {
			p := &draft.Profiles[i]
			active := p.ID == draft.ActiveProfileID
			var btn ui.Element
			if active {
				btn = ui.PrimaryButton(c, p.Name)
			} else {
				btn = ui.Button(c, p.Name)
			}
			if btn.Clicked() {
				draft.ActiveProfileID = p.ID
			}
		}
		ui.Spacer(c)
		if ui.Button(c, "+ 新增预设").Clicked() {
			id := fmt.Sprintf("custom-%d", time.Now().Unix()%10000)
			draft.Profiles = append(draft.Profiles, store.ProviderProfile{
				ID:            id,
				Name:          "自定义接口",
				Format:        store.FormatOpenAICompletions,
				BaseURL:       "https://api.openai.com/v1",
				ModelID:       "gpt-4.1",
				ThinkingLevel: "medium",
			})
			draft.ActiveProfileID = id
		}
	})

	prof := draft.ActiveProfile()
	if prof == nil {
		return
	}

	// Map Format to segmented index
	fmtIdx := 0
	for i, fv := range formatValues {
		if prof.Format == fv {
			fmtIdx = i
			break
		}
	}

	// Map ThinkingLevel to segmented index
	thinkIdx := 2
	for i, tv := range thinkingValues {
		if prof.ThinkingLevel == tv {
			thinkIdx = i
			break
		}
	}

	ui.Column(c).FillWidth().Gap(10).Children(func() {
		ui.Field(c, "预设名称 (Name)", func() {
			ui.TextInput(c, &prof.Name).FillWidth().Placeholder("例如：OpenRouter / 公司内部网关")
		})

		ui.Field(c, "接口协议格式 (API Wire Format)", func() {
			if ui.Segmented(c, &fmtIdx, formatLabels...).Changed() {
				prof.Format = formatValues[fmtIdx]
			}
		})

		basePlaceholder := "留空使用官方默认地址，或填如 https://api.openai.com/v1"
		if prof.Format == store.FormatAnthropicMessages {
			basePlaceholder = "留空使用 https://api.anthropic.com，或填写兼容 Anthropic 的 BaseURL"
		}
		ui.Field(c, "自定义 Base URL (支持官方或聚合中转网关)", func() {
			ui.TextInput(c, &prof.BaseURL).FillWidth().Placeholder(basePlaceholder)
		})

		ui.Field(c, "API Key (留空则自动读取系统环境变量 ANTHROPIC_API_KEY / OPENAI_API_KEY)", func() {
			ui.Row(c).FillWidth().Gap(8).Children(func() {
				in := ui.TextInput(c, &prof.APIKey).Grow(1).Placeholder("sk-...")
				if !s.ShowAPIKey {
					in.Password()
				}
				toggleLabel := "显示"
				if s.ShowAPIKey {
					toggleLabel = "隐藏"
				}
				if ui.Button(c, toggleLabel).Clicked() {
					s.ShowAPIKey = !s.ShowAPIKey
				}
			})
		})

		ui.Field(c, "模型 ID (Model ID)", func() {
			ui.TextInput(c, &prof.ModelID).FillWidth().Placeholder("例如: claude-sonnet-4-5 / gpt-5 / deepseek-chat / kimi-k2.5")
		})

		ui.Field(c, "推理思考深度 (Thinking Level)", func() {
			if ui.Segmented(c, &thinkIdx, thinkingLabels...).Changed() {
				prof.ThinkingLevel = thinkingValues[thinkIdx]
			}
		})

		ui.Text(c, "提示：保存后将立即热更新当前会话的模型与密钥，不会丢失正在进行的对话历史。").
			FontSize(11).
			TextColor(t.TextMuted)
	})
}

func (s *SettingsModal) renderTargetsTab(c *ui.Context, draft *store.AppConfig, liveCfg *store.AppConfig, onApply func()) {
	t := c.Theme()

	ui.Column(c).FillWidth().Gap(10).Children(func() {
		ui.Text(c, "已配置的工作区目标 (支持在同一 Session 对话中随时一键热切换):").Bold().FontSize(13)

		ui.Scroll(c).MaxHeight(180).FillWidth().Children(func() {
			ui.Column(c).FillWidth().Gap(6).Children(func() {
				for i := range draft.Targets {
					wt := &draft.Targets[i]
					tag := "💻 Local"
					sub := wt.WorkDir
					if wt.IsRemote {
						tag = "🌐 SSH (" + wt.SSHHost + ")"
						sub = wt.SSHHost + ":" + wt.WorkDir
					}
					ui.Row(c).FillWidth().Padding(6, 10).Radius(6).Background(t.Surface).Border(1, t.Border).AlignItems(ui.Center).Gap(8).Children(func() {
						ui.Badge(c, tag)
						ui.Text(c, wt.Name).Bold().FontSize(12)
						ui.Text(c, sub).FontSize(11).TextColor(t.TextMuted).Grow(1).SingleLine().Ellipsis("...")
						if len(draft.Targets) > 1 && i > 0 {
							idx := i
							if ui.Button(c, "删除").Clicked() {
								removedID := draft.Targets[idx].ID
								draft.Targets = append(draft.Targets[:idx], draft.Targets[idx+1:]...)
								if draft.ActiveTargetID == removedID {
									_ = draft.ActiveTarget()
								}
							}
						}
					})
				}
			})
		})

		ui.Divider(c)
		ui.Text(c, "添加新工作区目标:").Bold().FontSize(13)
		ui.Checkbox(c, &s.NewTargetRemote, "远程 SSH 服务器 (自动复用本地 ~/.ssh/config 与 ssh-agent)")

		ui.Row(c).FillWidth().Gap(8).Children(func() {
			ui.TextInput(c, &s.NewTargetName).Grow(1).Placeholder("显示名称 (如: 生产服务器 / 前端仓库)")
			if s.NewTargetRemote {
				ui.TextInput(c, &s.NewTargetSSHHost).Grow(1).Placeholder("SSH Host 别名或 user@ip:port")
			}
		})

		ui.Row(c).FillWidth().Gap(8).Children(func() {
			dirHint := "本地绝对路径 (如 ~/Documents/myproj)"
			if s.NewTargetRemote {
				dirHint = "远端工作目录 (如 /root/service 或 ~)"
			}
			ui.TextInput(c, &s.NewTargetWorkDir).Grow(1).Placeholder(dirHint)
			if ui.Button(c, "+ 添加到列表").Clicked() {
				workDir := strings.TrimSpace(s.NewTargetWorkDir)
				sshHost := strings.TrimSpace(s.NewTargetSSHHost)
				name := strings.TrimSpace(s.NewTargetName)
				if workDir == "" && s.NewTargetRemote {
					workDir = "~"
				}
				if !s.NewTargetRemote && workDir != "" {
					workDir = store.ExpandLocalPath(workDir)
				}
				if workDir != "" && (!s.NewTargetRemote || sshHost != "") {
					if name == "" {
						if s.NewTargetRemote {
							name = "SSH: " + sshHost
						} else {
							name = "Local: " + filepath.Base(workDir)
						}
					}
					id := fmt.Sprintf("target-%d", time.Now().UnixNano()%100000)
					newTarget := store.WorkspaceTarget{
						ID:       id,
						Name:     name,
						IsRemote: s.NewTargetRemote,
						WorkDir:  workDir,
						SSHHost:  sshHost,
					}
					draft.Targets = append(draft.Targets, newTarget)
					liveCfg.Targets = append(liveCfg.Targets, newTarget)
					s.NewTargetName = ""
					s.NewTargetWorkDir = ""
					s.NewTargetSSHHost = ""
					_ = liveCfg.Save()
					if onApply != nil {
						onApply()
					}
				}
			}
		})
	})
}
