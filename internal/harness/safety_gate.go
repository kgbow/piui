package harness

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sky-valley/pi/agent"

	"piui/internal/store"
)

// RiskLevel classifies the severity of an intercepted tool call.
type RiskLevel string

const (
	RiskCritical  RiskLevel = "critical"  // Destructive anywhere (e.g. rm -rf /, mkfs, shutdown)
	RiskHigh      RiskLevel = "high"      // High risk on remote or when always-high-risk is enabled
	RiskSensitive RiskLevel = "sensitive" // Sensitive path edit or strict-remote mutation
)

// RiskAssessment describes why a tool call requires Human-in-the-Loop approval.
type RiskAssessment struct {
	Level         RiskLevel
	Category      string
	Reason        string
	TargetSummary string
	Preview       string
	Signature     string
}

// ApprovalDecision is the user's response to an ApprovalRequest.
type ApprovalDecision struct {
	Approved       bool
	TrustSignature bool   // Remember and auto-approve this Signature for the rest of the session
	Feedback       string // Optional user instruction returned to the LLM when rejected
}

// ApprovalRequest represents an in-flight tool call suspended awaiting user confirmation.
type ApprovalRequest struct {
	ID         string
	ToolCallID string
	ToolName   string
	Target     store.WorkspaceTarget
	Assessment RiskAssessment
	CreatedAt  time.Time
	responseCh chan ApprovalDecision
}

// ApprovalPromptHandler is invoked when a tool call is suspended for user approval.
type ApprovalPromptHandler func(req *ApprovalRequest)

type bashRiskRule struct {
	level    RiskLevel
	category string
	reason   string
	re       *regexp.Regexp
}

var (
	criticalBashRules = []bashRiskRule{
		{
			level:    RiskCritical,
			category: "根目录/主目录递归销毁",
			reason:   "检测到针对根目录 (/) 或用户主目录 (~ / $HOME) 的强制删除指令",
			re:       regexp.MustCompile(`(?:^|[;&|]\s*)(?:sudo\s+)?rm\s+(-[a-zA-Z]*[rf][a-zA-Z]*\s+)+(/|/\*|~|~/\*|\$HOME|\$HOME/\*)(\s|$|[;&|])`),
		},
		{
			level:    RiskCritical,
			category: "磁盘格式化与底层块写入",
			reason:   "检测到 mkfs / dd / fdisk / parted 等底层磁盘破坏性指令",
			re:       regexp.MustCompile(`(?:^|[;&|]\s*)(?:sudo\s+)?(mkfs(?:\.\w+)?|fdisk|parted)\b|(?:^|[;&|]\s*)(?:sudo\s+)?dd\s+if=`),
		},
		{
			level:    RiskCritical,
			category: "主机关机或重启",
			reason:   "检测到 shutdown / reboot / poweroff / halt / init 0 系统关停指令",
			re:       regexp.MustCompile(`(?:^|[;&|]\s*)(?:sudo\s+)?(shutdown|reboot|poweroff|halt)\b|(?:^|[;&|]\s*)(?:sudo\s+)?init\s+[06]\b`),
		},
	}

	highRiskBashRules = []bashRiskRule{
		{
			level:    RiskHigh,
			category: "递归或强制删除文件 (rm)",
			reason:   "命令包含 rm -r / rm -f 强制或递归删除操作，可能导致数据不可逆丢失",
			re:       regexp.MustCompile(`(?:^|[;&|]\s*)(?:sudo\s+)?rm\s+(-[a-zA-Z]*[rf][a-zA-Z]*|--recursive|--force)\b`),
		},
		{
			level:    RiskHigh,
			category: "系统服务状态变更",
			reason:   "命令将停止、重启或禁用 systemd / init 系统服务",
			re:       regexp.MustCompile(`(?:^|[;&|]\s*)(?:sudo\s+)?(systemctl\s+(stop|restart|reload|disable|mask|kill)\b|service\s+\S+\s+(stop|restart)\b)`),
		},
		{
			level:    RiskHigh,
			category: "强制终止进程 (kill)",
			reason:   "命令包含 kill -9 / killall / pkill 强制杀进程操作",
			re:       regexp.MustCompile(`(?:^|[;&|]\s*)(?:sudo\s+)?(kill\s+(-9|-KILL|-SIGKILL)\b|killall\b|pkill\b)`),
		},
		{
			level:    RiskHigh,
			category: "批量权限或属主修改",
			reason:   "命令包含 chmod -R / chmod 777 / chown -R 权限变更",
			re:       regexp.MustCompile(`(?:^|[;&|]\s*)(?:sudo\s+)?(chmod\s+(-R\b|777\b)|chown\s+-R\b)`),
		},
		{
			level:    RiskHigh,
			category: "容器或集群资源销毁",
			reason:   "命令包含 docker rm/stop/prune 或 kubectl delete 容器/集群销毁动作",
			re:       regexp.MustCompile(`(?:^|[;&|]\s*)(?:sudo\s+)?(docker\s+(rm|rmi|stop|kill|system\s+prune)\b|docker-compose\s+down\b|kubectl\s+delete\b)`),
		},
		{
			level:    RiskHigh,
			category: "防火墙与网络路由变更",
			reason:   "命令将修改 iptables / ufw 防火墙或网络路由表，可能导致 SSH 断联",
			re:       regexp.MustCompile(`(?:^|[;&|]\s*)(?:sudo\s+)?(iptables\b|ufw\s+(disable|reset|delete|deny)\b|ip\s+route\s+(del|flush)\b|ifconfig\s+\S+\s+down\b)`),
		},
		{
			level:    RiskHigh,
			category: "数据库破坏性变更 (SQL)",
			reason:   "检测到 DROP TABLE / DROP DATABASE / TRUNCATE TABLE 等高危数据库操作",
			re:       regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|DATABASE|SCHEMA)|TRUNCATE\s+TABLE)\b`),
		},
		{
			level:    RiskHigh,
			category: "Git 强制覆盖或工作区清理",
			reason:   "命令包含 git push --force / git reset --hard / git clean -f，可能丢失未提交代码",
			re:       regexp.MustCompile(`(?:^|[;&|]\s*)git\s+(push\b[^\n;&|]*(--force|-f\b)|reset\s+--hard\b|clean\s+-[a-zA-Z]*f)`),
		},
		{
			level:    RiskHigh,
			category: "远程脚本管道直接执行",
			reason:   "检测到通过 curl / wget 下载并直接管道喂给 sh/bash 执行的操作",
			re:       regexp.MustCompile(`(curl|wget)\b[^\n|]*\|\s*(sudo\s+)?(ba)?sh\b`),
		},
		{
			level:    RiskHigh,
			category: "系统软件包卸载",
			reason:   "检测到 apt / yum / dnf / apk 软件包卸载指令",
			re:       regexp.MustCompile(`(?:^|[;&|]\s*)(?:sudo\s+)?(apt|apt-get|yum|dnf|apk)\s+(remove|purge|autoremove)\b`),
		},
		{
			level:    RiskSensitive,
			category: "sudo 提权执行",
			reason:   "命令使用 sudo 提升至 root 权限执行",
			re:       regexp.MustCompile(`(?:^|[;&|]\s*)sudo\s+\S+`),
		},
	}

	readOnlyCmdPrefixes = []string{
		"ls", "pwd", "cat", "head", "tail", "grep", "rg", "find", "fd",
		"ps", "df", "du", "free", "uptime", "whoami", "id", "uname", "hostname",
		"stat", "file", "wc", "which", "type", "env", "printenv", "date", "echo",
		"git status", "git log", "git diff", "git show", "git branch", "git remote",
	}
)

// AssessToolCallRisk evaluates whether a tool invocation should be intercepted for user confirmation.
func AssessToolCallRisk(target store.WorkspaceTarget, policy store.SafetyPolicy, toolName string, args map[string]any) *RiskAssessment {
	if policy == "" {
		policy = store.SafetyPolicyRemoteHighRisk
	}
	if policy == store.SafetyPolicyOff {
		return nil
	}

	checkHighRisk := target.IsRemote || policy == store.SafetyPolicyAlwaysHighRisk
	strictRemote := target.IsRemote && policy == store.SafetyPolicyStrictRemote

	switch toolName {
	case "bash":
		cmd, _ := args["command"].(string)
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			return nil
		}

		// 1. Critical rules always trigger regardless of Local vs Remote.
		for _, rule := range criticalBashRules {
			if rule.re.MatchString(cmd) {
				return &RiskAssessment{
					Level:         rule.level,
					Category:      rule.category,
					Reason:        rule.reason,
					TargetSummary: truncateOneLine(cmd, 90),
					Preview:       cmd,
					Signature:     "bash:" + cmd,
				}
			}
		}

		// 2. High-risk rules trigger on Remote SSH or when AlwaysHighRisk is active.
		if checkHighRisk {
			for _, rule := range highRiskBashRules {
				if rule.re.MatchString(cmd) {
					return &RiskAssessment{
						Level:         rule.level,
						Category:      rule.category,
						Reason:        rule.reason,
						TargetSummary: truncateOneLine(cmd, 90),
						Preview:       cmd,
						Signature:     "bash:" + cmd,
					}
				}
			}
		}

		// 3. Strict remote mode intercepts any non-readonly bash command on SSH hosts.
		if strictRemote && !isReadOnlyBashCommand(cmd) {
			return &RiskAssessment{
				Level:         RiskSensitive,
				Category:      "远程严格模式：命令执行确认",
				Reason:        "当前处于「远程严格审批」模式，非只读查询类的远端命令均需人工确认",
				TargetSummary: truncateOneLine(cmd, 90),
				Preview:       cmd,
				Signature:     "bash:" + cmd,
			}
		}

	case "write", "edit":
		rawPath, _ := args["path"].(string)
		rawPath = strings.TrimSpace(rawPath)
		if rawPath == "" {
			return nil
		}

		resolved := rawPath
		if target.IsRemote {
			resolved = resolveRemotePath(target.WorkDir, rawPath)
		} else {
			resolved = store.ExpandLocalPath(rawPath)
			if !filepath.IsAbs(resolved) && target.WorkDir != "" {
				resolved = filepath.Join(store.ExpandLocalPath(target.WorkDir), resolved)
			}
			resolved = filepath.ToSlash(resolved)
		}

		preview := buildFileMutationPreview(toolName, resolved, args)

		if cat, reason, isCrit := matchSensitiveFilePath(resolved); cat != "" {
			if isCrit || checkHighRisk {
				lvl := RiskHigh
				if isCrit {
					lvl = RiskCritical
				}
				return &RiskAssessment{
					Level:         lvl,
					Category:      cat,
					Reason:        reason,
					TargetSummary: resolved,
					Preview:       preview,
					Signature:     toolName + ":" + resolved,
				}
			}
		}

		if strictRemote {
			return &RiskAssessment{
				Level:         RiskSensitive,
				Category:      fmt.Sprintf("远程严格模式：文件%s (%s)", map[string]string{"write": "覆写", "edit": "修改"}[toolName], path.Base(resolved)),
				Reason:        "当前处于「远程严格审批」模式，对远程服务器文件的任何写入或修改均需确认",
				TargetSummary: resolved,
				Preview:       preview,
				Signature:     toolName + ":" + resolved,
			}
		}
	}

	return nil
}

func matchSensitiveFilePath(p string) (category, reason string, critical bool) {
	norm := strings.ToLower(strings.TrimSpace(p))
	base := path.Base(norm)

	// Critical system & SSH credential paths
	if strings.HasPrefix(norm, "/etc/") || strings.HasPrefix(norm, "/boot/") || strings.HasPrefix(norm, "/usr/") || strings.HasPrefix(norm, "/sbin/") || strings.HasPrefix(norm, "/bin/") {
		return "系统核心目录文件修改", fmt.Sprintf("目标路径 %q 位于操作系统核心配置/二进制目录", p), true
	}
	if strings.Contains(norm, "/.ssh/") || strings.HasPrefix(norm, "~/.ssh/") || base == "authorized_keys" || base == "sshd_config" || base == "id_rsa" || base == "id_ed25519" {
		return "SSH 密钥与认证配置修改", fmt.Sprintf("目标路径 %q 涉及 SSH 登录鉴权与密钥安全", p), true
	}
	if strings.HasPrefix(norm, "/var/spool/cron") || base == "crontab" {
		return "系统定时任务修改", fmt.Sprintf("目标路径 %q 涉及系统级定时任务配置", p), false
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return "敏感环境变量文件 (.env) 修改", fmt.Sprintf("目标路径 %q 包含环境密钥或敏感配置", p), false
	}
	return "", "", false
}

func isReadOnlyBashCommand(cmd string) bool {
	// Reject redirections, subshells, or compound operators in strict read-only check.
	if strings.ContainsAny(cmd, "><`$") || strings.Contains(cmd, "&&") || strings.Contains(cmd, "||") || strings.Contains(cmd, ";") || strings.Contains(cmd, "|") {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(cmd))
	for _, prefix := range readOnlyCmdPrefixes {
		if lower == prefix || strings.HasPrefix(lower, prefix+" ") {
			return true
		}
	}
	return false
}

func buildFileMutationPreview(toolName, resolvedPath string, args map[string]any) string {
	switch toolName {
	case "write":
		content, _ := args["content"].(string)
		return fmt.Sprintf("目标文件: %s\n写入内容预览:\n%s", resolvedPath, truncateMultiline(content, 500))
	case "edit":
		oldStr, _ := args["oldText"].(string)
		newStr, _ := args["newText"].(string)
		if oldStr == "" && newStr == "" {
			if edits, ok := args["edits"].([]any); ok && len(edits) > 0 {
				if first, ok := edits[0].(map[string]any); ok {
					oldStr, _ = first["oldText"].(string)
					newStr, _ = first["newText"].(string)
				}
			}
		}
		return fmt.Sprintf("目标文件: %s\n--- 原内容片段 ---\n%s\n+++ 新内容片段 +++\n%s",
			resolvedPath, truncateMultiline(oldStr, 260), truncateMultiline(newStr, 260))
	default:
		return resolvedPath
	}
}

func truncateOneLine(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

func truncateMultiline(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "\n... (已截断)"
}

// SafetyGate coordinates Human-in-the-Loop tool call interception and approval suspension.
type SafetyGate struct {
	mu       sync.Mutex
	policy   store.SafetyPolicy
	router   *EnvironmentRouter
	termSink TerminalStreamSink
	onPrompt ApprovalPromptHandler

	nextSeq int
	pending map[string]*ApprovalRequest
	trusted map[string]bool
}

// NewSafetyGate creates a SafetyGate bound to the EnvironmentRouter and policy.
func NewSafetyGate(router *EnvironmentRouter, policy store.SafetyPolicy, termSink TerminalStreamSink) *SafetyGate {
	if policy == "" {
		policy = store.SafetyPolicyRemoteHighRisk
	}
	return &SafetyGate{
		policy:   policy,
		router:   router,
		termSink: termSink,
		pending:  map[string]*ApprovalRequest{},
		trusted:  map[string]bool{},
	}
}

// SetPromptHandler registers the UI callback invoked when a tool call requires approval.
func (g *SafetyGate) SetPromptHandler(fn ApprovalPromptHandler) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onPrompt = fn
}

// SetPolicy updates the active safety interception policy.
func (g *SafetyGate) SetPolicy(policy store.SafetyPolicy) {
	if policy == "" {
		policy = store.SafetyPolicyRemoteHighRisk
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.policy = policy
}

// Policy returns the active safety interception policy.
func (g *SafetyGate) Policy() store.SafetyPolicy {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.policy
}

// TrustedCount returns the number of session-trusted operation signatures.
func (g *SafetyGate) TrustedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.trusted)
}

// ClearTrusted clears all session-trusted operation signatures.
func (g *SafetyGate) ClearTrusted() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.trusted = map[string]bool{}
}

// BeforeToolCall is the sky-valley/pi agent.BeforeToolCallFunc hook.
// When a risky operation is detected, it suspends the calling goroutine on a channel
// until the user approves/rejects in the UI or the context is cancelled.
func (g *SafetyGate) BeforeToolCall(ctx context.Context, c agent.BeforeToolCallContext) *agent.BeforeToolCallResult {
	g.mu.Lock()
	policy := g.policy
	promptFn := g.onPrompt
	var target store.WorkspaceTarget
	if g.router != nil {
		target = g.router.Target()
	}
	g.mu.Unlock()

	assessment := AssessToolCallRisk(target, policy, c.ToolCall.Name, c.Args)
	if assessment == nil {
		return nil
	}

	g.mu.Lock()
	if assessment.Signature != "" && g.trusted[assessment.Signature] {
		g.mu.Unlock()
		return nil
	}
	if promptFn == nil {
		// No interactive UI attached (e.g. headless background test without prompt handler).
		g.mu.Unlock()
		return nil
	}

	g.nextSeq++
	req := &ApprovalRequest{
		ID:         fmt.Sprintf("approval-%d", g.nextSeq),
		ToolCallID: c.ToolCall.ID,
		ToolName:   c.ToolCall.Name,
		Target:     target,
		Assessment: *assessment,
		CreatedAt:  time.Now(),
		responseCh: make(chan ApprovalDecision, 1),
	}
	g.pending[req.ID] = req
	g.mu.Unlock()

	if g.termSink != nil {
		g.termSink(fmt.Sprintf("\n🛡️ [安全拦截] 挂起高危操作 [%s] (%s)，等待人工审批: %s\n",
			req.ToolName, assessment.Category, assessment.TargetSummary))
	}

	promptFn(req)

	select {
	case dec := <-req.responseCh:
		g.mu.Lock()
		delete(g.pending, req.ID)
		if dec.Approved && dec.TrustSignature && assessment.Signature != "" {
			g.trusted[assessment.Signature] = true
		}
		g.mu.Unlock()

		if dec.Approved {
			if g.termSink != nil {
				g.termSink(fmt.Sprintf("✅ [安全拦截] 用户已批准执行 [%s]: %s\n", req.ToolName, assessment.TargetSummary))
			}
			return nil
		}

		reason := fmt.Sprintf("用户拒绝了高危操作 [%s: %s] (%s)。", req.ToolName, assessment.Category, assessment.TargetSummary)
		if fb := strings.TrimSpace(dec.Feedback); fb != "" {
			reason += " 用户反馈指示: " + fb
		} else {
			reason += " 请不要强行重试该危险操作，改为采用更安全的只读检查或向用户询问替代方案。"
		}
		if g.termSink != nil {
			g.termSink(fmt.Sprintf("🚫 [安全拦截] 用户已拒绝执行 [%s]: %s\n", req.ToolName, assessment.TargetSummary))
		}
		return &agent.BeforeToolCallResult{
			Block:  true,
			Reason: reason,
		}

	case <-ctx.Done():
		g.mu.Lock()
		delete(g.pending, req.ID)
		g.mu.Unlock()
		return &agent.BeforeToolCallResult{
			Block:     true,
			Reason:    "操作审批在等待期间被取消 (Aborted)。",
			Terminate: true,
		}
	}
}

// Resolve delivers the user's approval/rejection decision to a suspended tool call.
func (g *SafetyGate) Resolve(reqID string, decision ApprovalDecision) bool {
	g.mu.Lock()
	req, ok := g.pending[reqID]
	if ok {
		delete(g.pending, reqID)
	}
	g.mu.Unlock()
	if !ok || req == nil {
		return false
	}
	select {
	case req.responseCh <- decision:
		return true
	default:
		return false
	}
}
