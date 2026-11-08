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
	ui.Modal(c, &open, func() {
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

		targetChipText := "Local · " + req.Target.DisplaySub()
		targetChipBg := p.LocalSoft
		targetChipFg := p.LocalColor
		if req.Target.IsRemote {
			targetChipText = "SSH · " + req.Target.DisplaySub()
			targetChipBg = p.SSHSoft
			targetChipFg = p.SSHColor
		}

		ui.Column(c).Width(540).Gap(11).Children(func() {
			// Danger Banner (.danger-banner)
			ui.Row(c).FillWidth().Padding(10, 12).Radius(9).
				Background(bannerBg).Border(1, bannerFg).
				AlignItems(ui.Center).Gap(10).Children(func() {
				ui.Row(c).Padding(2, 8).Radius(5).Background(bannerBg).Gap(5).AlignItems(ui.Center).Children(func() {
					ui.Icon(c, IconShieldAlert).Size(13, 13).TextColor(bannerFg)
					ui.Text(c, levelLabel).Bold().FontSize(11).TextColor(bannerFg)
				})
				ui.Text(c, req.Assessment.Category).Bold().FontSize(13.5).TextColor(p.TextPrimary)
				ui.Spacer(c)
				ui.Text(c, fmt.Sprintf("待审队列: %d", len(m.Queue))).
					Font("Menlo").FontSize(11).TextColor(p.TextMuted)
			})

			// Metadata Chips (.meta-row)
			ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
				ui.Row(c).Padding(3, 8).Radius(5).Background(p.BgElevated).Border(1, p.BorderSubtle).Children(func() {
					ui.Text(c, "工具: "+req.ToolName).Font("Menlo").FontSize(11).TextColor(p.TextSecondary)
				})
				ui.Row(c).Padding(3, 8).Radius(5).Background(targetChipBg).Children(func() {
					ui.Text(c, targetChipText).Bold().Font("Menlo").FontSize(11).TextColor(targetChipFg)
				})
				ui.Row(c).Padding(3, 8).Radius(5).Background(p.BgElevated).Border(1, p.BorderSubtle).Children(func() {
					ui.Text(c, "策略: Safety Gate").Font("Menlo").FontSize(11).TextColor(p.TextSecondary)
				})
			})

			// Interception Reason
			ui.Row(c).FillWidth().Gap(4).Wrap().Children(func() {
				ui.Text(c, "拦截原因：").Bold().FontSize(12.5).TextColor(p.TextPrimary)
				ui.Text(c, req.Assessment.Reason).FontSize(12.5).TextColor(p.TextSecondary)
			})

			// Dark Command / File Diff Preview Box (.cmd-preview-box)
			ui.Column(c).FillWidth().Gap(4).Children(func() {
				ui.Text(c, "待执行内容详情:").FontSize(11.5).TextColor(p.TextSecondary)
				ui.Scroll(c).MaxHeight(160).FillWidth().Children(func() {
					ui.Column(c).FillWidth().Padding(10, 12).Radius(8).
						Background(ui.Hex("#0F172A")).Border(1, ui.RGBA(255, 255, 255, 0.10)).Gap(4).Children(func() {
						ui.Text(c, "# Target: "+req.Target.DisplaySub()).
							Font("Menlo").FontSize(11).TextColor(ui.Hex("#94A3B8"))
						ui.Text(c, req.Assessment.Preview).
							Font("Menlo").FontSize(11.5).TextColor(ui.Hex("#F8FAFC")).Selectable()
					})
				})
			})

			// Optional rejection / steering instruction
			ui.Column(c).FillWidth().Gap(4).Children(func() {
				ui.Text(c, "拒绝或纠偏指示 (可选，拒绝时将直接作为 Steer 反馈注入 Agent 上下文)").
					FontSize(11.5).TextColor(p.TextSecondary)
				ui.TextInput(c, &m.FeedbackInput).FillWidth().
					Placeholder("例如：不要直接 reload 生产网关，请先执行 nginx -t 校验语法...")
			})

			// Action Footer (.modal-footer)
			ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).
				Padding(10, 0, 0, 0).BorderWidth(1, 0, 0, 0).BorderColor(p.BorderSubtle).Children(func() {
				// Deny Button
				denyBtn := ui.ButtonBase(c).Padding(6, 13).Radius(7).Border(1, p.DangerColor).AlignItems(ui.Center)
				denyBg := p.DangerSoft
				if denyBtn.Hovered() {
					denyBg = p.DangerColor.Alpha(0.22)
				}
				denyBtn.Background(denyBg).Children(func() {
					ui.Text(c, "拒绝并反馈给 Agent (Deny)").Bold().FontSize(12).TextColor(p.DangerColor)
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
				renderModalGhostBtn(c, p, "本会话信任同类操作", func() {
					gate.Resolve(req.ID, harness.ApprovalDecision{
						Approved:       true,
						TrustSignature: true,
					})
					m.Pop()
					c.Toast("已放行并在当前会话内信任同类操作")
				})

				// Allow Button
				allowBtn := ui.ButtonBase(c).Padding(6, 14).Radius(7).AlignItems(ui.Center)
				allowBg := ui.Hex("#10B981")
				if allowBtn.Hovered() {
					allowBg = ui.Hex("#34D399")
				}
				allowBtn.Background(allowBg).Children(func() {
					ui.Text(c, "允许本次执行 (Allow)").Bold().FontSize(12).TextColor(ui.Hex("#052E16"))
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
