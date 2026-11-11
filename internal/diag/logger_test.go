package diag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingLoggerAndCrashCapture(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("PIUI_LOG_DIR", tmpDir)
	_ = Close()
	t.Cleanup(func() { _ = Close() })

	logPath := filepath.Join(tmpDir, "custom.log")
	logger, err := NewRotatingLogger(logPath, 240)
	if err != nil {
		t.Fatalf("NewRotatingLogger: %v", err)
	}
	for i := 0; i < 10; i++ {
		logger.Infof("iteration=%d payload=%s", i, strings.Repeat("x", 32))
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("expected rotated backup file %s.1 to exist: %v", logPath, err)
	}
	tail := TailLog(logPath, 1024)
	if !strings.Contains(tail, "iteration=9") {
		t.Fatalf("expected latest iteration in active log tail, got %q", tail)
	}

	// Verify CapturePanic records crash into DefaultLogPath()
	var caught any
	func() {
		defer CapturePanic("unit-test-worker", func(r any) {
			caught = r
		})
		panic("simulated subsystem panic")
	}()
	if caught == nil {
		t.Fatalf("expected CapturePanic callback to observe panic")
	}

	rep := FormatDiagnosticReport(SummaryInput{
		WorkspaceLabel: "piui",
		ModelLabel:     "claude-sonnet-4-5",
		MCPServers:     2,
		MCPTools:       5,
		LastError:      "connection timeout",
	})
	if !strings.Contains(rep, "simulated subsystem panic") || !strings.Contains(rep, "`2` servers configured (`5` tools active)") {
		t.Fatalf("unexpected diagnostic report:\n%s", rep)
	}
}
