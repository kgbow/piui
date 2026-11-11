package harness

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sky-valley/pi/coding"
)

// sshBinary is the OpenSSH client executable; package tests may override it with a local shim.
var sshBinary = "ssh"

var (
	sshSockDirOnce sync.Once
	sshSockDirPath string
)

// sshControlSocketPattern returns a user-private (0700) ControlPath template that stays
// strictly within Unix domain socket sun_path limits (104 bytes on macOS, 108 on Linux).
func sshControlSocketPattern() string {
	sshSockDirOnce.Do(func() {
		// Prefer ~/.pi/ssh if short enough; otherwise fall back to /tmp/piui-<uid> with 0700 perms.
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			candidate := filepath.Join(home, ".pi", "ssh")
			// "%C" expands to a 40-char SHA1 hex hash; "/s-" + 40 + NUL = 44 bytes.
			if len(candidate)+45 < 100 {
				if mkErr := os.MkdirAll(candidate, 0o700); mkErr == nil {
					_ = os.Chmod(candidate, 0o700)
					sshSockDirPath = candidate
					return
				}
			}
		}
		fallback := filepath.Join(os.TempDir(), fmt.Sprintf("piui-ssh-%d", os.Getuid()))
		_ = os.MkdirAll(fallback, 0o700)
		_ = os.Chmod(fallback, 0o700)
		sshSockDirPath = fallback
	})
	return filepath.Join(sshSockDirPath, "s-%C")
}

// sshMultiplexArgs returns the OpenSSH flags for non-interactive ControlMaster connection
// multiplexing inside a private 0700 socket directory. Using "%C" hashes (%l%h%p%r) keeps
// socket paths well within macOS/Linux sun_path limits while keeping connections warm.
func sshMultiplexArgs(sshHost string, remoteCommand ...string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=10m",
		"-o", "ControlPath=" + sshControlSocketPattern(),
		"--",
		sshHost,
	}
	args = append(args, remoteCommand...)
	return args
}

// sshRemoteBashCommand wraps a bash script as a single POSIX-quoted remote command string
// so OpenSSH's remote login shell passes the entire script verbatim as argv[2] to bash,
// preventing outer-shell word splitting, redirection interception, or premature $VAR expansion.
func sshRemoteBashCommand(loginShell bool, script string) string {
	if loginShell {
		return "bash -lc " + shellQuote(script)
	}
	return "bash -c " + shellQuote(script)
}

// sshBashArgs builds OpenSSH ControlMaster arguments that invoke bash on the remote host
// with the script safely quoted for the outer login shell.
func sshBashArgs(sshHost string, loginShell bool, script string) []string {
	return sshMultiplexArgs(sshHost, sshRemoteBashCommand(loginShell, script))
}

// wrapRemoteManagedProcessScript wraps a remote bash command so it runs in its own POSIX
// process group (`set -m`) with stdin=/dev/null while monitoring the outer SSH stdin pipe.
// If the local SSH client is aborted, times out, or disconnects, EOF on outer stdin triggers
// immediate SIGTERM/SIGKILL cleanup of the entire remote process group—preventing orphan
// remote processes while preserving exact stdout, stderr, and exit codes.
func wrapRemoteManagedProcessScript(userScript string) string {
	return fmt.Sprintf(`set -m
{
%s
} </dev/null &
__piui_pgid=$!
(
  exec >/dev/null 2>&1
  while IFS= read -r _; do :; done
  kill -TERM -- -"$__piui_pgid" 2>/dev/null || kill -TERM "$__piui_pgid" 2>/dev/null || true
  sleep 0.2
  kill -KILL -- -"$__piui_pgid" 2>/dev/null || kill -KILL "$__piui_pgid" 2>/dev/null || true
) &
__piui_watch=$!
wait "$__piui_pgid"
__piui_rc=$?
kill -KILL "$__piui_watch" 2>/dev/null || true
exit "$__piui_rc"`, userScript)
}

func isValidShellIdent(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

// SSHConnectionStatus describes the live health and environment details of an SSH workspace.
type SSHConnectionStatus struct {
	SSHHost          string
	Connected        bool
	Checking         bool
	LatencyMs        int64
	RemoteUser       string
	RemoteHost       string
	RemoteOS         string
	ResolvedWorkDir  string
	HasProjectSkills bool
	Error            string
	CheckedAt        time.Time
}

// RemoteWorkspaceBootstrap holds the probed metadata, context files (AGENTS.md / CLAUDE.md),
// and discovered SKILL.md definitions for a remote SSH workspace.
type RemoteWorkspaceBootstrap struct {
	Status       SSHConnectionStatus
	ContextFiles []coding.ContextFile
	Skills       []coding.Skill
}

// ProbeSSHWorkspace tests SSH connectivity, measures roundtrip latency, verifies/resolves
// remoteWorkDir, and checks whether untrusted project-local .pi/skills exist.
func ProbeSSHWorkspace(ctx context.Context, sshHost, remoteWorkDir string) SSHConnectionStatus {
	sshHost = strings.TrimSpace(sshHost)
	if sshHost == "" {
		return SSHConnectionStatus{
			Connected: false,
			Error:     "未配置 SSH 主机地址",
			CheckedAt: time.Now(),
		}
	}
	if strings.TrimSpace(remoteWorkDir) == "" {
		remoteWorkDir = "~"
	}

	pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	start := time.Now()
	script := fmt.Sprintf(`target=%s
if [ ! -d "$target" ]; then
  echo "ERR_NOT_DIR:$target" >&2
  exit 44
fi
cd -- "$target" || exit 44
echo "USER:$(whoami 2>/dev/null || id -un 2>/dev/null)"
echo "HOST:$(hostname 2>/dev/null)"
echo "OS:$(uname -srm 2>/dev/null)"
echo "PWD:$(pwd)"
has_ps=0
for f in .pi/skills/*/SKILL.md; do
  if [ -f "$f" ]; then has_ps=1; break; fi
done
echo "HAS_PROJECT_SKILLS:$has_ps"
`, shellQuotePath(remoteWorkDir))

	out, err := runRemoteReadOnlyScript(pctx, sshHost, "", script)
	lat := time.Since(start).Milliseconds()
	if lat < 1 {
		lat = 1
	}

	st := SSHConnectionStatus{
		SSHHost:   sshHost,
		LatencyMs: lat,
		CheckedAt: time.Now(),
	}
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "ERR_NOT_DIR:") {
			st.Error = fmt.Sprintf("远端目录不存在或不可访问: %s", remoteWorkDir)
		} else {
			st.Error = msg
		}
		return st
	}

	st.Connected = true
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "USER:"):
			st.RemoteUser = strings.TrimSpace(strings.TrimPrefix(line, "USER:"))
		case strings.HasPrefix(line, "HOST:"):
			st.RemoteHost = strings.TrimSpace(strings.TrimPrefix(line, "HOST:"))
		case strings.HasPrefix(line, "OS:"):
			st.RemoteOS = strings.TrimSpace(strings.TrimPrefix(line, "OS:"))
		case strings.HasPrefix(line, "PWD:"):
			st.ResolvedWorkDir = strings.TrimSpace(strings.TrimPrefix(line, "PWD:"))
		case strings.HasPrefix(line, "HAS_PROJECT_SKILLS:"):
			st.HasProjectSkills = strings.TrimSpace(strings.TrimPrefix(line, "HAS_PROJECT_SKILLS:")) == "1"
		}
	}
	if st.ResolvedWorkDir == "" {
		st.ResolvedWorkDir = remoteWorkDir
	}
	return st
}

// LoadRemoteWorkspaceBootstrap fetches remote host info, AGENTS.md / CLAUDE.md context files,
// and remote SKILL.md metadata in a single multiplexed SSH roundtrip.
// Project-local .pi/skills are included ONLY when trustProject is true.
func LoadRemoteWorkspaceBootstrap(ctx context.Context, sshHost, remoteWorkDir string, trustProject bool) RemoteWorkspaceBootstrap {
	sshHost = strings.TrimSpace(sshHost)
	if sshHost == "" {
		return RemoteWorkspaceBootstrap{
			Status: SSHConnectionStatus{Error: "未配置 SSH 主机地址", CheckedAt: time.Now()},
		}
	}
	if strings.TrimSpace(remoteWorkDir) == "" {
		remoteWorkDir = "~"
	}

	bctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	start := time.Now()
	trustFlag := "0"
	if trustProject {
		trustFlag = "1"
	}

	script := fmt.Sprintf(`target=%s
if [ ! -d "$target" ]; then
  echo "ERR_NOT_DIR:$target" >&2
  exit 44
fi
cd -- "$target" || exit 44
rpwd=$(pwd)
echo "===PIUI_META==="
echo "USER:$(whoami 2>/dev/null || id -un 2>/dev/null)"
echo "HOST:$(hostname 2>/dev/null)"
echo "OS:$(uname -srm 2>/dev/null)"
echo "PWD:$rpwd"
has_ps=0
for f in "$rpwd"/.pi/skills/*/SKILL.md; do
  if [ -f "$f" ]; then has_ps=1; break; fi
done
echo "HAS_PROJECT_SKILLS:$has_ps"

emit_ctx() {
  fp="$1"
  if [ -f "$fp" ]; then
    echo "===PIUI_CTX_FILE:$fp==="
    head -c 32768 "$fp" 2>/dev/null || true
    echo ""
  fi
}

if [ -n "$HOME" ]; then
  if [ -f "$HOME/.pi/agent/AGENTS.md" ]; then
    emit_ctx "$HOME/.pi/agent/AGENTS.md"
  elif [ -f "$HOME/.pi/agent/CLAUDE.md" ]; then
    emit_ctx "$HOME/.pi/agent/CLAUDE.md"
  fi
fi

if [ "%s" = "1" ]; then
  if [ -f "$rpwd/AGENTS.md" ]; then
    emit_ctx "$rpwd/AGENTS.md"
  elif [ -f "$rpwd/CLAUDE.md" ]; then
    emit_ctx "$rpwd/CLAUDE.md"
  fi
fi

emit_skill() {
  sf="$1"
  if [ -f "$sf" ]; then
    echo "===PIUI_SKILL_FILE:$sf==="
    head -n 60 "$sf" 2>/dev/null || true
    echo ""
  fi
}

if [ -n "$HOME" ]; then
  for sf in "$HOME"/.pi/agent/skills/*/SKILL.md "$HOME"/.agents/skills/*/SKILL.md; do
    emit_skill "$sf"
  done
fi

if [ "%s" = "1" ]; then
  for sf in "$rpwd"/.pi/skills/*/SKILL.md "$rpwd"/.agents/skills/*/SKILL.md; do
    emit_skill "$sf"
  done
fi
`, shellQuotePath(remoteWorkDir), trustFlag, trustFlag)

	out, err := runRemoteReadOnlyScript(bctx, sshHost, "", script)
	lat := time.Since(start).Milliseconds()
	if lat < 1 {
		lat = 1
	}

	res := RemoteWorkspaceBootstrap{
		Status: SSHConnectionStatus{
			SSHHost:   sshHost,
			LatencyMs: lat,
			CheckedAt: time.Now(),
		},
	}
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "ERR_NOT_DIR:") {
			res.Status.Error = fmt.Sprintf("远端目录不存在或不可访问: %s", remoteWorkDir)
		} else {
			res.Status.Error = msg
		}
		return res
	}

	res.Status.Connected = true
	parseRemoteBootstrapPayload(out, &res)
	if res.Status.ResolvedWorkDir == "" {
		res.Status.ResolvedWorkDir = remoteWorkDir
	}
	return res
}

func parseRemoteBootstrapPayload(raw string, out *RemoteWorkspaceBootstrap) {
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")

	// Extract metadata block up to first file marker.
	metaPart := normalized
	for _, marker := range []string{"===PIUI_CTX_FILE:", "===PIUI_SKILL_FILE:"} {
		if idx := strings.Index(metaPart, marker); idx >= 0 {
			metaPart = metaPart[:idx]
		}
	}
	for _, line := range strings.Split(metaPart, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "USER:"):
			out.Status.RemoteUser = strings.TrimSpace(strings.TrimPrefix(line, "USER:"))
		case strings.HasPrefix(line, "HOST:"):
			out.Status.RemoteHost = strings.TrimSpace(strings.TrimPrefix(line, "HOST:"))
		case strings.HasPrefix(line, "OS:"):
			out.Status.RemoteOS = strings.TrimSpace(strings.TrimPrefix(line, "OS:"))
		case strings.HasPrefix(line, "PWD:"):
			out.Status.ResolvedWorkDir = strings.TrimSpace(strings.TrimPrefix(line, "PWD:"))
		case strings.HasPrefix(line, "HAS_PROJECT_SKILLS:"):
			out.Status.HasProjectSkills = strings.TrimSpace(strings.TrimPrefix(line, "HAS_PROJECT_SKILLS:")) == "1"
		}
	}

	// Split out skill section vs context file section.
	ctxSection := normalized
	skillSection := ""
	if skIdx := strings.Index(normalized, "===PIUI_SKILL_FILE:"); skIdx >= 0 {
		ctxSection = normalized[:skIdx]
		skillSection = normalized[skIdx:]
	}

	const ctxMarker = "===PIUI_CTX_FILE:"
	if firstCtx := strings.Index(ctxSection, ctxMarker); firstCtx >= 0 {
		for _, chunk := range strings.Split(ctxSection[firstCtx:], ctxMarker) {
			chunk = strings.TrimSpace(chunk)
			if chunk == "" {
				continue
			}
			nl := strings.IndexByte(chunk, '\n')
			if nl < 0 {
				continue
			}
			fp := strings.TrimSuffix(strings.TrimSpace(chunk[:nl]), "===")
			content := strings.TrimSpace(chunk[nl+1:])
			if fp != "" && content != "" {
				out.ContextFiles = append(out.ContextFiles, coding.ContextFile{
					Path:    fp,
					Content: content,
				})
			}
		}
	}

	if skillSection != "" {
		out.Skills = parseRemoteSkillOutput(out.Status.SSHHost, skillSection)
	}
}

// ListRemoteDirectories lists direct subdirectories of dirPath on sshHost so the user
// can interactively browse and pick a remote project folder in the UI.
func ListRemoteDirectories(ctx context.Context, sshHost, dirPath string) (resolvedDir string, dirs []string, err error) {
	sshHost = strings.TrimSpace(sshHost)
	if sshHost == "" {
		return "", nil, fmt.Errorf("请先输入或选择 SSH 主机")
	}
	if strings.TrimSpace(dirPath) == "" {
		dirPath = "~"
	}

	lctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	script := fmt.Sprintf(`target=%s
cd -- "$target" 2>/dev/null || { echo "无法进入目录: $target" >&2; exit 44; }
pwd
for d in */ .*/; do
  case "$d" in
    "./"|"../"|".git/"|"*/"|".*/") continue ;;
  esac
  if [ -d "$d" ]; then
    printf '%%s\n' "${d%%/}"
  fi
done | head -n 200
`, shellQuotePath(dirPath))

	out, runErr := runRemoteReadOnlyScript(lctx, sshHost, "", script)
	if runErr != nil {
		return "", nil, runErr
	}
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(out, "\r\n", "\n")), "\n")
	if len(lines) == 0 {
		return dirPath, nil, nil
	}
	resolvedDir = strings.TrimSpace(lines[0])
	for _, item := range lines[1:] {
		item = strings.TrimSpace(item)
		if item != "" {
			dirs = append(dirs, item)
		}
	}
	return resolvedDir, dirs, nil
}

// ReconnectSSH closes any stale ControlMaster socket for sshHost and establishes a fresh probe.
func ReconnectSSH(ctx context.Context, sshHost, remoteWorkDir string) SSHConnectionStatus {
	sshHost = strings.TrimSpace(sshHost)
	if sshHost != "" {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		args := []string{"-O", "exit", "-o", "ControlPath=" + sshControlSocketPattern(), "--", sshHost}
		_ = exec.CommandContext(cctx, sshBinary, args...).Run()
		cancel()
	}
	return ProbeSSHWorkspace(ctx, sshHost, remoteWorkDir)
}

// SSHEnv implements coding.ExecutionEnv (coding.FileSystem + coding.Shell) over
// OpenSSH ControlMaster multiplexing.
type SSHEnv struct {
	SSHHost string
	WorkDir string
}

var _ coding.ExecutionEnv = (*SSHEnv)(nil)

// NewSSHEnv creates a multiplexed remote ExecutionEnv for the given SSH host and working directory.
func NewSSHEnv(sshHost, workDir string) *SSHEnv {
	if strings.TrimSpace(workDir) == "" {
		workDir = "~"
	}
	return &SSHEnv{
		SSHHost: strings.TrimSpace(sshHost),
		WorkDir: strings.TrimSpace(workDir),
	}
}

func (e *SSHEnv) resolve(p string) string {
	return resolveRemotePath(e.WorkDir, p)
}

// Cwd returns the remote working directory.
func (e *SSHEnv) Cwd() string {
	if e.WorkDir != "" {
		return e.WorkDir
	}
	return "~"
}

// AbsolutePath resolves a path against the remote Cwd.
func (e *SSHEnv) AbsolutePath(ctx context.Context, filePath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return e.resolve(filePath), nil
}

// JoinPath joins remote POSIX path segments.
func (e *SSHEnv) JoinPath(ctx context.Context, parts []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return path.Join(parts...), nil
}

// ReadBinaryFile reads a file as raw bytes from the remote host over the multiplexed SSH channel.
func (e *SSHEnv) ReadBinaryFile(ctx context.Context, filePath string) ([]byte, error) {
	return sshReadFile(ctx, e.SSHHost, e.resolve(filePath))
}

// ReadTextFile reads a UTF-8 text file from the remote host.
func (e *SSHEnv) ReadTextFile(ctx context.Context, filePath string) (string, error) {
	b, err := e.ReadBinaryFile(ctx, filePath)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type memTextLineReader struct {
	reader *bufio.Reader
	closed bool
}

func (r *memTextLineReader) ReadLine(ctx context.Context) (coding.TextLine, error) {
	if err := ctx.Err(); err != nil {
		return coding.TextLine{}, err
	}
	if r.closed {
		return coding.TextLine{}, errors.New("reader is closed")
	}
	text, err := r.reader.ReadString('\n')
	switch {
	case err == nil:
		return coding.TextLine{Text: strings.TrimSuffix(text, "\n"), Terminated: true}, nil
	case errors.Is(err, io.EOF) && text != "":
		return coding.TextLine{Text: text, Terminated: false}, nil
	case errors.Is(err, io.EOF):
		return coding.TextLine{}, io.EOF
	default:
		return coding.TextLine{}, err
	}
}

func (r *memTextLineReader) Close() error {
	r.closed = true
	return nil
}

// OpenTextLineReader opens a pull-based UTF-8 line reader over the remote file.
func (e *SSHEnv) OpenTextLineReader(ctx context.Context, filePath string) (coding.TextLineReader, error) {
	b, err := e.ReadBinaryFile(ctx, filePath)
	if err != nil {
		return nil, err
	}
	return &memTextLineReader{reader: bufio.NewReader(bytes.NewReader(b))}, nil
}

// ReadTextLines reads up to maxLines UTF-8 lines from the remote file.
func (e *SSHEnv) ReadTextLines(ctx context.Context, filePath string, maxLines int) ([]string, error) {
	r, err := e.OpenTextLineReader(ctx, filePath)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var lines []string
	for maxLines <= 0 || len(lines) < maxLines {
		line, err := r.ReadLine(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		lines = append(lines, line.Text)
	}
	return lines, nil
}

// WriteFile atomically writes data to a file on the remote host, creating parent directories if needed.
func (e *SSHEnv) WriteFile(ctx context.Context, filePath string, data []byte) error {
	remotePath := e.resolve(filePath)
	return sshWriteFileAtomic(ctx, e.SSHHost, remotePath, true, string(data))
}

// AppendFile appends data to a file on the remote host, creating parent directories if needed.
func (e *SSHEnv) AppendFile(ctx context.Context, filePath string, data []byte) error {
	remotePath := e.resolve(filePath)
	dir := path.Dir(remotePath)
	script := fmt.Sprintf("mkdir -p -- %s && cat >> %s", shellQuotePath(dir), shellQuotePath(remotePath))
	return sshWriteFile(ctx, e.SSHHost, script, string(data))
}

// RenameFile atomically renames a file on the remote host.
func (e *SSHEnv) RenameFile(ctx context.Context, sourcePath, destinationPath string) error {
	src := e.resolve(sourcePath)
	dst := e.resolve(destinationPath)
	script := fmt.Sprintf("mkdir -p -- %s && mv -f -- %s %s", shellQuotePath(path.Dir(dst)), shellQuotePath(src), shellQuotePath(dst))
	_, err := runRemoteReadOnlyScript(ctx, e.SSHHost, "", script)
	return err
}

// Stat queries file metadata (Kind, Size, Name, Path) on the remote host.
func (e *SSHEnv) Stat(ctx context.Context, filePath string) (coding.FileInfo, error) {
	remotePath := e.resolve(filePath)
	script := fmt.Sprintf(
		`if [ ! -e %[1]s ] && [ ! -L %[1]s ]; then echo "MISSING"; exit 0; fi; if [ -L %[1]s ]; then kind="symlink"; elif [ -d %[1]s ]; then kind="directory"; elif [ -f %[1]s ]; then kind="file"; else kind="other"; fi; size=$(wc -c < %[1]s 2>/dev/null || echo 0); echo "OK $kind $size"`,
		shellQuotePath(remotePath),
	)
	out, err := runRemoteReadOnlyScript(ctx, e.SSHHost, "", script)
	if err != nil {
		return coding.FileInfo{}, err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 || fields[0] == "MISSING" {
		return coding.FileInfo{}, os.ErrNotExist
	}
	kind := coding.FileKindFile
	if len(fields) > 1 {
		kind = coding.FileKind(fields[1])
	}
	var size int64
	if len(fields) > 2 {
		size, _ = strconv.ParseInt(fields[2], 10, 64)
	}
	return coding.FileInfo{
		Path: remotePath,
		Name: path.Base(remotePath),
		Kind: kind,
		Size: size,
	}, nil
}

// ListDir lists direct children of a directory on the remote host.
func (e *SSHEnv) ListDir(ctx context.Context, dirPath string) ([]coding.FileInfo, error) {
	remotePath := e.resolve(dirPath)
	script := fmt.Sprintf("ls -1Ap -- %s", shellQuotePath(remotePath))
	out, err := runRemoteReadOnlyScript(ctx, e.SSHHost, "", script)
	if err != nil {
		return nil, err
	}
	var items []coding.FileInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		kind := coding.FileKindFile
		if strings.HasSuffix(name, "/") {
			kind = coding.FileKindDirectory
			name = strings.TrimSuffix(name, "/")
		}
		items = append(items, coding.FileInfo{
			Path: path.Join(remotePath, name),
			Name: name,
			Kind: kind,
		})
	}
	return items, nil
}

// CanonicalPath resolves symlinks for an existing path on the remote host.
func (e *SSHEnv) CanonicalPath(ctx context.Context, filePath string) (string, error) {
	remotePath := e.resolve(filePath)
	script := fmt.Sprintf("realpath -- %s 2>/dev/null || readlink -f -- %s 2>/dev/null || echo %s",
		shellQuotePath(remotePath), shellQuotePath(remotePath), shellQuotePath(remotePath))
	out, err := runRemoteReadOnlyScript(ctx, e.SSHHost, "", script)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Exists reports whether a path exists on the remote host.
func (e *SSHEnv) Exists(ctx context.Context, filePath string) (bool, error) {
	_, err := e.Stat(ctx, filePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Exec executes a shell command on the remote host over the multiplexed SSH channel,
// automatically cleaning up remote child processes if ctx is cancelled or times out.
func (e *SSHEnv) Exec(ctx context.Context, command string, options *coding.ShellExecOptions) (coding.ShellResult, error) {
	var opts coding.ShellExecOptions
	if options != nil {
		opts = *options
	}

	execCtx := ctx
	var cancel context.CancelFunc
	if opts.TimeoutSeconds > 0 {
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(opts.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	cwd := strings.TrimSpace(opts.Cwd)
	if cwd == "" {
		cwd = e.WorkDir
	}

	var envExports strings.Builder
	for k, v := range opts.Env {
		if isValidShellIdent(k) {
			envExports.WriteString("export ")
			envExports.WriteString(k)
			envExports.WriteString("=")
			envExports.WriteString(shellQuote(v))
			envExports.WriteString("; ")
		}
	}

	remoteScript := envExports.String() + command
	if cwd != "" && cwd != "~" {
		remoteScript = fmt.Sprintf("cd -- %s && {\n%s\n}", shellQuotePath(cwd), remoteScript)
	}
	managedScript := wrapRemoteManagedProcessScript(remoteScript)

	stdinR, stdinW, pipeErr := os.Pipe()
	if pipeErr == nil {
		defer func() {
			_ = stdinW.Close()
			_ = stdinR.Close()
		}()
	}

	args := sshBashArgs(e.SSHHost, true, managedScript)
	cmd := exec.CommandContext(execCtx, sshBinary, args...)
	if pipeErr == nil {
		cmd.Stdin = stdinR
	}
	cmd.WaitDelay = 500 * time.Millisecond
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &chunkBufferWriter{
		buf: &stdoutBuf,
		onData: func(b []byte) {
			if opts.OnStdout != nil {
				opts.OnStdout(string(b))
			}
		},
	}
	cmd.Stderr = &chunkBufferWriter{
		buf: &stderrBuf,
		onData: func(b []byte) {
			if opts.OnStderr != nil {
				opts.OnStderr(string(b))
			}
		},
	}

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
	res := coding.ShellResult{
		Stdout: stdoutBuf.String(),
		Stderr: stderrBuf.String(),
	}
	if ctx.Err() != nil {
		res.ExitCode = -1
		return res, coding.ErrShellAborted
	}
	if execCtx.Err() == context.DeadlineExceeded && opts.TimeoutSeconds > 0 {
		res.ExitCode = -1
		return res, &coding.ShellTimeoutError{Seconds: float64(opts.TimeoutSeconds)}
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		res.ExitCode = -1
		return res, err
	}
	return res, nil
}

// Cleanup closes any idle multiplexed master connection if needed (no-op by default so ControlPersist keeps socket warm).
func (e *SSHEnv) Cleanup() error {
	return nil
}

type chunkBufferWriter struct {
	buf    *bytes.Buffer
	onData func([]byte)
}

func (w *chunkBufferWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		if w.buf != nil {
			w.buf.Write(p)
		}
		if w.onData != nil {
			cp := make([]byte, len(p))
			copy(cp, p)
			w.onData(cp)
		}
	}
	return len(p), nil
}

// DiscoverRemoteSkills scans SKILL.md definitions on the remote workspace (respecting trustProject).
// Returned Skill.FilePath values use real remote POSIX paths so the Agent's `read` tool can open them directly!
func DiscoverRemoteSkills(ctx context.Context, sshHost, remoteWorkDir string, trustProject ...bool) []coding.Skill {
	trusted := true
	if len(trustProject) > 0 {
		trusted = trustProject[0]
	}
	boot := LoadRemoteWorkspaceBootstrap(ctx, sshHost, remoteWorkDir, trusted)
	return boot.Skills
}

func parseRemoteSkillOutput(_ string, raw string) []coding.Skill {
	const marker = "===PIUI_SKILL_FILE:"
	chunks := strings.Split(raw, marker)
	var skills []coding.Skill
	seen := map[string]bool{}

	for _, chunk := range chunks {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		nl := strings.IndexByte(chunk, '\n')
		if nl == -1 {
			continue
		}
		header := strings.TrimSpace(chunk[:nl])
		filePath := strings.TrimSuffix(header, "===")
		body := chunk[nl+1:]

		name, desc := parseSimpleSkillFrontmatter(filePath, body)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		skills = append(skills, coding.Skill{
			Name:        name,
			Description: desc,
			FilePath:    filePath, // Real remote POSIX path so `read` tool works directly!
			BaseDir:     path.Dir(filePath),
		})
	}
	return skills
}

func parseSimpleSkillFrontmatter(filePath, content string) (name, desc string) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	inFrontmatter := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if i == 0 && trimmed == "---" {
			inFrontmatter = true
			continue
		}
		if inFrontmatter {
			if trimmed == "---" {
				break
			}
			if strings.HasPrefix(trimmed, "name:") {
				name = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "name:")), `"'`)
			} else if strings.HasPrefix(trimmed, "description:") {
				desc = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "description:")), `"'`)
			}
		}
	}
	if name == "" {
		dir := path.Base(path.Dir(filePath))
		if dir != "." && dir != "/" && dir != "skills" {
			name = dir
		}
	}
	if desc == "" && name != "" {
		desc = "Remote SSH Skill (" + filePath + ")"
	}
	return name, desc
}

// CheckMultiplexStatus runs a non-blocking `ssh -O check` to see if a warm ControlMaster socket exists.
func CheckMultiplexStatus(ctx context.Context, sshHost string) bool {
	if strings.TrimSpace(sshHost) == "" {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	args := []string{"-O", "check", "-o", "ControlPath=" + sshControlSocketPattern(), "--", sshHost}
	cmd := exec.CommandContext(cctx, sshBinary, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	return cmd.Run() == nil
}
