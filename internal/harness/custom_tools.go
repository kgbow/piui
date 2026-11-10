package harness

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"

	"piui/internal/store"
)

// probeScript collects a concise, read-only snapshot of the active Local or SSH workspace.
const probeScript = `
echo "=== HOST & OS ==="
uname -srm 2>/dev/null || true
echo "User: $(whoami 2>/dev/null || id -un 2>/dev/null) | Host: $(hostname 2>/dev/null)"
echo "PWD:  $(pwd)"
echo ""
echo "=== UPTIME & DISK ==="
uptime 2>/dev/null || true
df -h . 2>/dev/null | head -n 2 || true
echo ""
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "=== GIT REPOSITORY ==="
  echo "Branch: $(git rev-parse --abbrev-ref HEAD 2>/dev/null)"
  git status -sb 2>/dev/null | head -n 20
  echo "--- Recent Commits ---"
  git log -n 3 --oneline 2>/dev/null || true
else
  echo "=== DIRECTORY SAMPLE ==="
  ls -la 2>/dev/null | head -n 20 || true
fi
`

// CreateWorkspaceProbeTool builds the private "workspace_probe" tool that inspects
// the active Local or Remote SSH workspace (OS, disk, Git status, directory snapshot) in one call.
func CreateWorkspaceProbeTool(router *EnvironmentRouter) agent.AgentTool {
	return agent.AgentTool{
		Name:        "workspace_probe",
		Label:       "workspace_probe",
		Description: "Collect a fast read-only diagnostic snapshot of the currently active workspace environment (Local or Remote SSH): OS/architecture, disk usage, Git branch/status, or top-level directory listing.",
		Parameters: ai.Object(
			ai.Opt("subpath", ai.String("Optional subdirectory relative to the workspace root to inspect")),
		),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			target := router.ConsumeToolTarget(id)
			subpath, _ := params["subpath"].(string)
			subpath = strings.TrimSpace(subpath)

			execCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()

			var out string
			var err error
			if target.IsRemote {
				remoteDir := target.WorkDir
				if subpath != "" {
					remoteDir = resolveRemotePath(target.WorkDir, subpath)
				}
				out, err = runRemoteReadOnlyScript(execCtx, target.SSHHost, remoteDir, probeScript)
			} else {
				localDir := store.ExpandLocalPath(target.WorkDir)
				if subpath != "" {
					localDir = resolveLocalSubpath(localDir, subpath)
				}
				out, err = runLocalReadOnlyScript(execCtx, localDir, probeScript)
			}

			if err != nil {
				return agent.AgentToolResult{}, fmt.Errorf("workspace_probe failed: %w", err)
			}
			header := fmt.Sprintf("[Workspace Target: %s | Remote=%v | WorkDir=%s]\n", target.Name, target.IsRemote, target.WorkDir)
			text := header + strings.TrimSpace(out)
			return agent.AgentToolResult{
				Content:           ai.ContentList{ai.TextContent{Text: text}},
				StructuredContent: text,
			}, nil
		},
	}
}

func runLocalReadOnlyScript(ctx context.Context, dir, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return "", fmt.Errorf("%v (%s)", err, errMsg)
		}
		return "", err
	}
	return stdout.String(), nil
}

func runRemoteReadOnlyScript(ctx context.Context, sshHost, remoteDir, script string) (string, error) {
	wrapped := script
	if remoteDir != "" && remoteDir != "~" {
		wrapped = fmt.Sprintf("cd -- %s && {\n%s\n}", shellQuotePath(remoteDir), script)
	}
	args := sshBashArgs(sshHost, false, wrapped)
	cmd := exec.CommandContext(ctx, sshBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return "", fmt.Errorf("%v (%s)", err, errMsg)
		}
		return "", err
	}
	return stdout.String(), nil
}

func resolveLocalSubpath(baseDir, sub string) string {
	sub = store.ExpandLocalPath(sub)
	if strings.HasPrefix(sub, "/") {
		return sub
	}
	return strings.TrimSuffix(baseDir, "/") + "/" + sub
}
