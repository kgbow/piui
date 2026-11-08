package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/sky-valley/pi/coding"

	"piui/internal/harness"
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

	editing bool
	draft   store.AppConfig

	// Model fetching state (per profile ID)
	fetchingModels bool
	fetchStatus    string
	fetchError     string
	fetchedModels  map[string][]string
	modelSearch    string

	// New target draft fields
	NewTargetRemote  bool
	NewTargetName    string
	NewTargetWorkDir string
	NewTargetSSHHost string
}

// Render draws the Antigravity settings modal when Open is true.
func (s *SettingsModal) Render(c *ui.Context, p Palette, cfg *store.AppConfig, mgr *harness.Manager, updateUI func(func()), onApply func()) {
	if !s.Open {
		s.editing = false
		return
	}
	if !s.editing {
		s.draft = cfg.Clone()
		if s.fetchedModels == nil {
			s.fetchedModels = map[string][]string{}
		}
		s.fetchStatus = ""
		s.fetchError = ""
		s.editing = true
	}

	var skills []coding.Skill
	if mgr != nil {
		skills = mgr.DiscoverSkills()
	}

	ui.Modal(c, &s.Open, func() {
		ui.Column(c).Width(620).Gap(12).Children(func() {
			// Modal Header
			ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).
				Padding(0, 0, 8, 0).BorderWidth(0, 0, 1, 0).BorderColor(p.BorderSubtle).Children(func() {
				ui.Icon(c, IconSettings).Size(15, 15).TextColor(p.TextSecondary)
				ui.Text(c, "渠道模型、工作区与安全审批设置").Bold().FontSize(14.5).TextColor(p.TextPrimary)
				ui.Spacer(c)
				IconBtn(c, p, IconCross, 26, 14, func() {
					s.Open = false
					s.editing = false
				})
			})

			// 3-Segment Tab Bar
			tabTitles := []string{
				"渠道与模型配置",
				fmt.Sprintf("工作区与 SSH 目标 (%d)", len(s.draft.Targets)),
				fmt.Sprintf("安全拦截与 Skills (%d)", len(skills)),
			}
			renderSegmentedBar(c, p, &s.ActiveTab, tabTitles)

			switch s.ActiveTab {
			case 0:
				s.renderProviderTab(c, p, &s.draft, updateUI)
			case 1:
				s.renderTargetsTab(c, p, &s.draft, cfg, onApply)
			default:
				s.renderSafetyAndSkillsTab(c, p, &s.draft, mgr, skills)
			}

			// Modal Footer
			ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).
				Padding(10, 0, 0, 0).BorderWidth(1, 0, 0, 0).BorderColor(p.BorderSubtle).Children(func() {
				ui.Text(c, store.ShortenHomePath(store.ConfigPath())).
					Font("Menlo").FontSize(11).TextColor(p.TextMuted)
				ui.Spacer(c)
				renderModalGhostBtn(c, p, "取消", func() {
					s.Open = false
					s.editing = false
				})
				renderModalPrimaryBtn(c, p, "保存并立即生效", func() {
					s.commitToConfig(cfg, onApply)
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

func renderSegmentedBar(c *ui.Context, p Palette, activeIdx *int, labels []string) {
	ui.Row(c).FillWidth().Padding(3, 3).Radius(8).
		Background(p.BgSidebar).Border(1, p.BorderSubtle).Gap(3).Children(func() {
		for i, lbl := range labels {
			idx := i
			text := lbl
			active := *activeIdx == idx
			b := ui.ButtonBase(c).Grow(1).Padding(6, 8).Radius(6).
				AlignItems(ui.Center).Justify(ui.Center)
			bg := ui.Transparent
			fg := p.TextMuted
			borderCol := ui.Transparent
			if active {
				bg = p.BgElevated
				fg = p.TextPrimary
				borderCol = p.BorderSubtle
			} else if b.Hovered() {
				fg = p.TextSecondary
			}
			b.Background(bg).Border(1, borderCol).Children(func() {
				el := ui.Text(c, text).FontSize(12).TextColor(fg)
				if active {
					el.Bold()
				}
			})
			if b.Clicked() {
				*activeIdx = idx
			}
		}
	})
}

func renderModalGhostBtn(c *ui.Context, p Palette, label string, onClick func()) {
	b := ui.ButtonBase(c).Padding(6, 13).Radius(7).Border(1, p.BorderStrong).AlignItems(ui.Center)
	bg := ui.Transparent
	fg := p.TextSecondary
	if b.Hovered() || b.Pressed() {
		bg = p.BgHover
		fg = p.TextPrimary
	}
	b.Background(bg).Children(func() {
		ui.Text(c, label).FontSize(12).TextColor(fg)
	})
	if b.Clicked() && onClick != nil {
		onClick()
	}
}

func renderModalPrimaryBtn(c *ui.Context, p Palette, label string, onClick func()) {
	b := ui.ButtonBase(c).Padding(6, 14).Radius(7).AlignItems(ui.Center)
	bg := p.BtnPrimaryBg
	if b.Hovered() {
		bg = bg.Alpha(0.9)
	}
	b.Background(bg).Children(func() {
		ui.Text(c, label).Bold().FontSize(12).TextColor(p.BtnPrimaryFg)
	})
	if b.Clicked() && onClick != nil {
		onClick()
	}
}

func (s *SettingsModal) commitToConfig(cfg *store.AppConfig, onApply func()) {
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

	cfg.SafetyPolicy = s.draft.SafetyPolicy
	if cfg.SafetyPolicy == "" {
		cfg.SafetyPolicy = store.SafetyPolicyRemoteHighRisk
	}

	_ = cfg.ActiveProfile()
	_ = cfg.ActiveTarget()
	_ = cfg.Save()
	if onApply != nil {
		onApply()
	}
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
			s.fetchStatus = fmt.Sprintf("✓ 成功从 /v1/models 拉取 %d 个模型，勾选后即可在主界面快捷切换", len(models))

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
	// Channel pill strip (.chan-strip)
	ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
		for i := range draft.Profiles {
			profItem := &draft.Profiles[i]
			active := profItem.ID == draft.ActiveProfileID
			profID := profItem.ID
			profName := profItem.Name

			b := ui.ButtonBase(c).Padding(5, 11).Radius(7).Gap(6).AlignItems(ui.Center)
			bg := p.BgSurface
			fg := p.TextSecondary
			borderCol := p.BorderSubtle
			dotCol := p.TextMuted
			if active {
				bg = p.BgActive
				fg = p.TextPrimary
				borderCol = p.BorderStrong
				dotCol = p.LocalColor
			} else if b.Hovered() {
				bg = p.BgHover
				fg = p.TextPrimary
			}
			b.Background(bg).Border(1, borderCol).Children(func() {
				ui.Box(c).Size(6, 6).Radius(99).Background(dotCol)
				el := ui.Text(c, profName).FontSize(12).TextColor(fg)
				if active {
					el.Bold()
				}
			})
			if b.Clicked() {
				draft.ActiveProfileID = profID
				s.fetchStatus = ""
				s.fetchError = ""
			}
		}

		ui.Spacer(c)

		addBtn := ui.ButtonBase(c).Padding(5, 10).Radius(7).Border(1, p.BorderStrong).
			Gap(5).AlignItems(ui.Center)
		addBg := ui.Transparent
		if addBtn.Hovered() {
			addBg = p.BgHover
		}
		addBtn.Background(addBg).Children(func() {
			ui.Icon(c, IconPlus).Size(12, 12).TextColor(p.TextSecondary)
			ui.Text(c, "新增渠道").FontSize(11.5).TextColor(p.TextSecondary)
		})
		if addBtn.Clicked() {
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
		}

		if len(draft.Profiles) > 1 {
			delBtn := ui.ButtonBase(c).Padding(5, 8).Radius(7).AlignItems(ui.Center)
			fg := p.TextMuted
			if delBtn.Hovered() {
				fg = p.DangerColor
			}
			delBtn.Children(func() {
				ui.Text(c, "删除").FontSize(11.5).TextColor(fg)
			})
			if delBtn.Clicked() {
				for i := range draft.Profiles {
					if draft.Profiles[i].ID == draft.ActiveProfileID {
						draft.Profiles = append(draft.Profiles[:i], draft.Profiles[i+1:]...)
						break
					}
				}
				_ = draft.ActiveProfile()
				s.fetchStatus = ""
				s.fetchError = ""
			}
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

	ui.Column(c).FillWidth().Gap(9).Children(func() {
		// 2-column Name & BaseURL
		ui.Row(c).FillWidth().Gap(10).Children(func() {
			ui.Column(c).Grow(1).Gap(4).Children(func() {
				ui.Text(c, "渠道名称 (Channel Name)").FontSize(11.5).TextColor(p.TextSecondary)
				ui.TextInput(c, &prof.Name).FillWidth().Placeholder("例如：OpenRouter / DeepSeek / 聚合网关")
			})
			ui.Column(c).Grow(1).Gap(4).Children(func() {
				basePlaceholder := "https://api.openai.com/v1"
				if prof.Format == store.FormatAnthropicMessages {
					basePlaceholder = "https://api.anthropic.com"
				}
				ui.Text(c, "Base URL (接口地址)").FontSize(11.5).TextColor(p.TextSecondary)
				ui.TextInput(c, &prof.BaseURL).FillWidth().Font("Menlo").Placeholder(basePlaceholder)
			})
		})

		// Wire format 3-way segmented bar
		ui.Column(c).FillWidth().Gap(4).Children(func() {
			ui.Text(c, "接口协议格式 (API Wire Format)").FontSize(11.5).TextColor(p.TextSecondary)
			prevFmt := fmtIdx
			renderSegmentedBar(c, p, &fmtIdx, formatLabels)
			if fmtIdx != prevFmt {
				prof.Format = formatValues[fmtIdx]
			}
		})

		// API Key input
		ui.Column(c).FillWidth().Gap(4).Children(func() {
			ui.Text(c, "API Key (留空读取环境变量 ANTHROPIC_API_KEY / OPENAI_API_KEY)").FontSize(11.5).TextColor(p.TextSecondary)
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				in := ui.TextInput(c, &prof.APIKey).Grow(1).Font("Menlo").Placeholder("sk-...")
				if !s.ShowAPIKey {
					in.Password()
				}
				toggleLabel := "显示"
				if s.ShowAPIKey {
					toggleLabel = "隐藏"
				}
				renderModalGhostBtn(c, p, toggleLabel, func() {
					s.ShowAPIKey = !s.ShowAPIKey
				})
			})
		})

		// Models Management Box (.models-box)
		ui.Column(c).FillWidth().Padding(10, 12).Radius(9).
			Background(p.BgSurface).Border(1, p.BorderSubtle).Gap(8).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(6).Children(func() {
				ui.Text(c, fmt.Sprintf("该渠道启用的模型 (%d/%d 已勾选)", len(prof.AvailableModels()), len(candidates))).
					Bold().FontSize(11.5).TextColor(p.TextSecondary)
				ui.Spacer(c)

				if len(candidates) > 0 {
					MetaActionBtn(c, p, "全选", func() {
						q := strings.ToLower(strings.TrimSpace(s.modelSearch))
						for _, cand := range candidates {
							if q == "" || strings.Contains(strings.ToLower(cand), q) {
								prof.AddModel(cand)
							}
						}
					})
					ui.Text(c, "·").FontSize(11).TextColor(p.TextMuted)
					MetaActionBtn(c, p, "仅留当前", func() {
						cur := strings.TrimSpace(prof.ModelID)
						if cur != "" {
							prof.Models = []string{cur}
						} else {
							prof.Models = nil
						}
					})
				}

				fb := ui.ButtonBase(c).Padding(4, 9).Radius(6).Border(1, p.BorderStrong).
					Gap(5).AlignItems(ui.Center).Disabled(s.fetchingModels)
				fBg := p.BgElevated
				if fb.Hovered() && !s.fetchingModels {
					fBg = p.BgHover
				}
				lbl := "手动拉取模型"
				if s.fetchingModels {
					lbl = "拉取中..."
				}
				fb.Background(fBg).Children(func() {
					ui.Icon(c, IconRefresh).Size(12, 12).TextColor(p.Accent)
					ui.Text(c, lbl).Bold().FontSize(11).TextColor(p.TextPrimary)
				})
				if fb.Clicked() {
					s.triggerFetchModels(*prof, updateUI)
				}
			})

			// Active Model ID input + Search Filter
			ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Children(func() {
				ui.TextInput(c, &prof.ModelID).Grow(1).Font("Menlo").Placeholder("当前模型 ID (如 gpt-4.1 / deepseek-v3)")
				if trimmed := strings.TrimSpace(prof.ModelID); trimmed != "" && !prof.HasModel(trimmed) {
					renderModalGhostBtn(c, p, "+ 加入列表", func() {
						prof.AddModel(trimmed)
					})
				}
				if len(candidates) > 3 {
					ui.TextInput(c, &s.modelSearch).Width(155).Placeholder("搜索过滤模型...")
				}
			})

			if s.fetchError != "" {
				ui.Text(c, s.fetchError).Font("Menlo").FontSize(11).TextColor(p.DangerColor)
			} else if s.fetchStatus != "" {
				ui.Text(c, s.fetchStatus).Font("Menlo").FontSize(11).TextColor(p.LocalColor)
			}

			// Model Checklist (.model-row)
			if len(candidates) > 0 {
				q := strings.ToLower(strings.TrimSpace(s.modelSearch))
				ui.Scroll(c).MaxHeight(125).FillWidth().Children(func() {
					ui.Column(c).FillWidth().Gap(3).Children(func() {
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
							ui.Row(c).FillWidth().Padding(5, 8).Radius(6).Background(rowBg).
								AlignItems(ui.Center).Gap(8).Children(func() {
								// Custom Emerald Checkbox + Model ID
								cb := ui.ButtonBase(c).Gap(8).AlignItems(ui.Center)
								boxBg := ui.Transparent
								boxBorder := p.BorderStrong
								if checked {
									boxBg = p.LocalColor
									boxBorder = p.LocalColor
								}
								cb.Children(func() {
									ui.Row(c).Size(14, 14).Radius(4).Background(boxBg).Border(1, boxBorder).
										AlignItems(ui.Center).Justify(ui.Center).Children(func() {
										if checked {
											ui.Icon(c, IconCheck).Size(10, 10).TextColor(ui.Hex("#09090B"))
										}
									})
									el := ui.Text(c, modelName).Font("Menlo").FontSize(12).TextColor(p.TextPrimary)
									if isCurrent {
										el.Bold()
									}
								})
								if cb.Clicked() {
									if checked {
										prof.RemoveModel(modelName)
									} else {
										prof.AddModel(modelName)
									}
								}

								ui.Spacer(c)

								if isCurrent {
									ui.Row(c).Padding(1, 7).Radius(4).Background(p.AccentSoft).Children(func() {
										ui.Text(c, "当前使用").Bold().FontSize(10.5).TextColor(p.Accent)
									})
								} else {
									sb := ui.ButtonBase(c).Padding(2, 7).Radius(4).Border(1, p.BorderStrong)
									sBg := ui.Transparent
									sFg := p.TextSecondary
									if sb.Hovered() {
										sBg = p.BgHover
										sFg = p.TextPrimary
									}
									sb.Background(sBg).Children(func() {
										ui.Text(c, "设为当前").FontSize(10.5).TextColor(sFg)
									})
									if sb.Clicked() {
										prof.ModelID = modelName
										prof.AddModel(modelName)
									}
								}
							})
						}
						if shownCount == 0 {
							ui.Text(c, "没有匹配的模型").FontSize(11).TextColor(p.TextMuted)
						}
					})
				})
			}
		})

		// Thinking Level 4-way segmented bar
		ui.Column(c).FillWidth().Gap(4).Children(func() {
			ui.Text(c, "推理思考深度 (Thinking Level)").FontSize(11.5).TextColor(p.TextSecondary)
			prevThink := thinkIdx
			renderSegmentedBar(c, p, &thinkIdx, thinkingLabels)
			if thinkIdx != prevThink {
				prof.ThinkingLevel = thinkingValues[thinkIdx]
			}
		})
	})
}

func (s *SettingsModal) renderTargetsTab(c *ui.Context, p Palette, draft *store.AppConfig, liveCfg *store.AppConfig, onApply func()) {
	ui.Column(c).FillWidth().Gap(10).Children(func() {
		ui.Text(c, "已配置的工作区目标 (支持在同一 Session 对话中随时一键热切换):").
			Bold().FontSize(12).TextColor(p.TextSecondary)

		ui.Scroll(c).MaxHeight(175).FillWidth().Children(func() {
			ui.Column(c).FillWidth().Gap(6).Children(func() {
				for i := range draft.Targets {
					wt := &draft.Targets[i]
					icon := IconFolder
					iconBg := p.LocalSoft
					iconFg := p.LocalColor
					tagText := "Local"
					if wt.IsRemote {
						icon = IconServer
						iconBg = p.SSHSoft
						iconFg = p.SSHColor
						tagText = "SSH"
					}
					ui.Row(c).FillWidth().Padding(7, 10).Radius(8).
						Background(p.BgSurface).Border(1, p.BorderSubtle).
						AlignItems(ui.Center).Gap(9).Children(func() {
						ui.Row(c).Size(24, 24).Radius(6).Background(iconBg).
							AlignItems(ui.Center).Justify(ui.Center).Children(func() {
							ui.Icon(c, icon).Size(13, 13).TextColor(iconFg)
						})
						ui.Column(c).Grow(1).Gap(1).Children(func() {
							ui.Text(c, wt.ShortName()).Bold().FontSize(12.5).TextColor(p.TextPrimary)
							ui.Text(c, wt.DisplaySub()).Font("Menlo").FontSize(11).TextColor(p.TextMuted).SingleLine().Ellipsis("...")
						})
						ui.Row(c).Padding(1, 7).Radius(99).Background(iconBg).Children(func() {
							ui.Text(c, tagText).Bold().Font("Menlo").FontSize(10).TextColor(iconFg)
						})
						if len(draft.Targets) > 1 && i > 0 {
							idx := i
							MetaActionBtn(c, p, "删除", func() {
								removedID := draft.Targets[idx].ID
								draft.Targets = append(draft.Targets[:idx], draft.Targets[idx+1:]...)
								if draft.ActiveTargetID == removedID {
									_ = draft.ActiveTarget()
								}
							})
						}
					})
				}
			})
		})

		ui.Column(c).FillWidth().Padding(10, 12).Radius(9).
			Background(p.BgSurface).Border(1, p.BorderSubtle).Gap(8).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
				ui.Text(c, "添加新工作区目标").Bold().FontSize(12).TextColor(p.TextPrimary)
				ui.Spacer(c)
				ui.Checkbox(c, &s.NewTargetRemote, "远程 SSH 服务器 (复用 ~/.ssh/config 与 ControlMaster)")
			})

			ui.Row(c).FillWidth().Gap(8).Children(func() {
				ui.TextInput(c, &s.NewTargetName).Grow(1).Placeholder("显示名称 (如: prod-gateway / frontend)")
				if s.NewTargetRemote {
					ui.TextInput(c, &s.NewTargetSSHHost).Grow(1).Font("Menlo").Placeholder("SSH Host 别名或 user@ip")
				}
			})

			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				dirHint := "本地绝对路径 (如 ~/Documents/code/myproj)"
				if s.NewTargetRemote {
					dirHint = "远端工作目录 (如 /srv/gateway 或 ~)"
				}
				ui.TextInput(c, &s.NewTargetWorkDir).Grow(1).Font("Menlo").Placeholder(dirHint)
				renderModalPrimaryBtn(c, p, "+ 添加目标", func() {
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
								name = sshHost
							} else {
								name = filepath.Base(workDir)
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
				})
			})
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

	ui.Column(c).FillWidth().Gap(10).Children(func() {
		ui.Column(c).FillWidth().Gap(4).Children(func() {
			ui.Text(c, "高危操作弹窗拦截策略 (Human-in-the-Loop Safety Gate)").FontSize(11.5).TextColor(p.TextSecondary)
			prevPol := policyIdx
			renderSegmentedBar(c, p, &policyIdx, safetyLabels)
			if policyIdx != prevPol {
				draft.SafetyPolicy = safetyValues[policyIdx]
			}
		})

		policyDesc := "【远程高危拦截 (推荐)】：在远程 SSH 服务器上拦截 rm -rf、systemctl 关停、kill -9、防火墙/数据库破坏操作以及 /etc、~/.ssh、.env 等敏感文件修改；在本地环境仅拦截 rm -rf /、mkfs、shutdown 等毁灭性命令。"
		switch draft.SafetyPolicy {
		case store.SafetyPolicyAlwaysHighRisk:
			policyDesc = "【全局高危拦截】：无论在本地目录还是远程 SSH 服务器，遇到高危 bash 命令与敏感配置文件修改均弹窗要求人工确认。"
		case store.SafetyPolicyStrictRemote:
			policyDesc = "【远程严格模式】：在远程 SSH 服务器上，除 ls/cat/grep/git status 等纯只读查询命令外，所有文件写入/修改 (write/edit) 与变更命令均需弹窗审批。"
		case store.SafetyPolicyOff:
			policyDesc = "【关闭拦截】：所有工具调用均自动放行，不弹出安全确认框（请谨慎在生产服务器开启）。"
		}
		ui.Column(c).FillWidth().Padding(8, 12).Radius(8).
			Background(p.BgSurface).Border(1, p.BorderSubtle).Children(func() {
			ui.Text(c, policyDesc).FontSize(11.5).TextColor(p.TextSecondary)
		})

		if mgr != nil && mgr.SafetyGate() != nil {
			trustedCount := mgr.SafetyGate().TrustedCount()
			ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
				ui.Text(c, fmt.Sprintf("当前会话已记住免审的操作签名数: %d", trustedCount)).
					FontSize(12).TextColor(p.TextSecondary)
				ui.Spacer(c)
				if trustedCount > 0 {
					renderModalGhostBtn(c, p, "清空本会话免审白名单", func() {
						mgr.SafetyGate().ClearTrusted()
						c.Toast("已清空本会话免审白名单")
					})
				}
			})
		}

		// Active Skills discovery display (Local + Remote SSH)
		ui.Row(c).FillWidth().AlignItems(ui.Center).Padding(4, 0, 0, 0).Children(func() {
			ui.Text(c, fmt.Sprintf("已自动发现的 Skills (%d)", len(skills))).Bold().FontSize(12.5).TextColor(p.TextPrimary)
			ui.Spacer(c)
			ui.Text(c, "扫描路径: <工作区>/.pi/skills & ~/.pi/agent/skills").Font("Menlo").FontSize(10.5).TextColor(p.TextMuted)
		})

		ui.Scroll(c).MaxHeight(150).FillWidth().Children(func() {
			ui.Column(c).FillWidth().Gap(6).Children(func() {
				if len(skills) == 0 {
					ui.Column(c).FillWidth().Padding(10, 12).Radius(8).
						Background(p.BgSurface).Border(1, p.BorderSubtle).Children(func() {
						ui.Text(c, "当前工作区未发现自定义 SKILL.md，可在项目 .pi/skills/<name>/SKILL.md 中添加专属技能。").
							FontSize(11.5).TextColor(p.TextMuted)
					})
				}
				for _, sk := range skills {
					isRemoteSkill := strings.HasPrefix(sk.FilePath, "ssh://")
					badgeBg := p.LocalSoft
					badgeFg := p.LocalColor
					badgeText := "SKILL"
					if isRemoteSkill {
						badgeBg = p.SSHSoft
						badgeFg = p.SSHColor
						badgeText = "SSH SKILL"
					}
					ui.Column(c).FillWidth().Padding(7, 10).Radius(8).
						Background(p.BgSurface).Border(1, p.BorderSubtle).Gap(3).Children(func() {
						ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(6).Children(func() {
							ui.Row(c).Padding(1, 6).Radius(4).Background(badgeBg).Children(func() {
								ui.Text(c, badgeText).Bold().Font("Menlo").FontSize(10).TextColor(badgeFg)
							})
							ui.Text(c, sk.Name).Bold().FontSize(12).TextColor(p.TextPrimary)
							ui.Spacer(c)
							ui.Text(c, filepath.Base(filepath.Dir(sk.FilePath))+"/"+filepath.Base(sk.FilePath)).
								Font("Menlo").FontSize(10.5).TextColor(p.TextMuted)
						})
						if sk.Description != "" {
							ui.Text(c, sk.Description).FontSize(11).TextColor(p.TextSecondary).SingleLine().Ellipsis("...")
						}
					})
				}
			})
		})
	})
}
