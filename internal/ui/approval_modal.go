package ui

import (
	"fmt"
	"sync"

	"github.com/egoist/mygo/ui"

	"piui/internal/harness"
)

// ApprovalModal manages the UI state for Human-in-the-Loop high-risk tool call interception.
type ApprovalModal struct {
	mu            sync.Mutex
	Queue         []*harness.ApprovalRequest
	FeedbackInput string
}

// Enqueue adds a new suspended tool call approval request to the modal queue.
func (m *ApprovalModal) Enqueue(req *harness.ApprovalRequest) {
	if req == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Queue = append(m.Queue, req)
}

// Current returns the front-most pending approval request, or nil if none.
func (m *ApprovalModal) Current() *harness.ApprovalRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Queue) == 0 {
		return nil
	}
	return m.Queue[0]
}

// PendingCount returns the number of queued approval requests.
func (m *ApprovalModal) PendingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Queue)
}

// Pop removes the front-most request after it has been resolved.
func (m *ApprovalModal) Pop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Queue) > 0 {
		m.Queue = m.Queue[1:]
	}
	m.FeedbackInput = ""
}

// Clear empties any pending requests (e.g. when a run is aborted).
func (m *ApprovalModal) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Queue = nil
	m.FeedbackInput = ""
}

// Render draws the Antigravity Human-in-the-Loop approval dialog if there is a pending request.
func (m *ApprovalModal) Render(c *ui.Context, p Palette, gate *harness.SafetyGate) {
	req := m.Current()
	if req == nil || gate == nil {
		return
	}

	open := true
	AppDialog(c, p, &open, 540, 0, func() {
		levelLabel := "高危操作拦截"
		bannerBg := p.DangerSoft
		bannerFg := p.DangerColor
		switch req.Assessment.Level {
		case harness.RiskCritical:
			levelLabel = "极高危毁灭性拦截"
		case harness.RiskSensitive:
			levelLabel = "敏感操作确认"
			bannerBg = p.WarnSoft
			bannerFg = p.WarnColor
		}

		ui.Column(c).FillWidth().Children(func() {
			// Clean Edge-to-Edge Risk Header (zero nested double-boxes)
			ui.Row(c).FillWidth().Padding(14, 18).Gap(10).AlignItems(ui.Center).
				Background(bannerBg).BorderWidth(0, 0, 1, 0).BorderColor(bannerFg.Alpha(0.28)).
				Children(func() {
					ui.Row(c).Size(30, 30).Shrink(0).Radius(RadiusMD).Background(bannerFg.Alpha(0.16)).
						AlignItems(ui.Center).Justify(ui.Center).Children(func() {
						ui.Icon(c, IconShieldAlert).Size(16, 16).TextColor(bannerFg)
					})
					ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
						ui.Text(c, levelLabel+" · "+req.Assessment.Category).
							FontWeight(WeightSemibold).FontSize(FontBody).TextColor(p.TextPrimary).SingleLine().Ellipsis("...")
						ui.Text(c, req.Assessment.Reason).
							FontSize(FontCaption + 0.5).TextColor(p.TextSecondary).SingleLine().Ellipsis("...")
					})
					if len(m.Queue) > 1 {
						ui.Row(c).Padding(2, 7).Radius(RadiusPill).Background(p.ModalBg).Border(1, p.BorderSubtle).Children(func() {
							ui.Text(c, fmt.Sprintf("1/%d", len(m.Queue))).
								FontFeatures("tnum").FontSize(FontCaption).FontWeight(WeightMedium).TextColor(p.TextSecondary)
						})
					}
				})

			// Body Section
			ui.Column(c).FillWidth().Padding(16, 18).Gap(12).Children(func() {
				// Context Metadata Row
				ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
					ui.Row(c).Padding(3, 8).Radius(RadiusSM).Background(p.InputBg).Border(1, p.BorderSubtle).Gap(5).AlignItems(ui.Center).Children(func() {
						toolIcon, _ := toolVisualMeta(req.ToolName)
						ui.Icon(c, toolIcon).Size(11.5, 11.5).TextColor(p.TextMuted)
						ui.Text(c, req.ToolName).Font(FontMono).FontWeight(WeightMedium).FontSize(FontCaption).TextColor(p.TextPrimary)
					})

					if req.Target.IsRemote {
						ui.Row(c).Padding(3, 8).Radius(RadiusSM).Background(p.SSHSoft).Gap(5).AlignItems(ui.Center).Children(func() {
							ui.Icon(c, IconServer).Size(11.5, 11.5).TextColor(p.SSHColor)
							ui.Text(c, "SSH · "+req.Target.DisplaySub()).Font(FontMono).FontWeight(WeightMedium).FontSize(FontCaption).TextColor(p.SSHColor)
						})
					} else {
						ui.Row(c).Padding(3, 8).Radius(RadiusSM).Background(p.InputBg).Border(1, p.BorderSubtle).Gap(5).AlignItems(ui.Center).Children(func() {
							ui.Icon(c, IconFolder).Size(11.5, 11.5).TextColor(p.TextMuted)
							ui.Text(c, req.Target.DisplaySub()).Font(FontMono).FontSize(FontCaption).TextColor(p.TextSecondary)
						})
					}
				})

				// Command / File Diff Preview Box
				ui.Column(c).FillWidth().Gap(5).Children(func() {
					ui.Text(c, "待执行指令或变更预览").FontSize(FontSmall).FontWeight(WeightMedium).TextColor(p.TextSecondary)
					ui.Scroll(c).MaxHeight(160).FillWidth().Children(func() {
						ui.Column(c).FillWidth().Padding(10, 12).Radius(RadiusMD).
							Background(p.CodeBg).Border(1, p.BorderSubtle).Gap(4).Children(func() {
							ui.Text(c, req.Assessment.Preview).
								Font(FontMono).FontSize(FontSmall).LineHeight(1.48).TextColor(p.TermText).Selectable()
						})
					})
				})

				// Optional rejection / steering instruction
				ui.Column(c).FillWidth().Gap(5).Children(func() {
					ui.Text(c, "拒绝或纠偏指示（可选，拒绝时将作为 Steer 反馈注入上下文）").
						FontSize(FontSmall).TextColor(p.TextSecondary)
					ui.TextInput(c, &m.FeedbackInput).FillWidth().
						Placeholder("例如：不要直接 reload 生产网关，请先执行 nginx -t 校验语法...")
				})
			})

			// Edge-to-Edge Action Footer
			ui.Row(c).FillWidth().Height(52).Padding(0, 18).AlignItems(ui.Center).Gap(8).
				Background(p.BgSidebar).BorderWidth(1, 0, 0, 0).BorderColor(p.BorderSubtle).Children(func() {
				// Deny Button
				denyBtn := ui.ButtonBase(c).Height(30).Padding(0, 12).Radius(RadiusSM).
					Border(1, p.DangerColor.Alpha(0.45)).Gap(5).
					AlignItems(ui.Center).Tooltip("拒绝执行并将上方纠偏指示反馈给 Agent")
				denyBg := p.DangerSoft
				if denyBtn.Hovered() || denyBtn.Pressed() {
					denyBg = p.DangerColor.Alpha(0.22)
				}
				denyBtn.Background(denyBg).Children(func() {
					ui.Icon(c, IconCross).Size(13, 13).TextColor(p.DangerColor)
					ui.Text(c, "拒绝").FontWeight(WeightMedium).FontSize(FontSmall).TextColor(p.DangerColor)
				})
				if denyBtn.Clicked() {
					gate.Resolve(req.ID, harness.ApprovalDecision{
						Approved: false,
						Feedback: m.FeedbackInput,
					})
					m.Pop()
					c.Toast("已拒绝该高危操作并反馈给 Agent")
				}

				ui.Spacer(c)

				// Trust in Session Button
				GhostBtn(c, p, IconShieldCheck, "本会话免审", "允许本次执行，并在本会话内自动放行同类签名操作", func() {
					gate.Resolve(req.ID, harness.ApprovalDecision{
						Approved:       true,
						TrustSignature: true,
					})
					m.Pop()
					c.Toast("已放行并在当前会话内信任同类操作")
				})

				// Allow Button (100% Palette-driven)
				allowBtn := ui.ButtonBase(c).Height(30).Padding(0, 14).Radius(RadiusSM).Gap(5).
					AlignItems(ui.Center).Tooltip("批准本次操作执行")
				allowBg := p.SuccessBg
				if allowBtn.Hovered() || allowBtn.Pressed() {
					allowBg = allowBg.Alpha(0.88)
				}
				allowBtn.Background(allowBg).Children(func() {
					ui.Icon(c, IconCheck).Size(13.5, 13.5).TextColor(p.SuccessFg)
					ui.Text(c, "允许执行").FontWeight(WeightMedium).FontSize(FontSmall).TextColor(p.SuccessFg)
				})
				if allowBtn.Clicked() {
					gate.Resolve(req.ID, harness.ApprovalDecision{
						Approved: true,
					})
					m.Pop()
					c.Toast("已批准执行")
				}
			})
		})
	})

	// If user dismissed modal via Escape / backdrop click, treat as Deny for safety.
	if !open {
		gate.Resolve(req.ID, harness.ApprovalDecision{
			Approved: false,
			Feedback: m.FeedbackInput,
		})
		m.Pop()
	}
}
