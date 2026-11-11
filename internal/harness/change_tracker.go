package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"piui/internal/store"
)

const (
	maxTrackedMutationBytes = 2 * 1024 * 1024 // 2 MiB snapshot cap
	maxDiffLinesMatrix      = 600
	maxRenderedDiffRows     = 140
)

// DiffLine represents one line in a unified diff view.
type DiffLine struct {
	Kind    byte // '@', ' ', '+', '-'
	OldLine int
	NewLine int
	Text    string
}

// FileMutationSnapshot stores the pre- and post-mutation state of a single edit/write tool call,
// enabling structured unified diff rendering and one-click rollback across Local and SSH workspaces.
type FileMutationSnapshot struct {
	ToolCallID    string
	ToolName      string
	Target        store.WorkspaceTarget
	ResolvedPath  string
	DisplayPath   string
	ExistedBefore bool
	BeforeBytes   []byte
	AfterBytes    []byte
	AddedLines    int
	RemovedLines  int
	DiffLines     []DiffLine
	Reverted      bool
	CapturedAt    time.Time
}

// ChangeTracker records file mutations performed by edit/write tool calls in the active session.
type ChangeTracker struct {
	mu        sync.RWMutex
	snapshots map[string]*FileMutationSnapshot
}

// NewChangeTracker creates an initialized ChangeTracker.
func NewChangeTracker() *ChangeTracker {
	return &ChangeTracker{
		snapshots: make(map[string]*FileMutationSnapshot),
	}
}

// Clear discards all stored snapshots (e.g. when resetting or switching workspaces).
func (ct *ChangeTracker) Clear() {
	if ct == nil {
		return
	}
	ct.mu.Lock()
	ct.snapshots = make(map[string]*FileMutationSnapshot)
	ct.mu.Unlock()
}

// Get returns a copy of the mutation snapshot for toolCallID if tracked.
func (ct *ChangeTracker) Get(toolCallID string) (*FileMutationSnapshot, bool) {
	if ct == nil || toolCallID == "" {
		return nil, false
	}
	ct.mu.RLock()
	snap, ok := ct.snapshots[toolCallID]
	ct.mu.RUnlock()
	if !ok || snap == nil {
		return nil, false
	}
	cp := *snap
	return &cp, true
}

// Record stores a completed FileMutationSnapshot.
func (ct *ChangeTracker) Record(snap *FileMutationSnapshot) {
	if ct == nil || snap == nil || snap.ToolCallID == "" {
		return
	}
	ct.mu.Lock()
	ct.snapshots[snap.ToolCallID] = snap
	ct.mu.Unlock()
}

// CaptureFileState reads the current existence and bytes of path on target (Local or SSH) if within size cap.
func CaptureFileState(ctx context.Context, target store.WorkspaceTarget, fallbackCwd, rawPath string) (resolvedPath, displayPath string, existed bool, data []byte, ok bool) {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return "", "", false, nil, false
	}
	displayPath = rawPath

	if target.IsRemote {
		resolvedPath = resolveRemotePath(target.WorkDir, rawPath)
		readCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		checkScript := fmt.Sprintf(`
if [ ! -e %s ]; then
  printf '__PIUI_MISSING__'
  exit 0
fi
if [ ! -f %s ]; then
  exit 2
fi
sz=$(wc -c < %s 2>/dev/null | tr -d '[:space:]')
if [ -n "$sz" ] && [ "$sz" -gt %d ]; then
  exit 3
fi
`, shellQuotePath(resolvedPath), shellQuotePath(resolvedPath), shellQuotePath(resolvedPath), maxTrackedMutationBytes)
		out, err := exec.CommandContext(readCtx, sshBinary, sshBashArgs(target.SSHHost, false, checkScript)...).Output()
		if err != nil {
			return resolvedPath, displayPath, false, nil, false
		}
		if strings.TrimSpace(string(out)) == "__PIUI_MISSING__" {
			return resolvedPath, displayPath, false, nil, true
		}
		raw, err := sshReadFile(readCtx, target.SSHHost, resolvedPath)
		if err != nil || len(raw) > maxTrackedMutationBytes {
			return resolvedPath, displayPath, false, nil, false
		}
		return resolvedPath, displayPath, true, raw, true
	}

	baseDir := fallbackCwd
	if target.WorkDir != "" {
		baseDir = store.ExpandLocalPath(target.WorkDir)
	}
	if filepath.IsAbs(rawPath) || strings.HasPrefix(rawPath, "~") {
		resolvedPath = store.ExpandLocalPath(rawPath)
	} else {
		resolvedPath = filepath.Join(baseDir, filepath.FromSlash(rawPath))
	}

	info, err := os.Stat(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return resolvedPath, displayPath, false, nil, true
		}
		return resolvedPath, displayPath, false, nil, false
	}
	if info.IsDir() || info.Size() > maxTrackedMutationBytes {
		return resolvedPath, displayPath, false, nil, false
	}
	raw, err := os.ReadFile(resolvedPath)
	if err != nil {
		return resolvedPath, displayPath, false, nil, false
	}
	return resolvedPath, displayPath, true, raw, true
}

// BuildMutationSnapshot constructs a FileMutationSnapshot with unified diff hunks and +added/-removed stats.
func BuildMutationSnapshot(
	toolCallID, toolName string,
	target store.WorkspaceTarget,
	resolvedPath, displayPath string,
	existedBefore bool,
	beforeBytes, afterBytes []byte,
) *FileMutationSnapshot {
	snap := &FileMutationSnapshot{
		ToolCallID:    toolCallID,
		ToolName:      toolName,
		Target:        target,
		ResolvedPath:  resolvedPath,
		DisplayPath:   displayPath,
		ExistedBefore: existedBefore,
		BeforeBytes:   beforeBytes,
		AfterBytes:    afterBytes,
		CapturedAt:    time.Now(),
	}

	if isLikelyUTF8Text(beforeBytes) && isLikelyUTF8Text(afterBytes) {
		diffLines, added, removed := ComputeUnifiedDiff(string(beforeBytes), string(afterBytes), 3)
		snap.DiffLines = diffLines
		snap.AddedLines = added
		snap.RemovedLines = removed
	}
	return snap
}

// RevertMutation restores the target file to its pre-mutation state (or deletes it if newly created).
func (ct *ChangeTracker) RevertMutation(ctx context.Context, toolCallID string) (*FileMutationSnapshot, error) {
	if ct == nil {
		return nil, fmt.Errorf("变更追踪器未初始化")
	}
	ct.mu.RLock()
	snap, ok := ct.snapshots[toolCallID]
	ct.mu.RUnlock()
	if !ok || snap == nil {
		return nil, fmt.Errorf("未找到该工具调用的文件快照（可能仅在当前会话内生成的修改支持一键撤销）")
	}
	if snap.Reverted {
		return nil, fmt.Errorf("该文件修改已撤销")
	}

	revCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if snap.Target.IsRemote {
		if snap.ExistedBefore {
			if err := sshWriteFileAtomic(revCtx, snap.Target.SSHHost, snap.ResolvedPath, true, string(snap.BeforeBytes)); err != nil {
				return nil, fmt.Errorf("远程回滚写入失败: %w", err)
			}
		} else {
			rmScript := fmt.Sprintf("rm -f -- %s", shellQuotePath(snap.ResolvedPath))
			if out, err := exec.CommandContext(revCtx, sshBinary, sshBashArgs(snap.Target.SSHHost, false, rmScript)...).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("远程删除新建文件失败: %v (%s)", err, strings.TrimSpace(string(out)))
			}
		}
	} else {
		if snap.ExistedBefore {
			if err := os.MkdirAll(filepath.Dir(snap.ResolvedPath), 0o755); err != nil {
				return nil, fmt.Errorf("创建父目录失败: %w", err)
			}
			if err := os.WriteFile(snap.ResolvedPath, snap.BeforeBytes, 0o644); err != nil {
				return nil, fmt.Errorf("回滚本地文件失败: %w", err)
			}
		} else {
			if err := os.Remove(snap.ResolvedPath); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("删除新建文件失败: %w", err)
			}
		}
	}

	ct.mu.Lock()
	snap.Reverted = true
	cp := *snap
	ct.mu.Unlock()
	return &cp, nil
}

// ComputeUnifiedDiff computes line-numbered unified diff hunks and total added/removed line counts.
func ComputeUnifiedDiff(before, after string, contextLines int) (lines []DiffLine, added, removed int) {
	if contextLines < 0 {
		contextLines = 3
	}
	oldLines := splitDiffTextLines(before)
	newLines := splitDiffTextLines(after)

	// Trim identical prefix & suffix first so edits inside large files run in microseconds!
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	if prefix == len(oldLines) && prefix == len(newLines) {
		return nil, 0, 0
	}

	oldEnd := len(oldLines)
	newEnd := len(newLines)
	for oldEnd > prefix && newEnd > prefix && oldLines[oldEnd-1] == newLines[newEnd-1] {
		oldEnd--
		newEnd--
	}

	midOld := oldLines[prefix:oldEnd]
	midNew := newLines[prefix:newEnd]

	type rawEdit struct {
		kind    byte // ' ', '-', '+'
		oldLine int
		newLine int
		text    string
	}

	var midOps []rawEdit
	if len(midOld) <= maxDiffLinesMatrix && len(midNew) <= maxDiffLinesMatrix {
		// Exact LCS dynamic programming on the changed middle window.
		m, n := len(midOld), len(midNew)
		dp := make([][]uint16, m+1)
		for i := range dp {
			dp[i] = make([]uint16, n+1)
		}
		for i := m - 1; i >= 0; i-- {
			for j := n - 1; j >= 0; j-- {
				if midOld[i] == midNew[j] {
					dp[i][j] = dp[i+1][j+1] + 1
				} else if dp[i+1][j] >= dp[i][j+1] {
					dp[i][j] = dp[i+1][j]
				} else {
					dp[i][j] = dp[i][j+1]
				}
			}
		}
		i, j := 0, 0
		for i < m || j < n {
			switch {
			case i < m && j < n && midOld[i] == midNew[j]:
				midOps = append(midOps, rawEdit{kind: ' ', oldLine: prefix + i + 1, newLine: prefix + j + 1, text: midOld[i]})
				i++
				j++
			case j < n && (i == m || dp[i][j+1] >= dp[i+1][j]):
				midOps = append(midOps, rawEdit{kind: '+', newLine: prefix + j + 1, text: midNew[j]})
				added++
				j++
			default:
				midOps = append(midOps, rawEdit{kind: '-', oldLine: prefix + i + 1, text: midOld[i]})
				removed++
				i++
			}
		}
	} else {
		// Large replacement block fallback: linear delete + insert without O(N*M) allocation.
		for i, s := range midOld {
			midOps = append(midOps, rawEdit{kind: '-', oldLine: prefix + i + 1, text: s})
			removed++
		}
		for j, s := range midNew {
			midOps = append(midOps, rawEdit{kind: '+', newLine: prefix + j + 1, text: s})
			added++
		}
	}

	// Stitch up to contextLines from prefix and suffix around midOps, then group into hunks.
	preStart := max(0, prefix-contextLines)
	sufEnd := min(len(oldLines), oldEnd+contextLines)

	all := make([]rawEdit, 0, (prefix-preStart)+len(midOps)+(sufEnd-oldEnd))
	for k := preStart; k < prefix; k++ {
		all = append(all, rawEdit{kind: ' ', oldLine: k + 1, newLine: k + 1, text: oldLines[k]})
	}
	all = append(all, midOps...)
	newSuffixOffset := newEnd - oldEnd
	for k := oldEnd; k < sufEnd; k++ {
		all = append(all, rawEdit{kind: ' ', oldLine: k + 1, newLine: k + newSuffixOffset + 1, text: oldLines[k]})
	}

	// Mark indices within contextLines of any '+'/'-' op.
	keep := make([]bool, len(all))
	for idx, op := range all {
		if op.kind == '+' || op.kind == '-' {
			lo := max(0, idx-contextLines)
			hi := min(len(all)-1, idx+contextLines)
			for c := lo; c <= hi; c++ {
				keep[c] = true
			}
		}
	}

	idx := 0
	for idx < len(all) {
		if !keep[idx] {
			idx++
			continue
		}
		hunkStart := idx
		for idx < len(all) && keep[idx] {
			idx++
		}
		hunk := all[hunkStart:idx]

		oldStart, oldCnt := 0, 0
		newStart, newCnt := 0, 0
		for _, op := range hunk {
			if op.oldLine > 0 {
				if oldStart == 0 {
					oldStart = op.oldLine
				}
				oldCnt++
			}
			if op.newLine > 0 {
				if newStart == 0 {
					newStart = op.newLine
				}
				newCnt++
			}
		}
		if len(lines) >= maxRenderedDiffRows {
			lines = append(lines, DiffLine{
				Kind: '@',
				Text: fmt.Sprintf("... 其余变更已折叠（总计 +%d / -%d 行）...", added, removed),
			})
			break
		}
		lines = append(lines, DiffLine{
			Kind: '@',
			Text: fmt.Sprintf("@@ -%d,%d +%d,%d @@", oldStart, oldCnt, newStart, newCnt),
		})
		for _, op := range hunk {
			if len(lines) >= maxRenderedDiffRows {
				lines = append(lines, DiffLine{
					Kind: '@',
					Text: fmt.Sprintf("... 其余变更已折叠（总计 +%d / -%d 行）...", added, removed),
				})
				return lines, added, removed
			}
			lines = append(lines, DiffLine{
				Kind:    op.kind,
				OldLine: op.oldLine,
				NewLine: op.newLine,
				Text:    op.text,
			})
		}
	}

	return lines, added, removed
}

func splitDiffTextLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
