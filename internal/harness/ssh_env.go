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
	"strconv"
	"strings"
	"time"

	"github.com/sky-valley/pi/coding"
)

// sshMultiplexArgs returns the OpenSSH flags for non-interactive ControlMaster connection
// multiplexing. Using "%C" hashes (%l%h%p%r) so socket paths stay well within macOS/Linux
// sun_path length limits while keeping connections warm across consecutive tool calls.
func sshMultiplexArgs(sshHost string, remoteCommand ...string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=10m",
		"-o", "ControlPath=/tmp/piui-ssh-%C",
		sshHost,
	}
	args = append(args, remoteCommand...)
	return args
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

// WriteFile writes data to a file on the remote host, creating parent directories if needed.
func (e *SSHEnv) WriteFile(ctx context.Context, filePath string, data []byte) error {
	remotePath := e.resolve(filePath)
	dir := path.Dir(remotePath)
	script := fmt.Sprintf("mkdir -p -- %s && cat > %s", shellQuotePath(dir), shellQuotePath(remotePath))
	return sshWriteFile(ctx, e.SSHHost, script, string(data))
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

// Exec executes a shell command on the remote host over the multiplexed SSH channel.
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
		if k != "" {
			envExports.WriteString("export ")
			envExports.WriteString(k)
			envExports.WriteString("=")
			envExports.WriteString(shellQuote(v))
			envExports.WriteString("; ")
		}
	}

	remoteScript := envExports.String() + command
	if cwd != "" && cwd != "~" {
		remoteScript = fmt.Sprintf("cd -- %s && { %s; }", shellQuotePath(cwd), remoteScript)
	}

	args := sshMultiplexArgs(e.SSHHost, "bash", "-lc", remoteScript)
	cmd := exec.CommandContext(execCtx, "ssh", args...)
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

	err := cmd.Run()
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

// DiscoverRemoteSkills scans .pi/skills/*/SKILL.md in the remote workspace (and ~/.pi/agent/skills)
// with a short timeout so remote skill definitions are surfaced when connected via SSH.
func DiscoverRemoteSkills(ctx context.Context, sshHost, remoteWorkDir string) []coding.Skill {
	sshHost = strings.TrimSpace(sshHost)
	if sshHost == "" {
		return nil
	}
	execCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	script := `
for f in .pi/skills/*/SKILL.md "$HOME"/.pi/agent/skills/*/SKILL.md; do
  if [ -f "$f" ]; then
    echo "===PIUI_SKILL_FILE:$f==="
    head -n 40 "$f" 2>/dev/null || true
  fi
done
`
	out, err := runRemoteReadOnlyScript(execCtx, sshHost, remoteWorkDir, script)
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	return parseRemoteSkillOutput(sshHost, out)
}

func parseRemoteSkillOutput(sshHost, raw string) []coding.Skill {
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
			FilePath:    fmt.Sprintf("ssh://%s/%s", sshHost, strings.TrimPrefix(filePath, "/")),
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
	args := []string{"-O", "check", "-o", "ControlPath=/tmp/piui-ssh-%C", sshHost}
	cmd := exec.CommandContext(cctx, "ssh", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	return cmd.Run() == nil
}
