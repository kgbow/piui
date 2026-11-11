package harness

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
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
	mu            sync.RWMutex
	target        store.WorkspaceTarget
	switched      bool
	onTermChunk   TerminalStreamSink
	pinnedTargets map[string]store.WorkspaceTarget
	changes       *ChangeTracker
}

// NewEnvironmentRouter creates a router bound to an initial WorkspaceTarget.
func NewEnvironmentRouter(initial store.WorkspaceTarget, sink TerminalStreamSink) *EnvironmentRouter {
	if !initial.IsRemote && initial.WorkDir != "" {
		initial.WorkDir = store.ExpandLocalPath(initial.WorkDir)
	}
	return &EnvironmentRouter{
		target:        initial,
		onTermChunk:   sink,
		pinnedTargets: map[string]store.WorkspaceTarget{},
		changes:       NewChangeTracker(),
	}
}

// Changes returns the file mutation snapshot tracker for diff rendering and one-click rollback.
func (r *EnvironmentRouter) Changes() *ChangeTracker {
	return r.changes
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

// SetTargetQuiet binds the router to a new workspace target at a clean session boundary
// without emitting a mid-conversation switch notice.
func (r *EnvironmentRouter) SetTargetQuiet(t store.WorkspaceTarget) {
	if !t.IsRemote && t.WorkDir != "" {
		t.WorkDir = store.ExpandLocalPath(t.WorkDir)
	}
	r.mu.Lock()
	r.target = t
	r.switched = false
	r.pinnedTargets = map[string]store.WorkspaceTarget{}
	r.mu.Unlock()
}

// UpdateTargetMetadata updates non-routing metadata (such as Trusted or Name) on the active target
// without triggering a target-switch prompt notice.
func (r *EnvironmentRouter) UpdateTargetMetadata(t store.WorkspaceTarget) {
	if !t.IsRemote && t.WorkDir != "" {
		t.WorkDir = store.ExpandLocalPath(t.WorkDir)
	}
	r.mu.Lock()
	r.target = t
	r.mu.Unlock()
}

// Target returns the currently active WorkspaceTarget.
func (r *EnvironmentRouter) Target() store.WorkspaceTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.target
}

// PinToolTarget snapshots and binds the current WorkspaceTarget to toolCallID so that
// safety assessment, user approval, and actual tool execution are guaranteed to operate
// on the exact same target even if the user hot-switches targets mid-flight.
func (r *EnvironmentRouter) PinToolTarget(toolCallID string) store.WorkspaceTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.target
	if toolCallID != "" {
		if r.pinnedTargets == nil {
			r.pinnedTargets = map[string]store.WorkspaceTarget{}
		}
		r.pinnedTargets[toolCallID] = t
	}
	return t
}

// PeekToolTarget returns the pinned target for toolCallID if present, or the active target otherwise.
func (r *EnvironmentRouter) PeekToolTarget(toolCallID string) store.WorkspaceTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if toolCallID != "" && r.pinnedTargets != nil {
		if t, ok := r.pinnedTargets[toolCallID]; ok {
			return t
		}
	}
	return r.target
}

// ConsumeToolTarget pops and returns the pinned target for toolCallID, falling back to Target()
// when invoked directly outside the BeforeToolCall pipeline.
func (r *EnvironmentRouter) ConsumeToolTarget(toolCallID string) store.WorkspaceTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	if toolCallID != "" && r.pinnedTargets != nil {
		if t, ok := r.pinnedTargets[toolCallID]; ok {
			delete(r.pinnedTargets, toolCallID)
			return t
		}
	}
	return r.target
}

// ReleaseToolTarget removes a pinned target entry when a tool call is blocked or cancelled.
func (r *EnvironmentRouter) ReleaseToolTarget(toolCallID string) {
	if toolCallID == "" {
		return
	}
	r.mu.Lock()
	delete(r.pinnedTargets, toolCallID)
	r.mu.Unlock()
}

// ClearPinnedTargets clears all in-flight pinned tool targets (e.g. on session reset).
func (r *EnvironmentRouter) ClearPinnedTargets() {
	r.mu.Lock()
	r.pinnedTargets = map[string]store.WorkspaceTarget{}
	r.mu.Unlock()
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
		target := r.ConsumeToolTarget(id)
		if schemaTemplate.PrepareArguments != nil {
			if prepared := schemaTemplate.PrepareArguments(params); prepared != nil {
				params = prepared
			}
		}

		trackMutation := (name == "edit" || name == "write") && id != "" && r.changes != nil
		var (
			resolvedPath string
			displayPath  string
			existed      bool
			beforeBytes  []byte
			beforeOK     bool
		)
		if trackMutation {
			rawPath, _ := params["path"].(string)
			resolvedPath, displayPath, existed, beforeBytes, beforeOK = CaptureFileState(ctx, target, target.WorkDir, rawPath)
		}

		var (
			res agent.AgentToolResult
			err error
		)
		if !target.IsRemote {
			localCwd := store.ExpandLocalPath(target.WorkDir)
			localTool, toolErr := coding.CreateTool(name, localCwd)
			if toolErr != nil {
				return agent.AgentToolResult{}, toolErr
			}
			res, err = localTool.Execute(ctx, id, params, onUpdate)
		} else {
			res, err = r.execRemoteFileTool(ctx, target, name, params)
		}

		if err == nil && beforeOK {
			var afterBytes []byte
			afterOK := false
			if name == "write" {
				if contentStr, ok := params["content"].(string); ok && len(contentStr) <= maxTrackedMutationBytes {
					afterBytes = []byte(contentStr)
					afterOK = true
				}
			}
			if !afterOK {
				rawPath, _ := params["path"].(string)
				_, _, _, afterBytes, afterOK = CaptureFileState(ctx, target, target.WorkDir, rawPath)
			}
			if afterOK {
				snap := BuildMutationSnapshot(id, name, target, resolvedPath, displayPath, existed, beforeBytes, afterBytes)
				r.changes.Record(snap)
			}
		}
		return res, err
	}
	return t
}

func (r *EnvironmentRouter) wrapBashTool(schemaTemplate agent.AgentTool) agent.AgentTool {
	t := schemaTemplate
	t.Execute = func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		target := r.ConsumeToolTarget(id)
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

// ExecDirectBash executes an explicit user "! <command>" shell command on the active Local or Remote SSH
// workspace, streaming live partial output via onUpdate and returning final combined output + duration.
func (r *EnvironmentRouter) ExecDirectBash(ctx context.Context, command string, onUpdate func(partial string)) (string, int64, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", 0, nil
	}
	target := r.Target()
	localCwd := target.WorkDir
	if !target.IsRemote {
		localCwd = store.ExpandLocalPath(localCwd)
	}

	var baseOps coding.BashOperations
	if target.IsRemote {
		baseOps = r.sshBashOperations(target)
	} else {
		baseOps = coding.LocalBashOperations(coding.LocalBashOptions{})
	}

	startedAt := time.Now()
	tool := coding.CreateBashTool(localCwd, &coding.BashToolOptions{
		Operations: &baseOps,
	})
	res, err := tool.Execute(ctx, fmt.Sprintf("direct-%d", startedAt.UnixNano()), map[string]any{
		"command": command,
	}, func(partial agent.AgentToolResult) {
		if onUpdate != nil {
			txt := ai.ContentText(partial.Content, "\n")
			if txt != "" {
				onUpdate(txt)
			}
		}
	})
	durMs := max(int64(1), time.Since(startedAt).Milliseconds())
	out := ai.ContentText(res.Content, "\n")
	if err != nil && out == "" {
		out = err.Error()
	}
	return out, durMs, err
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
				remoteScript = fmt.Sprintf("cd -- %s && {\n%s\n}", shellQuotePath(cwd), command)
			}
			managedScript := wrapRemoteManagedProcessScript(remoteScript)

			stdinR, stdinW, pipeErr := os.Pipe()
			if pipeErr == nil {
				defer func() {
					_ = stdinW.Close()
					_ = stdinR.Close()
				}()
			}

			args := sshBashArgs(target.SSHHost, true, managedScript)
			cmd := exec.CommandContext(execCtx, sshBinary, args...)
			if pipeErr == nil {
				cmd.Stdin = stdinR
			}
			cmd.WaitDelay = 500 * time.Millisecond
			stream := &chunkWriter{onData: options.OnData}
			cmd.Stdout = stream
			cmd.Stderr = stream

			err := cmd.Start()
			if pipeErr == nil {
				_ = stdinR.Close()
			}
			if err == nil {
				doneWatch := make(chan struct{})
				if pipeErr == nil {
					go func() {
						select {
						case <-execCtx.Done():
							_ = stdinW.Close()
						case <-doneWatch:
						}
					}()
				}
				err = cmd.Wait()
				close(doneWatch)
			}
			if pipeErr == nil {
				_ = stdinW.Close()
			}
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
		return execRemoteRead(ctx, target.SSHHost, remotePath, pathArg, params)

	case "write":
		content, _ := params["content"].(string)
		if err := sshWriteFileAtomic(ctx, target.SSHHost, remotePath, true, content); err != nil {
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
		if err := sshWriteFileAtomic(ctx, target.SSHHost, remotePath, false, updated); err != nil {
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
		return execRemoteFind(ctx, target.SSHHost, remotePath, params)

	case "grep":
		return execRemoteGrep(ctx, target.SSHHost, remotePath, params)
	}

	return agent.AgentToolResult{}, fmt.Errorf("unsupported remote tool: %s", name)
}

func execRemoteRead(ctx context.Context, sshHost, remotePath, displayPath string, params map[string]any) (agent.AgentToolResult, error) {
	script := fmt.Sprintf(
		`p=%s
if [ ! -e "$p" ]; then
  echo "ENOENT: no such file or directory" >&2; exit 44
fi
if [ -d "$p" ]; then
  echo "EISDIR: illegal operation on a directory, read" >&2; exit 45
fi
sz=$(wc -c < "$p" 2>/dev/null | tr -d '[:space:]')
if [ -n "$sz" ] && [ "$sz" -gt %d ] 2>/dev/null; then
  echo "LARGE:$sz" >&2; exit 46
fi
cat -- "$p"`,
		shellQuotePath(remotePath),
		maxRemoteReadFullBytes,
	)

	args := sshBashArgs(sshHost, false, script)
	cmd := exec.CommandContext(ctx, sshBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			switch exitErr.ExitCode() {
			case 44:
				return agent.AgentToolResult{}, fmt.Errorf("remote read %s failed: ENOENT: no such file or directory", remotePath)
			case 45:
				return agent.AgentToolResult{}, fmt.Errorf("remote read %s failed: EISDIR: illegal operation on a directory, read", remotePath)
			case 46:
				return execRemoteReadLargeWindow(ctx, sshHost, remotePath, displayPath, params)
			}
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return agent.AgentToolResult{}, fmt.Errorf("remote read %s failed: %v (%s)", remotePath, err, errMsg)
		}
		return agent.AgentToolResult{}, fmt.Errorf("remote read %s failed: %w", remotePath, err)
	}

	rawBytes := stdout.Bytes()
	if mime := detectRemoteImageMimeType(rawBytes); mime != "" {
		b64 := base64.StdEncoding.EncodeToString(rawBytes)
		note := fmt.Sprintf("Read image file [%s]", mime)
		return agent.AgentToolResult{
			Content: ai.ContentList{
				ai.TextContent{Text: note},
				ai.ImageContent{Data: b64, MimeType: mime},
			},
			StructuredContent: note,
		}, nil
	}

	return formatReadToolResult(string(rawBytes), displayPath, params)
}

func execRemoteReadLargeWindow(ctx context.Context, sshHost, remotePath, displayPath string, params map[string]any) (agent.AgentToolResult, error) {
	offset, hasOffset := paramInt(params, "offset")
	startLine := 1
	if hasOffset && offset > 0 {
		startLine = offset
	}
	maxLines := coding.DefaultMaxLines
	hasUserLimit := false
	if limit, ok := paramInt(params, "limit"); ok && limit > 0 {
		hasUserLimit = true
		if limit < maxLines {
			maxLines = limit
		}
	}
	// Fetch at most maxLines+1 and DefaultMaxBytes*2 so TruncateHead still produces standard notices without loading huge files.
	byteWindow := coding.DefaultMaxBytes * 2
	script := fmt.Sprintf(
		`p=%s
total=$(wc -l < "$p" 2>/dev/null | tr -d '[:space:]')
echo "TOTAL_LINES:${total:-0}"
tail -n +%d -- "$p" | head -n %d | head -c %d`,
		shellQuotePath(remotePath),
		startLine,
		maxLines,
		byteWindow,
	)
	out, err := runRemoteReadOnlyScript(ctx, sshHost, "", script)
	if err != nil {
		return agent.AgentToolResult{}, fmt.Errorf("remote windowed read %s failed: %w", remotePath, err)
	}

	nl := strings.IndexByte(out, '\n')
	header := out
	body := ""
	if nl >= 0 {
		header = out[:nl]
		body = strings.TrimSuffix(out[nl+1:], "\n")
	}
	totalLines := 0
	if strings.HasPrefix(header, "TOTAL_LINES:") {
		totalLines, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(header, "TOTAL_LINES:")))
	}
	if totalLines > 0 && startLine > totalLines {
		return agent.AgentToolResult{}, fmt.Errorf("Offset %d is beyond end of file (%d lines total)", startLine, totalLines)
	}

	tr := coding.TruncateHead(body, 0, 0)
	shownLines := tr.OutputLines
	if shownLines < 1 && body != "" {
		shownLines = len(strings.Split(body, "\n"))
	}
	endLine := startLine + shownLines - 1
	nextOffset := endLine + 1
	text := tr.Content
	if tr.Truncated || (totalLines > 0 && endLine < totalLines) || hasUserLimit {
		if totalLines > 0 {
			text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (large remote file streamed). Use offset=%d to continue.]", startLine, endLine, totalLines, nextOffset)
		} else {
			text += fmt.Sprintf("\n\n[Showing lines %d-%d (large remote file streamed). Use offset=%d to continue.]", startLine, endLine, nextOffset)
		}
	}
	return agent.AgentToolResult{
		Content:           ai.ContentList{ai.TextContent{Text: text}},
		StructuredContent: text,
	}, nil
}

func execRemoteFind(ctx context.Context, sshHost, remotePath string, params map[string]any) (agent.AgentToolResult, error) {
	pattern, _ := params["pattern"].(string)
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return agent.AgentToolResult{}, fmt.Errorf("find: missing pattern argument")
	}
	limit := 1000
	if l, ok := paramInt(params, "limit"); ok && l > 0 {
		limit = l
	}

	// Prefer rg --files or git ls-files so .gitignore is respected; fallback to pruned POSIX find.
	script := `if command -v rg >/dev/null 2>&1; then
  rg --files --hidden -g '!.git' 2>/dev/null || true
elif git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git ls-files --cached --others --exclude-standard 2>/dev/null || true
else
  find . \( -name .git -o -name node_modules -o -name vendor -o -name .venv \) -prune -o -not -type d -print 2>/dev/null | sed 's|^\./||'
fi`

	out, err := runRemoteReadOnlyScript(ctx, sshHost, remotePath, script)
	if err != nil {
		return agent.AgentToolResult{}, fmt.Errorf("remote find in %s failed: %w", remotePath, err)
	}

	matched, err := cleanRelativeFindPaths(out, pattern, limit)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	resultBody := "No files found matching pattern"
	if len(matched) > 0 {
		resultBody = strings.Join(matched, "\n")
	}
	tr := coding.TruncateHead(resultBody, 0, 0)
	return agent.AgentToolResult{
		Content:           ai.ContentList{ai.TextContent{Text: tr.Content}},
		StructuredContent: tr.Content,
	}, nil
}

func execRemoteGrep(ctx context.Context, sshHost, remotePath string, params map[string]any) (agent.AgentToolResult, error) {
	pattern, _ := params["pattern"].(string)
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return agent.AgentToolResult{}, fmt.Errorf("grep: missing pattern argument")
	}
	globPat, _ := params["glob"].(string)
	globPat = strings.TrimSpace(globPat)

	limit := 100
	if l, ok := paramInt(params, "limit"); ok {
		limit = l
	}
	if limit < 1 {
		limit = 1
	}

	ctxLines := 0
	if c, ok := paramInt(params, "context"); ok && c > 0 {
		ctxLines = c
	}

	ignoreCase, _ := params["ignoreCase"].(bool)
	literal, _ := params["literal"].(bool)

	// Validate regex locally first for consistent error feedback across Local and SSH.
	if !literal {
		expr := pattern
		if ignoreCase {
			expr = "(?i)" + expr
		}
		if _, err := regexp.Compile(expr); err != nil {
			return agent.AgentToolResult{}, fmt.Errorf("invalid regex: %w", err)
		}
	}

	var rgFlags, gitFlags, grepFlags []string
	if ignoreCase {
		rgFlags = append(rgFlags, "-i")
		gitFlags = append(gitFlags, "-i")
		grepFlags = append(grepFlags, "-i")
	}
	if literal {
		rgFlags = append(rgFlags, "-F")
		gitFlags = append(gitFlags, "-F")
		grepFlags = append(grepFlags, "-F")
	} else {
		gitFlags = append(gitFlags, "-E")
		grepFlags = append(grepFlags, "-E")
	}
	if ctxLines > 0 {
		cFlag := fmt.Sprintf("-C %d", ctxLines)
		rgFlags = append(rgFlags, cFlag)
		gitFlags = append(gitFlags, cFlag)
		grepFlags = append(grepFlags, cFlag)
	}
	if globPat != "" {
		rgFlags = append(rgFlags, "-g "+shellQuote(globPat))
	}

	qPat := shellQuote(pattern)
	qTarget := shellQuotePath(remotePath)
	rgFlagStr := strings.Join(rgFlags, " ")
	gitFlagStr := strings.Join(gitFlags, " ")
	grepFlagStr := strings.Join(grepFlags, " ")

	script := fmt.Sprintf(
		`target=%s
if [ ! -e "$target" ]; then
  echo "ENOENT" >&2; exit 44
fi
if [ -f "$target" ]; then
  if command -v rg >/dev/null 2>&1; then
    rg -n --with-filename --no-heading --color=never %s -- %s "$target" 2>/dev/null || true
  else
    grep -nH %s -- %s "$target" 2>/dev/null || true
  fi
  exit 0
fi
cd -- "$target" || exit 1
if command -v rg >/dev/null 2>&1; then
  rg -n --with-filename --no-heading --color=never %s -- %s . 2>/dev/null || true
elif git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git grep -n --no-color -I --untracked %s -- %s 2>/dev/null || true
else
  grep -rnHI --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=vendor %s -- %s . 2>/dev/null || \
    find . \( -name .git -o -name node_modules -o -name vendor \) -prune -o -type f -exec grep -nHI %s -- %s {} + 2>/dev/null || true
fi`,
		qTarget,
		rgFlagStr, qPat,
		grepFlagStr, qPat,
		rgFlagStr, qPat,
		gitFlagStr, qPat,
		grepFlagStr, qPat,
		grepFlagStr, qPat,
	)

	args := sshBashArgs(sshHost, false, script)
	cmd := exec.CommandContext(ctx, sshBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
			return agent.AgentToolResult{}, fmt.Errorf("Path not found: %s", remotePath)
		}
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return agent.AgentToolResult{}, fmt.Errorf("remote grep in %s failed: %v (%s)", remotePath, err, errMsg)
		}
		return agent.AgentToolResult{}, fmt.Errorf("remote grep in %s failed: %w", remotePath, err)
	}

	formatted, err := filterAndFormatRemoteGrepOutput(stdout.String(), globPat, limit, ctxLines > 0)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	if strings.TrimSpace(formatted) == "" {
		formatted = "No matches found"
	}
	tr := coding.TruncateHead(formatted, 0, 0)
	return agent.AgentToolResult{
		Content:           ai.ContentList{ai.TextContent{Text: tr.Content}},
		StructuredContent: tr.Content,
	}, nil
}

func sshReadFile(ctx context.Context, sshHost, remotePath string) ([]byte, error) {
	script := "cat -- " + shellQuotePath(remotePath)
	args := sshBashArgs(sshHost, false, script)
	cmd := exec.CommandContext(ctx, sshBinary, args...)
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

// sshWriteFileAtomic writes content to a temporary file in the target directory on the
// remote host and atomically renames it into place only after stdin finishes cleanly,
// preventing file truncation or partial writes on connection drops.
func sshWriteFileAtomic(ctx context.Context, sshHost, remotePath string, createParent bool, content string) error {
	mkdirStep := ""
	if createParent {
		mkdirStep = `mkdir -p -- "$dir" && `
	}
	script := fmt.Sprintf(
		`target=%s
if [ -L "$target" ]; then
  resolved=$(realpath -- "$target" 2>/dev/null || readlink -f -- "$target" 2>/dev/null || true)
  if [ -n "$resolved" ]; then target="$resolved"; fi
fi
dir=$(dirname -- "$target")
%stmp=$(mktemp "$dir/.piui-tmp.XXXXXX") || exit 1
trap 'rm -f -- "$tmp"' EXIT
if [ -e "$target" ]; then
  chmod --reference="$target" "$tmp" 2>/dev/null || cp -p -- "$target" "$tmp" 2>/dev/null || true
else
  chmod 0644 "$tmp" 2>/dev/null || true
fi
cat > "$tmp" && mv -f -- "$tmp" "$target" && trap - EXIT`,
		shellQuotePath(remotePath),
		mkdirStep,
	)
	return sshWriteFile(ctx, sshHost, script, content)
}

func sshWriteFile(ctx context.Context, sshHost, script, content string) error {
	args := sshBashArgs(sshHost, false, script)
	cmd := exec.CommandContext(ctx, sshBinary, args...)
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

type remoteEditSpan struct {
	blockIdx int
	start    int
	end      int
	newText  string
}

// applyRemoteEdits matches every replacement block against the ORIGINAL file content
// (non-incrementally, rejecting duplicates and overlaps), matching sky-valley/pi/coding's edit contract.
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
	original := normalizeLF(raw)

	spans := make([]remoteEditSpan, 0, len(edits))
	for i, e := range edits {
		oldStr, _ := e["oldText"].(string)
		newStr, _ := e["newText"].(string)
		oldNorm := normalizeLF(oldStr)
		newNorm := normalizeLF(newStr)
		if oldNorm == "" {
			return "", 0, fmt.Errorf("edit block %d: oldText cannot be empty", i+1)
		}
		matches := strings.Count(original, oldNorm)
		if matches == 0 {
			return "", 0, fmt.Errorf("edit block %d: oldText not found in file", i+1)
		}
		if matches > 1 {
			return "", 0, fmt.Errorf("edit block %d: oldText matches %d locations in file; include more surrounding context", i+1, matches)
		}
		idx := strings.Index(original, oldNorm)
		spans = append(spans, remoteEditSpan{
			blockIdx: i + 1,
			start:    idx,
			end:      idx + len(oldNorm),
			newText:  newNorm,
		})
	}

	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end < spans[j].end
	})

	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return "", 0, fmt.Errorf("edit blocks %d and %d overlap in original file; merge nearby changes into one edit", spans[i-1].blockIdx, spans[i].blockIdx)
		}
	}

	var out strings.Builder
	cursor := 0
	for _, sp := range spans {
		out.WriteString(original[cursor:sp.start])
		out.WriteString(sp.newText)
		cursor = sp.end
	}
	out.WriteString(original[cursor:])

	updated := out.String()
	if useCRLF {
		updated = strings.ReplaceAll(updated, "\n", "\r\n")
	}
	return bom + updated, len(edits), nil
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
