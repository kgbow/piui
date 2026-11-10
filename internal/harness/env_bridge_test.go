package harness

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/coding"

	"piui/internal/store"
)

func TestShellQuotePathAndResolveRemotePath(t *testing.T) {
	if got := resolveRemotePath("~", "cmd/main.go"); got != "~/cmd/main.go" {
		t.Fatalf("resolveRemotePath(~, cmd/main.go) = %q, want ~/cmd/main.go", got)
	}
	if got := resolveRemotePath("/srv/app", "config.yaml"); got != "/srv/app/config.yaml" {
		t.Fatalf("resolveRemotePath(/srv/app, config.yaml) = %q", got)
	}

	if got := shellQuotePath("~"); got != `"$HOME"` {
		t.Fatalf("shellQuotePath(~) = %q, want \"$HOME\"", got)
	}
	if got := shellQuotePath("~/my dir/a'b.txt"); got != `"$HOME/"'my dir/a'\''b.txt'` {
		t.Fatalf("shellQuotePath(~/my dir/a'b.txt) = %q", got)
	}
}

func TestFormatReadToolResultOffsetAndLimit(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5"
	res, err := formatReadToolResult(content, "test.txt", map[string]any{
		"offset": float64(2),
		"limit":  float64(2),
	})
	if err != nil {
		t.Fatalf("formatReadToolResult: %v", err)
	}
	str, _ := res.StructuredContent.(string)
	if !strings.HasPrefix(str, "line2\nline3") {
		t.Fatalf("unexpected slice content: %q", str)
	}
	if !strings.Contains(str, "Use offset=4 to continue") {
		t.Fatalf("expected continuation notice in %q", str)
	}
}

func TestApplyRemoteEditsPreservesCRLFAndChecksUniqueness(t *testing.T) {
	orig := "\uFEFFpackage main\r\n\r\nfunc hello() string {\r\n\treturn \"old\"\r\n}\r\n"
	updated, count, err := applyRemoteEdits(orig, map[string]any{
		"oldText": "func hello() string {\n\treturn \"old\"\n}",
		"newText": "func hello() string {\n\treturn \"new\"\n}",
	})
	if err != nil {
		t.Fatalf("applyRemoteEdits: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected count=1, got %d", count)
	}
	if !strings.HasPrefix(updated, "\uFEFF") {
		t.Fatalf("expected BOM to be preserved")
	}
	if !strings.Contains(updated, "return \"new\"\r\n}") {
		t.Fatalf("expected CRLF line endings to be preserved, got %q", updated)
	}

	// Duplicate oldText should fail rather than silently replacing first occurrence.
	dupContent := "foo = 1\nbar = 2\nfoo = 1\n"
	if _, _, err := applyRemoteEdits(dupContent, map[string]any{
		"oldText": "foo = 1",
		"newText": "foo = 2",
	}); err == nil || !strings.Contains(err.Error(), "matches 2 locations") {
		t.Fatalf("expected duplicate match error, got %v", err)
	}
}

func TestSSHMultiplexArgsAndRemoteSkillsParsing(t *testing.T) {
	args := sshMultiplexArgs("prod-gateway", "bash", "-c", "uptime")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ControlMaster=auto") || !strings.Contains(joined, "ControlPersist=10m") || !strings.Contains(joined, "ControlPath=/tmp/piui-ssh-%C") {
		t.Fatalf("expected SSH multiplexing flags in %q", joined)
	}

	rawSkillOutput := `===PIUI_SKILL_FILE:.pi/skills/deploy/SKILL.md===
---
name: prod-deploy
description: Rolling zero-downtime deployment on prod-gateway
---
# Deploy Skill
===PIUI_SKILL_FILE:/root/.pi/agent/skills/nginx-diag/SKILL.md===
# Nginx diagnostic skill without YAML frontmatter
`
	skills := parseRemoteSkillOutput("prod-gateway", rawSkillOutput)
	if len(skills) != 2 {
		t.Fatalf("expected 2 parsed remote skills, got %d (%+v)", len(skills), skills)
	}
	if skills[0].Name != "prod-deploy" || !strings.Contains(skills[0].Description, "Rolling zero-downtime") {
		t.Fatalf("unexpected first skill: %+v", skills[0])
	}
	if skills[1].Name != "nginx-diag" {
		t.Fatalf("expected fallback folder name 'nginx-diag', got %+v", skills[1])
	}
}


// installFakeSSH installs an executable shell shim that replicates OpenSSH's exact
// remote command joining semantics: all post-host argv items are joined with single
// spaces and passed to `/bin/sh -c "$joined"`.
func installFakeSSH(t *testing.T) string {
	t.Helper()
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "fake-ssh")
	script := `#!/bin/sh
check_mode=0
while [ $# -gt 0 ]; do
  case "$1" in
    --)
      shift
      break
      ;;
    -o)
      shift 2
      ;;
    -o*)
      shift
      ;;
    -O)
      if [ "$2" = "check" ]; then
        check_mode=1
      fi
      shift 2
      ;;
    -*)
      shift
      ;;
    *)
      break
      ;;
  esac
done

if [ $# -eq 0 ]; then
  echo "fake-ssh: missing host" >&2
  exit 255
fi
host="$1"
shift

if [ "$check_mode" = "1" ]; then
  echo "Master running (pid=42424)" >&2
  exit 0
fi

joined="$*"
exec /bin/sh -c "$joined"
`
	if err := os.WriteFile(shimPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake-ssh shim: %v", err)
	}
	prev := sshBinary
	sshBinary = shimPath
	t.Cleanup(func() {
		sshBinary = prev
	})
	return shimPath
}

func extractAgentToolText(res agent.AgentToolResult) string {
	for _, item := range res.Content {
		if tc, ok := item.(ai.TextContent); ok {
			return tc.Text
		}
	}
	if s, ok := res.StructuredContent.(string); ok {
		return s
	}
	return ""
}

func findToolByName(t *testing.T, tools []agent.AgentTool, name string) agent.AgentTool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q not found", name)
	return agent.AgentTool{}
}

func TestRemoteToolsEndToEndOverOpenSSHJoiningShim(t *testing.T) {
	installFakeSSH(t)

	if !CheckMultiplexStatus(context.Background(), "deploy@staging") {
		t.Fatal("expected CheckMultiplexStatus to return true against fake-ssh")
	}

	// Create a remote root containing spaces in path to stress-test outer + inner quoting.
	remoteBase := t.TempDir()
	remoteRoot := filepath.Join(remoteBase, "remote workspace")
	if err := os.MkdirAll(remoteRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll remoteRoot: %v", err)
	}

	// Initialize a git repository with .gitignore to verify remote find/grep honor ignore rules.
	initGit := exec.Command("git", "init")
	initGit.Dir = remoteRoot
	if out, err := initGit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, string(out))
	}
	if err := os.WriteFile(filepath.Join(remoteRoot, ".gitignore"), []byte("ignored_dir/\n*.gen.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	remoteTarget := store.WorkspaceTarget{
		ID:       "ssh-staging",
		Name:     "SSH: staging",
		IsRemote: true,
		WorkDir:  remoteRoot,
		SSHHost:  "deploy@staging",
	}

	var streamed bytes.Buffer
	router := NewEnvironmentRouter(remoteTarget, func(chunk string) {
		streamed.WriteString(chunk)
	})
	tools := router.BuildTools(t.TempDir())
	probeTool := CreateWorkspaceProbeTool(router)

	readTool := findToolByName(t, tools, "read")
	writeTool := findToolByName(t, tools, "write")
	editTool := findToolByName(t, tools, "edit")
	findTool := findToolByName(t, tools, "find")
	grepTool := findToolByName(t, tools, "grep")
	lsTool := findToolByName(t, tools, "ls")
	bashTool := findToolByName(t, tools, "bash")

	ctx := context.Background()

	// 1. Remote write & read with spaces, single quotes, and $HOME literal in filename + content.
	trickyRel := "nested dir/quote's $HOME.txt"
	trickyContent := "alpha $HOME\nbeta 'quoted'\ngamma\ndelta\n"
	if _, err := writeTool.Execute(ctx, "tc-w1", map[string]any{
		"path":    trickyRel,
		"content": trickyContent,
	}, nil); err != nil {
		t.Fatalf("remote write tricky file failed: %v", err)
	}

	readRes, err := readTool.Execute(ctx, "tc-r1", map[string]any{
		"path":   trickyRel,
		"offset": float64(1),
		"limit":  float64(2),
	}, nil)
	if err != nil {
		t.Fatalf("remote read tricky file failed: %v", err)
	}
	readText := extractAgentToolText(readRes)
	if !strings.Contains(readText, "alpha $HOME\nbeta 'quoted'") {
		t.Fatalf("unexpected remote read slice: %q", readText)
	}

	// Missing file -> ENOENT; directory -> EISDIR.
	if _, err := readTool.Execute(ctx, "tc-r-enoent", map[string]any{"path": "does-not-exist.txt"}, nil); err == nil || !strings.Contains(err.Error(), "ENOENT") {
		t.Fatalf("expected ENOENT for missing remote file, got %v", err)
	}
	if _, err := readTool.Execute(ctx, "tc-r-eisdir", map[string]any{"path": "nested dir"}, nil); err == nil || !strings.Contains(err.Error(), "EISDIR") {
		t.Fatalf("expected EISDIR for remote directory read, got %v", err)
	}

	// 2. Remote read on PNG binary file returns ImageContent with image/png MIME type.
	pngBytes := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	}
	if err := os.WriteFile(filepath.Join(remoteRoot, "icon.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	imgRes, err := readTool.Execute(ctx, "tc-r-png", map[string]any{"path": "icon.png"}, nil)
	if err != nil {
		t.Fatalf("remote PNG read failed: %v", err)
	}
	var gotImg *ai.ImageContent
	for _, item := range imgRes.Content {
		if ic, ok := item.(ai.ImageContent); ok {
			gotImg = &ic
		}
	}
	if gotImg == nil || gotImg.MimeType != "image/png" || gotImg.Data != base64.StdEncoding.EncodeToString(pngBytes) {
		t.Fatalf("expected ImageContent(image/png) matching pngBytes, got %+v", gotImg)
	}

	// 3. Remote large file (>10 MiB) server-side streaming window read.
	largePath := filepath.Join(remoteRoot, "huge.log")
	var largeBuf bytes.Buffer
	linePayload := strings.Repeat("x", 1000)
	for i := 1; i <= 11000; i++ {
		fmt.Fprintf(&largeBuf, "LINE_%05d %s\n", i, linePayload)
	}
	if err := os.WriteFile(largePath, largeBuf.Bytes(), 0o644); err != nil {
		t.Fatalf("write huge.log: %v", err)
	}
	hugeRes, err := readTool.Execute(ctx, "tc-r-huge", map[string]any{
		"path":   "huge.log",
		"offset": float64(5000),
		"limit":  float64(3),
	}, nil)
	if err != nil {
		t.Fatalf("remote huge.log windowed read failed: %v", err)
	}
	hugeText := extractAgentToolText(hugeRes)
	if !strings.Contains(hugeText, "LINE_05000") || !strings.Contains(hugeText, "LINE_05002") || strings.Contains(hugeText, "LINE_05003") {
		t.Fatalf("unexpected huge.log slice: %q", hugeText[:min(200, len(hugeText))])
	}
	if !strings.Contains(hugeText, "Use offset=5003 to continue") {
		t.Fatalf("expected continuation prompt in huge.log read: %q", hugeText)
	}
	_ = os.Remove(largePath)

	// 4. Remote edit: non-incremental multi-block edits[] + atomic permission preservation (0755).
	scriptPath := filepath.Join(remoteRoot, "deploy.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nstep1\nstep2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Note: block 1 inserts an extra "step2". Incremental matching would fail block 2 with duplicate matches,
	// whereas matching against the original file succeeds cleanly!
	if _, err := editTool.Execute(ctx, "tc-edit-multi", map[string]any{
		"path": "deploy.sh",
		"edits": []any{
			map[string]any{"oldText": "step1", "newText": "step1\nstep2"},
			map[string]any{"oldText": "step2", "newText": "step2_updated"},
		},
	}, nil); err != nil {
		t.Fatalf("remote multi-block edit failed: %v", err)
	}
	editedBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(editedBytes) != "#!/bin/sh\nstep1\nstep2\nstep2_updated\n" {
		t.Fatalf("unexpected edited deploy.sh content: %q", string(editedBytes))
	}
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("expected atomic remote edit to preserve 0755 permissions, got %o", info.Mode().Perm())
	}

	// Overlapping edits must be rejected.
	if _, err := editTool.Execute(ctx, "tc-edit-overlap", map[string]any{
		"path": "deploy.sh",
		"edits": []any{
			map[string]any{"oldText": "step1\nstep2", "newText": "A"},
			map[string]any{"oldText": "step2\nstep2_updated", "newText": "B"},
		},
	}, nil); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("expected overlapping edits error, got %v", err)
	}

	// 5. Remote find with doublestar path glob ("src/**/*.go") + .gitignore filtering.
	if err := os.MkdirAll(filepath.Join(remoteRoot, "src", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(remoteRoot, "ignored_dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteRoot, "src", "api", "handler.go"), []byte("package api\n\n// line before\nfunc HandlePing() string {\n\treturn \"pong\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteRoot, "src", "api", "handler.gen.go"), []byte("package api\nfunc HandleGenerated() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteRoot, "ignored_dir", "secret.go"), []byte("package secret\nfunc HandleSecret() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	findRes, err := findTool.Execute(ctx, "tc-find", map[string]any{
		"pattern": "src/**/*.go",
	}, nil)
	if err != nil {
		t.Fatalf("remote find failed: %v", err)
	}
	findText := extractAgentToolText(findRes)
	if !strings.Contains(findText, "src/api/handler.go") {
		t.Fatalf("expected src/api/handler.go in remote find output, got %q", findText)
	}
	if strings.Contains(findText, "handler.gen.go") || strings.Contains(findText, "secret.go") {
		t.Fatalf("expected gitignored files to be excluded from remote find, got %q", findText)
	}

	// 6. Remote grep with glob, context, ignoreCase, single-file target, and .gitignore filtering.
	grepRes, err := grepTool.Execute(ctx, "tc-grep-dir", map[string]any{
		"pattern":    "handleping",
		"ignoreCase": true,
		"glob":       "src/**/*.go",
		"context":    float64(1),
	}, nil)
	if err != nil {
		t.Fatalf("remote grep directory failed: %v", err)
	}
	grepText := extractAgentToolText(grepRes)
	if !strings.Contains(grepText, "handler.go:4: func HandlePing()") || !strings.Contains(grepText, "handler.go-3- // line before") {
		t.Fatalf("expected match + context lines in remote grep output, got %q", grepText)
	}
	if strings.Contains(grepText, "HandleGenerated") || strings.Contains(grepText, "HandleSecret") {
		t.Fatalf("expected gitignored matches excluded from remote grep, got %q", grepText)
	}

	// Single-file grep target (`path` pointing to a file rather than a directory)
	grepSingleRes, err := grepTool.Execute(ctx, "tc-grep-file", map[string]any{
		"pattern": "HandlePing",
		"path":    "src/api/handler.go",
	}, nil)
	if err != nil {
		t.Fatalf("remote grep single file failed: %v", err)
	}
	if !strings.Contains(extractAgentToolText(grepSingleRes), "func HandlePing()") {
		t.Fatalf("expected match from single-file remote grep, got %q", extractAgentToolText(grepSingleRes))
	}

	// 7. Remote ls, bash ($VAR expansion on remote shell + compound commands), workspace_probe, and SSHEnv.
	lsRes, err := lsTool.Execute(ctx, "tc-ls", map[string]any{}, nil)
	if err != nil || !strings.Contains(extractAgentToolText(lsRes), "deploy.sh") {
		t.Fatalf("expected deploy.sh in remote ls output, err=%v out=%q", err, extractAgentToolText(lsRes))
	}

	bashRes, err := bashTool.Execute(ctx, "tc-bash", map[string]any{
		"command": `REMOTE_VAR="ok_from_remote"; printf '%s|%s\n' "$REMOTE_VAR" "$(pwd)"`,
	}, nil)
	if err != nil {
		t.Fatalf("remote bash failed: %v", err)
	}
	if !strings.Contains(extractAgentToolText(bashRes), "ok_from_remote|"+remoteRoot) {
		t.Fatalf("expected remote bash expansion and remoteRoot cwd, got %q", extractAgentToolText(bashRes))
	}

	probeRes, err := probeTool.Execute(ctx, "tc-probe", map[string]any{}, nil)
	if err != nil || !strings.Contains(extractAgentToolText(probeRes), "HOST & OS") {
		t.Fatalf("expected remote workspace_probe output, err=%v out=%q", err, extractAgentToolText(probeRes))
	}

	// Verify SSHEnv WriteFile, ReadTextLines, and Exec with environment variables containing single quotes.
	sshEnv := router.ExecutionEnv()
	if err := sshEnv.WriteFile(ctx, "env_test.txt", []byte("initial")); err != nil {
		t.Fatalf("SSHEnv.WriteFile: %v", err)
	}
	gotEnvLines, err := sshEnv.ReadTextLines(ctx, "env_test.txt", 10)
	if err != nil || len(gotEnvLines) != 1 || gotEnvLines[0] != "initial" {
		t.Fatalf("SSHEnv.ReadTextLines got (%+v, %v)", gotEnvLines, err)
	}
	execRes, err := sshEnv.Exec(ctx, `printf '%s' "$CUSTOM_TOKEN"`, codingExecOpts("CUSTOM_TOKEN", "tok'123"))
	if err != nil || execRes.ExitCode != 0 || execRes.Stdout != "tok'123" {
		t.Fatalf("SSHEnv.Exec unexpected result: err=%v res=%+v", err, execRes)
	}
}

func codingExecOpts(k, v string) *coding.ShellExecOptions {
	return &coding.ShellExecOptions{
		Env: map[string]string{k: v},
	}
}
