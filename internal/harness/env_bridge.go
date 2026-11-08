package harness

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/coding"

	"piui/internal/store"
)

// TerminalStreamSink receives real-time command execution headers and stdout/stderr chunks.
type TerminalStreamSink func(chunk string)

// EnvironmentRouter routes tool calls (read, write, edit, bash) dynamically to the
// currently active WorkspaceTarget without recreating or breaking the active Session.
type EnvironmentRouter struct {
	mu          sync.RWMutex
	target      store.WorkspaceTarget
	switched    bool
	onTermChunk TerminalStreamSink
}

// NewEnvironmentRouter creates a router bound to an initial WorkspaceTarget.
func NewEnvironmentRouter(initial store.WorkspaceTarget, sink TerminalStreamSink) *EnvironmentRouter {
	return &EnvironmentRouter{
		target:      initial,
		onTermChunk: sink,
	}
}

// SetTarget hot-swaps the active execution target mid-session.
func (r *EnvironmentRouter) SetTarget(t store.WorkspaceTarget) {
	r.mu.Lock()
	changed := r.target.ID != t.ID || r.target.WorkDir != t.WorkDir || r.target.SSHHost != t.SSHHost || r.target.IsRemote != t.IsRemote
	r.target = t
	if changed {
		r.switched = true
	}
	r.mu.Unlock()

	if changed && r.onTermChunk != nil {
		mode := "LOCAL"
		loc := t.WorkDir
		if t.IsRemote {
			mode = "SSH (" + t.SSHHost + ")"
		}
		r.onTermChunk(fmt.Sprintf("\n─── [Target Switched → %s : %s] ───\n", mode, loc))
	}
}

// Target returns the currently active WorkspaceTarget.
func (r *EnvironmentRouter) Target() store.WorkspaceTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.target
}

// ConsumeSwitchNotice returns a context notice if the target was switched since the last prompt.
func (r *EnvironmentRouter) ConsumeSwitchNotice() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.switched {
		return ""
	}
	r.switched = false
	t := r.target
	if t.IsRemote {
		return fmt.Sprintf("[System Notice: Active execution environment switched to Remote SSH host %q, working directory %q]", t.SSHHost, t.WorkDir)
	}
	return fmt.Sprintf("[System Notice: Active execution environment switched to Local workspace, working directory %q]", t.WorkDir)
}

func (r *EnvironmentRouter) emitTerm(s string) {
	if r.onTermChunk != nil && s != "" {
		r.onTermChunk(s)
	}
}

// BuildTools returns the 4 core tools (read, write, edit, bash) wired through this router.
func (r *EnvironmentRouter) BuildTools(fallbackCwd string) []agent.AgentTool {
	baseRead, _ := coding.CreateTool("read", fallbackCwd)
	baseWrite, _ := coding.CreateTool("write", fallbackCwd)
	baseEdit, _ := coding.CreateTool("edit", fallbackCwd)
	baseBash := coding.CreateBashTool(fallbackCwd, nil)

	return []agent.AgentTool{
		r.wrapFileTool("read", baseRead),
		r.wrapFileTool("write", baseWrite),
		r.wrapFileTool("edit", baseEdit),
		r.wrapBashTool(baseBash),
	}
}

func (r *EnvironmentRouter) wrapFileTool(name string, schemaTemplate agent.AgentTool) agent.AgentTool {
	t := schemaTemplate
	t.Execute = func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		target := r.Target()
		if !target.IsRemote {
			localTool, err := coding.CreateTool(name, target.WorkDir)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			return localTool.Execute(ctx, id, params, onUpdate)
		}
		return r.execRemoteFileTool(ctx, target, name, params)
	}
	return t
}

func (r *EnvironmentRouter) wrapBashTool(schemaTemplate agent.AgentTool) agent.AgentTool {
	t := schemaTemplate
	t.Execute = func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		target := r.Target()
		cmdStr, _ := params["command"].(string)
		ts := time.Now().Format("15:04:05")
		if target.IsRemote {
			r.emitTerm(fmt.Sprintf("\n[%s] 🌐 %s:%s $ %s\n", ts, target.SSHHost, target.WorkDir, cmdStr))
		} else {
			r.emitTerm(fmt.Sprintf("\n[%s] 💻 %s $ %s\n", ts, filepath.Base(target.WorkDir), cmdStr))
		}

		var baseOps coding.BashOperations
		if target.IsRemote {
			baseOps = r.sshBashOperations(target)
		} else {
			baseOps = coding.LocalBashOperations(coding.LocalBashOptions{})
		}

		hookedOps := coding.BashOperations{
			Exec: func(ctx context.Context, command, cwd string, options coding.BashExecOptions) (*int, error) {
				origOnData := options.OnData
				options.OnData = func(data []byte) {
					if origOnData != nil {
						origOnData(data)
					}
					r.emitTerm(string(data))
				}
				code, err := baseOps.Exec(ctx, command, cwd, options)
				if err != nil {
					r.emitTerm(fmt.Sprintf("\n[error: %v]\n", err))
				} else if code != nil && *code != 0 {
					r.emitTerm(fmt.Sprintf("\n[exit code: %d]\n", *code))
				}
				return code, err
			},
		}

		tool := coding.CreateBashTool(target.WorkDir, &coding.BashToolOptions{
			Operations: &hookedOps,
		})
		return tool.Execute(ctx, id, params, onUpdate)
	}
	return t
}

// sshBashOperations executes bash commands on a remote host via local ssh client
// (automatically honoring ~/.ssh/config, ProxyJump, Tailscale, and ssh-agent).
func (r *EnvironmentRouter) sshBashOperations(target store.WorkspaceTarget) coding.BashOperations {
	return coding.BashOperations{
		Exec: func(ctx context.Context, command, cwd string, options coding.BashExecOptions) (*int, error) {
			execCtx := ctx
			var cancel context.CancelFunc
			if options.TimeoutSeconds != nil && *options.TimeoutSeconds > 0 {
				d := time.Duration(*options.TimeoutSeconds * float64(time.Second))
				execCtx, cancel = context.WithTimeout(ctx, d)
				defer cancel()
			}

			remoteScript := command
			if cwd != "" && cwd != "~" {
				remoteScript = fmt.Sprintf("cd %s && %s", shellQuote(cwd), command)
			}

			cmd := exec.CommandContext(execCtx, "ssh", "-o", "BatchMode=yes", target.SSHHost, "bash", "-lc", remoteScript)
			stream := &chunkWriter{onData: options.OnData}
			cmd.Stdout = stream
			cmd.Stderr = stream

			err := cmd.Run()
			if ctx.Err() != nil {
				return nil, coding.ErrShellAborted
			}
			if execCtx.Err() == context.DeadlineExceeded && options.TimeoutSeconds != nil {
				return nil, &coding.ShellTimeoutError{Seconds: *options.TimeoutSeconds}
			}
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					code := exitErr.ExitCode()
					return &code, nil
				}
				return nil, err
			}
			zero := 0
			return &zero, nil
		},
	}
}

func (r *EnvironmentRouter) execRemoteFileTool(ctx context.Context, target store.WorkspaceTarget, name string, params map[string]any) (agent.AgentToolResult, error) {
	pathArg, _ := params["path"].(string)
	if pathArg == "" {
		return agent.AgentToolResult{}, fmt.Errorf("%s: missing path argument", name)
	}
	remotePath := resolveRemotePath(target.WorkDir, pathArg)

	switch name {
	case "read":
		cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", target.SSHHost, "cat", "--", remotePath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("remote read %s failed: %v (%s)", remotePath, err, strings.TrimSpace(string(out)))
		}
		lines := strings.Split(string(out), "\n")
		var numbered strings.Builder
		for i, line := range lines {
			if i == len(lines)-1 && line == "" {
				break
			}
			fmt.Fprintf(&numbered, "%6d\t%s\n", i+1, line)
		}
		return agent.AgentToolResult{
			Content: ai.ContentList{ai.TextContent{Text: numbered.String()}},
		}, nil

	case "write":
		content, _ := params["content"].(string)
		dir := filepath.Dir(remotePath)
		script := fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(dir), shellQuote(remotePath))
		cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", target.SSHHost, "bash", "-c", script)
		cmd.Stdin = strings.NewReader(content)
		if out, err := cmd.CombinedOutput(); err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("remote write %s failed: %v (%s)", remotePath, err, strings.TrimSpace(string(out)))
		}
		return agent.AgentToolResult{
			Content: ai.ContentList{ai.TextContent{Text: fmt.Sprintf("Successfully wrote %d bytes to %s on %s.", len(content), remotePath, target.SSHHost)}},
		}, nil

	case "edit":
		readCmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", target.SSHHost, "cat", "--", remotePath)
		origBytes, err := readCmd.CombinedOutput()
		if err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("remote edit could not read %s: %v", remotePath, err)
		}
		updated, count, err := applyRemoteEdits(string(origBytes), params)
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		writeCmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", target.SSHHost, "bash", "-c", "cat > "+shellQuote(remotePath))
		writeCmd.Stdin = strings.NewReader(updated)
		if out, err := writeCmd.CombinedOutput(); err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("remote edit write %s failed: %v (%s)", remotePath, err, strings.TrimSpace(string(out)))
		}
		return agent.AgentToolResult{
			Content: ai.ContentList{ai.TextContent{Text: fmt.Sprintf("Successfully replaced %d block(s) in %s on %s.", count, remotePath, target.SSHHost)}},
		}, nil
	}

	return agent.AgentToolResult{}, fmt.Errorf("unsupported remote tool: %s", name)
}

func applyRemoteEdits(content string, params map[string]any) (string, int, error) {
	var edits []map[string]any
	if rawEdits, ok := params["edits"].([]any); ok {
		for _, item := range rawEdits {
			if m, ok := item.(map[string]any); ok {
				edits = append(edits, m)
			}
		}
	}
	if oldText, ok := params["oldText"].(string); ok {
		newText, _ := params["newText"].(string)
		edits = append(edits, map[string]any{"oldText": oldText, "newText": newText})
	}
	if len(edits) == 0 {
		return "", 0, fmt.Errorf("edit requires at least one replacement")
	}
	current := content
	for i, e := range edits {
		oldStr, _ := e["oldText"].(string)
		newStr, _ := e["newText"].(string)
		if oldStr == "" || !strings.Contains(current, oldStr) {
			return "", 0, fmt.Errorf("edit block %d: oldText not found in file", i+1)
		}
		current = strings.Replace(current, oldStr, newStr, 1)
	}
	return current, len(edits), nil
}

func resolveRemotePath(workDir, p string) string {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~/") || p == "~" {
		return p
	}
	if workDir == "" || workDir == "~" {
		return p
	}
	return strings.TrimSuffix(workDir, "/") + "/" + p
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type chunkWriter struct {
	onData func([]byte)
	buf    bytes.Buffer
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	if w.onData != nil && len(p) > 0 {
		cp := make([]byte, len(p))
		copy(cp, p)
		w.onData(cp)
	}
	return len(p), nil
}
