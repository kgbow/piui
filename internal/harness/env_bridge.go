package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path"
	"strconv"
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
	if !initial.IsRemote && initial.WorkDir != "" {
		initial.WorkDir = store.ExpandLocalPath(initial.WorkDir)
	}
	return &EnvironmentRouter{
		target:      initial,
		onTermChunk: sink,
	}
}

// SetTarget hot-swaps the active execution target mid-session.
func (r *EnvironmentRouter) SetTarget(t store.WorkspaceTarget) {
	if !t.IsRemote && t.WorkDir != "" {
		t.WorkDir = store.ExpandLocalPath(t.WorkDir)
	}
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

// ExecutionEnv returns a coding.ExecutionEnv (LocalEnv or multiplexed SSHEnv) for the active target.
func (r *EnvironmentRouter) ExecutionEnv() coding.ExecutionEnv {
	t := r.Target()
	if t.IsRemote {
		return NewSSHEnv(t.SSHHost, t.WorkDir)
	}
	return coding.NewLocalEnv(store.ExpandLocalPath(t.WorkDir))
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

// BuildTools returns the core coding tools (read, write, edit, bash, ls, find, grep)
// plus the private workspace_probe tool wired through this router.
func (r *EnvironmentRouter) BuildTools(fallbackCwd string) []agent.AgentTool {
	fallbackCwd = store.ExpandLocalPath(fallbackCwd)
	baseRead, _ := coding.CreateTool("read", fallbackCwd)
	baseWrite, _ := coding.CreateTool("write", fallbackCwd)
	baseEdit, _ := coding.CreateTool("edit", fallbackCwd)
	baseBash := coding.CreateBashTool(fallbackCwd, nil)
	baseLs, _ := coding.CreateTool("ls", fallbackCwd)
	baseFind, _ := coding.CreateTool("find", fallbackCwd)
	baseGrep, _ := coding.CreateTool("grep", fallbackCwd)

	return []agent.AgentTool{
		r.wrapFileTool("read", baseRead),
		r.wrapFileTool("write", baseWrite),
		r.wrapFileTool("edit", baseEdit),
		r.wrapBashTool(baseBash),
		r.wrapFileTool("ls", baseLs),
		r.wrapFileTool("find", baseFind),
		r.wrapFileTool("grep", baseGrep),
		CreateWorkspaceProbeTool(r),
	}
}

func (r *EnvironmentRouter) wrapFileTool(name string, schemaTemplate agent.AgentTool) agent.AgentTool {
	t := schemaTemplate
	t.Execute = func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		target := r.Target()
		if !target.IsRemote {
			localCwd := store.ExpandLocalPath(target.WorkDir)
			localTool, err := coding.CreateTool(name, localCwd)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			if localTool.PrepareArguments != nil {
				params = localTool.PrepareArguments(params)
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
		localCwd := target.WorkDir
		if !target.IsRemote {
			localCwd = store.ExpandLocalPath(localCwd)
		}

		cmdStr, _ := params["command"].(string)
		if target.IsRemote {
			wd := target.WorkDir
			if wd == "" {
				wd = "~"
			}
			r.emitTerm(fmt.Sprintf("\n[ssh:%s:%s] $ %s\n", target.ShortName(), wd, cmdStr))
		} else {
			r.emitTerm(fmt.Sprintf("\n[local:%s] $ %s\n", store.ShortenHomePath(localCwd), cmdStr))
		}

		var baseOps coding.BashOperations
		if target.IsRemote {
			baseOps = r.sshBashOperations(target)
		} else {
			baseOps = coding.LocalBashOperations(coding.LocalBashOptions{})
		}

		hookedOps := coding.BashOperations{
			Exec: func(ctx context.Context, command, cwd string, options coding.BashExecOptions) (*int, error) {
				startedAt := time.Now()
				origOnData := options.OnData
				options.OnData = func(data []byte) {
					if origOnData != nil {
						origOnData(data)
					}
					r.emitTerm(string(data))
				}
				code, err := baseOps.Exec(ctx, command, cwd, options)
				durMs := time.Since(startedAt).Milliseconds()
				if durMs < 1 {
					durMs = 1
				}
				if err != nil {
					r.emitTerm(fmt.Sprintf("\n[error: %v · %dms]\n", err, durMs))
				} else if code != nil {
					r.emitTerm(fmt.Sprintf("\n[exit code: %d · %dms]\n", *code, durMs))
				}
				return code, err
			},
		}

		tool := coding.CreateBashTool(localCwd, &coding.BashToolOptions{
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
				remoteScript = fmt.Sprintf("cd -- %s && %s", shellQuotePath(cwd), command)
			}

			args := sshMultiplexArgs(target.SSHHost, "bash", "-lc", remoteScript)
			cmd := exec.CommandContext(execCtx, "ssh", args...)
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
	pathArg = strings.TrimSpace(pathArg)
	remotePath := target.WorkDir
	if remotePath == "" {
		remotePath = "~"
	}
	if pathArg != "" {
		remotePath = resolveRemotePath(target.WorkDir, pathArg)
	} else if name == "read" || name == "write" || name == "edit" {
		return agent.AgentToolResult{}, fmt.Errorf("%s: missing path argument", name)
	}

	switch name {
	case "read":
		rawBytes, err := sshReadFile(ctx, target.SSHHost, remotePath)
		if err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("remote read %s failed: %w", remotePath, err)
		}
		return formatReadToolResult(string(rawBytes), pathArg, params)

	case "write":
		content, _ := params["content"].(string)
		dir := path.Dir(remotePath)
		script := fmt.Sprintf("mkdir -p -- %s && cat > %s", shellQuotePath(dir), shellQuotePath(remotePath))
		if err := sshWriteFile(ctx, target.SSHHost, script, content); err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("remote write %s failed: %w", remotePath, err)
		}
		return agent.AgentToolResult{
			Content: ai.ContentList{ai.TextContent{Text: fmt.Sprintf("Successfully wrote to %s", pathArg)}},
		}, nil

	case "edit":
		origBytes, err := sshReadFile(ctx, target.SSHHost, remotePath)
		if err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("remote edit could not read %s: %w", remotePath, err)
		}
		updated, count, err := applyRemoteEdits(string(origBytes), params)
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		script := "cat > " + shellQuotePath(remotePath)
		if err := sshWriteFile(ctx, target.SSHHost, script, updated); err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("remote edit write %s failed: %w", remotePath, err)
		}
		return agent.AgentToolResult{
			Content: ai.ContentList{ai.TextContent{Text: fmt.Sprintf("Successfully replaced %d block(s) in %s.", count, pathArg)}},
		}, nil

	case "ls":
		limit := 500
		if l, ok := paramInt(params, "limit"); ok && l > 0 {
			limit = l
		}
		script := fmt.Sprintf("ls -1Ap -- %s | head -n %d", shellQuotePath(remotePath), limit)
		out, err := runRemoteReadOnlyScript(ctx, target.SSHHost, "", script)
		if err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("remote ls %s failed: %w", remotePath, err)
		}
		trimmed := strings.TrimSpace(out)
		if trimmed == "" {
			trimmed = "(empty directory)"
		}
		tr := coding.TruncateHead(trimmed, 0, 0)
		return agent.AgentToolResult{
			Content:           ai.ContentList{ai.TextContent{Text: tr.Content}},
			StructuredContent: tr.Content,
		}, nil

	case "find":
		pattern, _ := params["pattern"].(string)
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return agent.AgentToolResult{}, fmt.Errorf("find: missing pattern argument")
		}
		limit := 1000
		if l, ok := paramInt(params, "limit"); ok && l > 0 {
			limit = l
		}
		script := fmt.Sprintf("find . -name %s -print 2>/dev/null | sed 's|^\\./||' | head -n %d", shellQuote(pattern), limit)
		out, err := runRemoteReadOnlyScript(ctx, target.SSHHost, remotePath, script)
		if err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("remote find in %s failed: %w", remotePath, err)
		}
		trimmed := strings.TrimSpace(out)
		if trimmed == "" {
			trimmed = "No files found matching pattern"
		}
		tr := coding.TruncateHead(trimmed, 0, 0)
		return agent.AgentToolResult{
			Content:           ai.ContentList{ai.TextContent{Text: tr.Content}},
			StructuredContent: tr.Content,
		}, nil

	case "grep":
		pattern, _ := params["pattern"].(string)
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return agent.AgentToolResult{}, fmt.Errorf("grep: missing pattern argument")
		}
		limit := 100
		if l, ok := paramInt(params, "limit"); ok && l > 0 {
			limit = l
		}
		flags := "-rn"
		if ic, ok := params["ignoreCase"].(bool); ok && ic {
			flags += "i"
		}
		if lit, ok := params["literal"].(bool); ok && lit {
			flags += "F"
		} else {
			flags += "E"
		}
		script := fmt.Sprintf("(grep %s -- %s . 2>/dev/null || true) | sed 's|^\\./||' | head -n %d", flags, shellQuote(pattern), limit)
		out, err := runRemoteReadOnlyScript(ctx, target.SSHHost, remotePath, script)
		if err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("remote grep in %s failed: %w", remotePath, err)
		}
		trimmed := strings.TrimSpace(out)
		if trimmed == "" {
			trimmed = "No matches found"
		}
		tr := coding.TruncateHead(trimmed, 0, 0)
		return agent.AgentToolResult{
			Content:           ai.ContentList{ai.TextContent{Text: tr.Content}},
			StructuredContent: tr.Content,
		}, nil
	}

	return agent.AgentToolResult{}, fmt.Errorf("unsupported remote tool: %s", name)
}

func sshReadFile(ctx context.Context, sshHost, remotePath string) ([]byte, error) {
	script := "cat -- " + shellQuotePath(remotePath)
	args := sshMultiplexArgs(sshHost, "bash", "-c", script)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return nil, fmt.Errorf("%v (%s)", err, errMsg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func sshWriteFile(ctx context.Context, sshHost, script, content string) error {
	args := sshMultiplexArgs(sshHost, "bash", "-c", script)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = strings.NewReader(content)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("%v (%s)", err, errMsg)
		}
		return err
	}
	return nil
}

// formatReadToolResult applies offset, limit, and TruncateHead matching sky-valley/pi/coding's read tool.
func formatReadToolResult(content, displayPath string, params map[string]any) (agent.AgentToolResult, error) {
	allLines := strings.Split(content, "\n")
	totalFileLines := len(allLines)

	offset, hasOffset := paramInt(params, "offset")
	startLine := 0
	if hasOffset && offset > 0 {
		startLine = offset - 1
	}
	startLineDisplay := startLine + 1
	if startLine >= len(allLines) {
		return agent.AgentToolResult{}, fmt.Errorf("Offset %d is beyond end of file (%d lines total)", offset, len(allLines))
	}

	var selected string
	hasLimit := false
	userLimitedLines := 0
	if limit, ok := paramInt(params, "limit"); ok {
		hasLimit = true
		endLine := startLine + limit
		if endLine > len(allLines) {
			endLine = len(allLines)
		}
		effEnd := endLine
		if effEnd < 0 {
			effEnd = len(allLines) + effEnd
			if effEnd < 0 {
				effEnd = 0
			}
		}
		if effEnd < startLine {
			effEnd = startLine
		}
		selected = strings.Join(allLines[startLine:effEnd], "\n")
		userLimitedLines = endLine - startLine
	} else {
		selected = strings.Join(allLines[startLine:], "\n")
	}

	tr := coding.TruncateHead(selected, 0, 0)
	var out string
	switch {
	case tr.FirstLineExceedsLimit:
		firstLineSize := coding.FormatSize(len(allLines[startLine]))
		out = fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use bash: sed -n '%dp' %s | head -c %d]",
			startLineDisplay, firstLineSize, coding.FormatSize(coding.DefaultMaxBytes), startLineDisplay, displayPath, coding.DefaultMaxBytes)
	case tr.Truncated:
		endLineDisplay := startLineDisplay + tr.OutputLines - 1
		nextOffset := endLineDisplay + 1
		out = tr.Content
		if tr.TruncatedBy == "lines" {
			out += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", startLineDisplay, endLineDisplay, totalFileLines, nextOffset)
		} else {
			out += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Use offset=%d to continue.]", startLineDisplay, endLineDisplay, totalFileLines, coding.FormatSize(coding.DefaultMaxBytes), nextOffset)
		}
	case hasLimit && startLine+userLimitedLines < len(allLines):
		remaining := len(allLines) - (startLine + userLimitedLines)
		nextOffset := startLine + userLimitedLines + 1
		out = fmt.Sprintf("%s\n\n[%d more lines in file. Use offset=%d to continue.]", tr.Content, remaining, nextOffset)
	default:
		out = tr.Content
	}

	res := agent.AgentToolResult{
		Content:           ai.ContentList{ai.TextContent{Text: out}},
		StructuredContent: out,
	}
	if tr.Truncated {
		res.Details = map[string]any{"truncation": tr}
	}
	return res, nil
}

func paramInt(params map[string]any, key string) (int, bool) {
	if params == nil {
		return 0, false
	}
	v, exists := params[key]
	if !exists || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i, true
		}
	}
	return 0, false
}

func applyRemoteEdits(content string, params map[string]any) (string, int, error) {
	var edits []map[string]any
	if rawEdits, ok := params["edits"].([]any); ok {
		for _, item := range rawEdits {
			if m, ok := item.(map[string]any); ok {
				edits = append(edits, m)
			}
		}
	} else if rawStr, ok := params["edits"].(string); ok {
		var parsed any
		if json.Unmarshal([]byte(rawStr), &parsed) == nil {
			switch p := parsed.(type) {
			case []any:
				for _, item := range p {
					if m, ok := item.(map[string]any); ok {
						edits = append(edits, m)
					}
				}
			case map[string]any:
				edits = append(edits, p)
			}
		}
	} else if singleMap, ok := params["edits"].(map[string]any); ok {
		edits = append(edits, singleMap)
	}

	if oldText, ok := params["oldText"].(string); ok {
		if newText, okNew := params["newText"].(string); okNew {
			edits = append(edits, map[string]any{"oldText": oldText, "newText": newText})
		}
	}
	if len(edits) == 0 {
		return "", 0, fmt.Errorf("edit requires at least one replacement")
	}

	// Preserve UTF-8 BOM and CRLF line endings across replacements.
	bom := ""
	raw := content
	if strings.HasPrefix(raw, "\uFEFF") {
		bom = "\uFEFF"
		raw = strings.TrimPrefix(raw, "\uFEFF")
	}
	useCRLF := hasCRLFBeforeLF(raw)
	current := normalizeLF(raw)

	for i, e := range edits {
		oldStr, _ := e["oldText"].(string)
		newStr, _ := e["newText"].(string)
		oldNorm := normalizeLF(oldStr)
		newNorm := normalizeLF(newStr)
		if oldNorm == "" {
			return "", 0, fmt.Errorf("edit block %d: oldText cannot be empty", i+1)
		}
		matches := strings.Count(current, oldNorm)
		if matches == 0 {
			return "", 0, fmt.Errorf("edit block %d: oldText not found in file", i+1)
		}
		if matches > 1 {
			return "", 0, fmt.Errorf("edit block %d: oldText matches %d locations in file; include more surrounding context", i+1, matches)
		}
		current = strings.Replace(current, oldNorm, newNorm, 1)
	}

	if useCRLF {
		current = strings.ReplaceAll(current, "\n", "\r\n")
	}
	return bom + current, len(edits), nil
}

func normalizeLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func hasCRLFBeforeLF(s string) bool {
	crlfIdx := strings.Index(s, "\r\n")
	lfIdx := strings.Index(s, "\n")
	return crlfIdx != -1 && lfIdx != -1 && crlfIdx < lfIdx
}

func resolveRemotePath(workDir, p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~/") || p == "~" {
		return p
	}
	workDir = strings.TrimSpace(workDir)
	if workDir == "" || workDir == "~" {
		return "~/" + p
	}
	return strings.TrimSuffix(workDir, "/") + "/" + p
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellQuotePath quotes a remote POSIX path for bash while allowing a leading "~" or "~/"
// to expand to "$HOME" on the remote machine.
func shellQuotePath(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if strings.HasPrefix(p, "~/") {
		return `"$HOME/"` + shellQuote(p[2:])
	}
	return shellQuote(p)
}

type chunkWriter struct {
	onData func([]byte)
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	if w.onData != nil && len(p) > 0 {
		cp := make([]byte, len(p))
		copy(cp, p)
		w.onData(cp)
	}
	return len(p), nil
}
