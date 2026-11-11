package diag

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxLogBytes = 5 * 1024 * 1024 // 5 MB per rotating log file
)

// RotatingLogger is a thread-safe size-bounded file logger for production desktop diagnostics.
type RotatingLogger struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}

var (
	globalMu     sync.Mutex
	globalLogger *RotatingLogger
)

// DefaultLogPath returns ~/.config/piui/logs/piui.log, honoring PIUI_LOG_DIR when set.
func DefaultLogPath() string {
	if dir := strings.TrimSpace(os.Getenv("PIUI_LOG_DIR")); dir != "" {
		return filepath.Join(dir, "piui.log")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "piui.log")
	}
	return filepath.Join(home, ".config", "piui", "logs", "piui.log")
}

// NewRotatingLogger opens or creates a rotating log file at logPath.
func NewRotatingLogger(logPath string, maxBytes int64) (*RotatingLogger, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxLogBytes
	}
	dir := filepath.Dir(logPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	var sz int64
	if info, statErr := f.Stat(); statErr == nil {
		sz = info.Size()
	}
	return &RotatingLogger{
		path:     logPath,
		maxBytes: maxBytes,
		file:     f,
		size:     sz,
	}, nil
}

// Init initializes the process-wide diagnostic logger. Safe to call multiple times.
func Init() error {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalLogger != nil {
		return nil
	}
	l, err := NewRotatingLogger(DefaultLogPath(), defaultMaxLogBytes)
	if err != nil {
		return err
	}
	globalLogger = l
	l.writef("INFO", "PiUI started (go=%s os=%s/%s pid=%d)", runtime.Version(), runtime.GOOS, runtime.GOARCH, os.Getpid())
	return nil
}

// Close flushes and closes the global diagnostic logger.
func Close() error {
	globalMu.Lock()
	l := globalLogger
	globalLogger = nil
	globalMu.Unlock()
	if l != nil {
		return l.Close()
	}
	return nil
}

// LogPath returns the active log file path.
func LogPath() string {
	globalMu.Lock()
	l := globalLogger
	globalMu.Unlock()
	if l != nil {
		return l.Path()
	}
	return DefaultLogPath()
}

// Infof writes an informational diagnostic log entry.
func Infof(format string, args ...any) {
	globalMu.Lock()
	l := globalLogger
	globalMu.Unlock()
	if l != nil {
		l.Infof(format, args...)
	}
}

// Warnf writes a warning diagnostic log entry.
func Warnf(format string, args ...any) {
	globalMu.Lock()
	l := globalLogger
	globalMu.Unlock()
	if l != nil {
		l.Warnf(format, args...)
	}
}

// Errorf writes an error diagnostic log entry.
func Errorf(format string, args ...any) {
	globalMu.Lock()
	l := globalLogger
	globalMu.Unlock()
	if l != nil {
		l.Errorf(format, args...)
	}
}

// RecordCrash writes a panic value and stack trace to the diagnostic log file (creating it if needed).
func RecordCrash(where string, recovered any, stack []byte) {
	if recovered == nil {
		return
	}
	_ = Init()
	globalMu.Lock()
	l := globalLogger
	globalMu.Unlock()
	msg := fmt.Sprintf("PANIC in %s: %v\n%s", where, recovered, string(stack))
	if l != nil {
		l.writef("CRASH", "%s", msg)
		_ = l.Sync()
	}
	_, _ = fmt.Fprintf(os.Stderr, "[PiUI CRASH] %s\n", msg)
}

// CapturePanic recovers an active panic if any, logs the full stack trace, and invokes optional onPanic.
func CapturePanic(where string, onPanic func(recovered any)) {
	if r := recover(); r != nil {
		RecordCrash(where, r, debug.Stack())
		if onPanic != nil {
			onPanic(r)
		}
	}
}

// Path returns the file path of this logger.
func (l *RotatingLogger) Path() string {
	return l.path
}

// Infof writes an INFO line to this logger.
func (l *RotatingLogger) Infof(format string, args ...any) {
	l.writef("INFO", format, args...)
}

// Warnf writes a WARN line to this logger.
func (l *RotatingLogger) Warnf(format string, args ...any) {
	l.writef("WARN", format, args...)
}

// Errorf writes an ERROR line to this logger.
func (l *RotatingLogger) Errorf(format string, args ...any) {
	l.writef("ERROR", format, args...)
}

func (l *RotatingLogger) writef(level, format string, args ...any) {
	msg := strings.TrimRight(fmt.Sprintf(format, args...), "\r\n")
	line := fmt.Sprintf("%s [%s] %s\n", time.Now().Format("2006-01-02T15:04:05.000Z07:00"), level, msg)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	if l.size+int64(len(line)) > l.maxBytes {
		l.rotateLocked()
	}
	if l.file == nil {
		return
	}
	n, err := l.file.WriteString(line)
	if err == nil {
		l.size += int64(n)
	}
}

func (l *RotatingLogger) rotateLocked() {
	_ = l.file.Sync()
	_ = l.file.Close()
	l.file = nil

	backup := l.path + ".1"
	_ = os.Remove(backup)
	_ = os.Rename(l.path, backup)

	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	l.file = f
	l.size = 0
}

// Sync flushes buffered OS file writes.
func (l *RotatingLogger) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return l.file.Sync()
	}
	return nil
}

// Close flushes and closes the underlying log file.
func (l *RotatingLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	_ = l.file.Sync()
	err := l.file.Close()
	l.file = nil
	return err
}

// TailLog reads up to the last maxBytes bytes of logPath (or DefaultLogPath when empty).
func TailLog(logPath string, maxBytes int) string {
	if strings.TrimSpace(logPath) == "" {
		logPath = LogPath()
	}
	if maxBytes <= 0 {
		maxBytes = 4096
	}
	f, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}
	offset := int64(0)
	if info.Size() > int64(maxBytes) {
		offset = info.Size() - int64(maxBytes)
	}
	buf := make([]byte, info.Size()-offset)
	n, _ := f.ReadAt(buf, offset)
	s := string(buf[:n])
	if offset > 0 {
		if nl := strings.IndexByte(s, '\n'); nl != -1 && nl+1 < len(s) {
			s = s[nl+1:]
		}
	}
	return strings.TrimSpace(s)
}

// SummaryInput holds runtime state included in a shareable diagnostic report.
type SummaryInput struct {
	WorkspaceLabel string
	IsRemote       bool
	SSHStatus      string
	ModelLabel     string
	MCPServers     int
	MCPTools       int
	SessionID      string
	LastError      string
	ConfigPath     string
}

// FormatDiagnosticReport produces a sanitized Markdown diagnostic summary suitable for issue reporting.
func FormatDiagnosticReport(in SummaryInput) string {
	var b strings.Builder
	b.WriteString("# PiUI Runtime Diagnostic Report\n\n")
	fmt.Fprintf(&b, "- **Generated At**: `%s`\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Runtime**: `%s` (`%s/%s`)\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if in.WorkspaceLabel != "" {
		mode := "Local"
		if in.IsRemote {
			mode = "Remote SSH"
		}
		fmt.Fprintf(&b, "- **Active Workspace**: `%s` (%s)\n", in.WorkspaceLabel, mode)
	}
	if in.SSHStatus != "" {
		fmt.Fprintf(&b, "- **SSH Health**: `%s`\n", in.SSHStatus)
	}
	if in.ModelLabel != "" {
		fmt.Fprintf(&b, "- **Active Model**: `%s`\n", in.ModelLabel)
	}
	fmt.Fprintf(&b, "- **MCP Integration**: `%d` servers configured (`%d` tools active)\n", in.MCPServers, in.MCPTools)
	if in.SessionID != "" {
		fmt.Fprintf(&b, "- **Active Session ID**: `%s`\n", in.SessionID)
	}
	if in.ConfigPath != "" {
		fmt.Fprintf(&b, "- **Config File**: `%s`\n", in.ConfigPath)
	}
	fmt.Fprintf(&b, "- **Log File**: `%s`\n", LogPath())
	if strings.TrimSpace(in.LastError) != "" {
		fmt.Fprintf(&b, "\n## Last Recorded Error\n\n```\n%s\n```\n", strings.TrimSpace(in.LastError))
	}
	if tail := TailLog("", 3200); tail != "" {
		fmt.Fprintf(&b, "\n## Recent Log Tail\n\n```\n%s\n```\n", tail)
	}
	return b.String()
}
