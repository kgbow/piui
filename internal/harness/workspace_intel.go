package harness

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sky-valley/pi/ai"

	"piui/internal/store"
)

const (
	defaultMaxIndexedFiles     = 4000
	maxReferencedFilesPerTurn  = 5
	maxReferencedFileLines     = 250
	maxReferencedFileBytes     = 20 * 1024
	maxReferencedImageBytes    = 5 * 1024 * 1024
	attachedContextMarkerStart = "[Attached Context:"
)

var ignoredIndexDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".piui":        true,
	"dist":         true,
	"build":        true,
	"target":       true,
	".next":        true,
	".nuxt":        true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".idea":        true,
	".vscode":      true,
}

// GitFileChange represents a single modified, staged, deleted, or untracked file in Git.
type GitFileChange struct {
	Path    string
	Status  string // "M", "A", "D", "R", "?"
	Staged  bool
	Added   int
	Removed int
}

// GitWorkspaceStatus captures a lightweight snapshot of the active workspace's Git repository state.
type GitWorkspaceStatus struct {
	IsRepo         bool
	Branch         string
	Ahead          int
	Behind         int
	Changes        []GitFileChange
	ModifiedCount  int
	UntrackedCount int
	TotalAdded     int
	TotalRemoved   int
	CheckedAt      time.Time
	Error          string
}

// DirtyCount returns the total number of changed and untracked files.
func (s GitWorkspaceStatus) DirtyCount() int {
	return len(s.Changes)
}

// ListWorkspaceFiles indexes relative file paths for either a Local or Remote SSH workspace.
func ListWorkspaceFiles(ctx context.Context, target store.WorkspaceTarget, maxFiles int) ([]string, error) {
	if maxFiles <= 0 {
		maxFiles = defaultMaxIndexedFiles
	}
	if target.IsRemote {
		return listRemoteWorkspaceFiles(ctx, target.SSHHost, target.WorkDir, maxFiles)
	}
	return listLocalWorkspaceFiles(ctx, store.ExpandLocalPath(target.WorkDir), maxFiles)
}

func listLocalWorkspaceFiles(ctx context.Context, workDir string, maxFiles int) ([]string, error) {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return nil, fmt.Errorf("empty workspace directory")
	}
	info, err := os.Stat(workDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", workDir)
	}

	// Fast path: git ls-files respects .gitignore and runs in single-digit milliseconds.
	gitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(gitCtx, "git", "-C", workDir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if out, err := cmd.Output(); err == nil && len(out) > 0 {
		rawParts := bytes.Split(out, []byte{0})
		files := make([]string, 0, min(len(rawParts), maxFiles))
		seen := make(map[string]struct{}, min(len(rawParts), maxFiles))
		for _, p := range rawParts {
			rel := filepath.ToSlash(strings.TrimSpace(string(p)))
			if rel == "" || strings.HasPrefix(rel, ".git/") {
				continue
			}
			if _, dup := seen[rel]; dup {
				continue
			}
			seen[rel] = struct{}{}
			files = append(files, rel)
			if len(files) >= maxFiles {
				break
			}
		}
		if len(files) > 0 {
			sort.Strings(files)
			return files, nil
		}
	}

	// Fallback: pure Go directory walk skipping heavy ignored directories.
	var files []string
	_ = filepath.WalkDir(workDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p == workDir {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if ignoredIndexDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		if name == ".DS_Store" {
			return nil
		}
		rel, err := filepath.Rel(workDir, p)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		if len(files) >= maxFiles {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(files)
	return files, nil
}

func listRemoteWorkspaceFiles(ctx context.Context, sshHost, remoteWorkDir string, maxFiles int) ([]string, error) {
	sshHost = strings.TrimSpace(sshHost)
	if sshHost == "" {
		return nil, fmt.Errorf("empty SSH host")
	}
	wd := strings.TrimSpace(remoteWorkDir)
	if wd == "" {
		wd = "~"
	}

	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	script := fmt.Sprintf(`
__wd=%s
if [ "$__wd" = "~" ]; then
  __wd="$HOME"
elif [ "${__wd#\~/}" != "$__wd" ]; then
  __wd="$HOME/${__wd#\~/}"
fi
cd "$__wd" 2>/dev/null || exit 1
if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git ls-files --cached --others --exclude-standard 2>/dev/null | head -n %d
elif command -v rg >/dev/null 2>&1; then
  rg --files --hidden -g '!.git' -g '!node_modules' -g '!vendor' 2>/dev/null | head -n %d
else
  find . \( -name .git -o -name node_modules -o -name vendor -o -name dist -o -name build -o -name __pycache__ -o -name .venv \) -prune -o -type f -print 2>/dev/null | sed 's|^\./||' | head -n %d
fi
`, shellQuote(wd), maxFiles, maxFiles, maxFiles)

	args := sshBashArgs(sshHost, false, script)
	out, err := exec.CommandContext(probeCtx, sshBinary, args...).Output()
	if err != nil {
		return nil, err
	}

	var files []string
	seen := map[string]struct{}{}
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		clean := strings.TrimPrefix(strings.TrimSpace(line), "./")
		if clean == "" || clean == "." || strings.HasPrefix(clean, ".git/") {
			continue
		}
		if _, dup := seen[clean]; dup {
			continue
		}
		seen[clean] = struct{}{}
		files = append(files, clean)
		if len(files) >= maxFiles {
			break
		}
	}
	sort.Strings(files)
	return files, nil
}

type scoredFile struct {
	path  string
	score int
}

// FuzzyMatchFiles ranks workspace relative file paths against a query string.
// An empty query returns top shallow/root files first up to limit.
func FuzzyMatchFiles(files []string, query string, limit int) []string {
	if limit <= 0 {
		limit = 10
	}
	if len(files) == 0 {
		return nil
	}

	q := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(query, "./")))
	if q == "" {
		cp := make([]string, len(files))
		copy(cp, files)
		sort.SliceStable(cp, func(i, j int) bool {
			di := strings.Count(cp[i], "/")
			dj := strings.Count(cp[j], "/")
			if di != dj {
				return di < dj
			}
			return cp[i] < cp[j]
		})
		if len(cp) > limit {
			cp = cp[:limit]
		}
		return cp
	}

	scored := make([]scoredFile, 0, min(len(files), 64))
	for _, f := range files {
		sc := scoreFileMatch(f, q)
		if sc > 0 {
			scored = append(scored, scoredFile{path: f, score: sc})
		}
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if len(scored[i].path) != len(scored[j].path) {
			return len(scored[i].path) < len(scored[j].path)
		}
		return scored[i].path < scored[j].path
	})

	if len(scored) > limit {
		scored = scored[:limit]
	}
	out := make([]string, len(scored))
	for i, s := range scored {
		out[i] = s.path
	}
	return out
}

func scoreFileMatch(relPath, queryLower string) int {
	lower := strings.ToLower(relPath)
	base := lower
	if idx := strings.LastIndexByte(lower, '/'); idx != -1 {
		base = lower[idx+1:]
	}

	switch {
	case lower == queryLower:
		return 1000
	case base == queryLower:
		return 920 - min(len(relPath), 80)
	case strings.HasPrefix(base, queryLower):
		return 820 - min(len(base), 60) - strings.Count(relPath, "/")*4
	case strings.HasPrefix(lower, queryLower):
		return 760 - min(len(relPath), 80)
	case strings.Contains(base, queryLower):
		return 660 - strings.Index(base, queryLower)*5 - strings.Count(relPath, "/")*3
	case strings.Contains(lower, queryLower):
		return 540 - strings.Index(lower, queryLower)*2
	}

	// Subsequence match (e.g. "mgo" -> "internal/harness/manager.go")
	qRunes := []rune(queryLower)
	qi := 0
	gaps := 0
	lastMatch := -1
	for idx, r := range lower {
		if qi < len(qRunes) && r == qRunes[qi] {
			if lastMatch >= 0 && idx-lastMatch > 1 {
				gaps += idx - lastMatch - 1
			}
			lastMatch = idx
			qi++
		}
	}
	if qi == len(qRunes) {
		return max(100, 360-gaps*4-strings.Count(relPath, "/")*5)
	}
	return 0
}

// ParseActiveAtMention inspects composer text and returns the trailing or active `@query` token
// if the user is currently typing an @file mention.
func ParseActiveAtMention(input string) (query string, tokenStart int, tokenEnd int, active bool) {
	if input == "" {
		return "", 0, 0, false
	}
	// Find the last '@' preceded by start-of-string or whitespace, with no whitespace after it.
	lastAt := strings.LastIndexByte(input, '@')
	if lastAt < 0 {
		return "", 0, 0, false
	}
	if lastAt > 0 {
		prevRune, _ := utf8.DecodeLastRuneInString(input[:lastAt])
		if !unicode.IsSpace(prevRune) {
			return "", 0, 0, false
		}
	}
	tail := input[lastAt+1:]
	// If there is any whitespace in tail, the @token has already been completed!
	for _, r := range tail {
		if unicode.IsSpace(r) {
			return "", 0, 0, false
		}
	}
	return tail, lastAt, len(input), true
}

// ReplaceActiveAtMention replaces the active `@query` token at the end of input with `@selectedPath `.
func ReplaceActiveAtMention(input, selectedPath string) string {
	_, start, end, ok := ParseActiveAtMention(input)
	if !ok {
		if input != "" && !strings.HasSuffix(input, " ") && !strings.HasSuffix(input, "\n") {
			return input + " @" + selectedPath + " "
		}
		return input + "@" + selectedPath + " "
	}
	return input[:start] + "@" + selectedPath + " " + input[end:]
}

// ExtractAtMentionPaths returns unique `@path` tokens found in a prompt string.
func ExtractAtMentionPaths(prompt string) []string {
	var out []string
	seen := map[string]struct{}{}
	fields := strings.Fields(prompt)
	for _, f := range fields {
		if !strings.HasPrefix(f, "@") || len(f) <= 1 {
			continue
		}
		candidate := strings.TrimRight(f[1:], ",.;:!?)\"'`")
		candidate = strings.TrimPrefix(candidate, "./")
		if candidate == "" || strings.Contains(candidate, "@") || strings.HasPrefix(candidate, "/") {
			continue
		}
		if _, dup := seen[candidate]; dup {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
		if len(out) >= maxReferencedFilesPerTurn {
			break
		}
	}
	return out
}

// ExpandAtMentionsContext resolves `@relative/path` mentions in the active workspace (Local or SSH),
// returning any referenced inline images and appending concise file content blocks for text files.
func ExpandAtMentionsContext(ctx context.Context, target store.WorkspaceTarget, prompt string) (string, []ai.ImageContent) {
	mentions := ExtractAtMentionPaths(prompt)
	if len(mentions) == 0 {
		return prompt, nil
	}

	var blocks []string
	var images []ai.ImageContent

	for _, relPath := range mentions {
		raw, err := readWorkspaceFileBounded(ctx, target, relPath, maxReferencedImageBytes)
		if err != nil || len(raw) == 0 {
			continue
		}
		if mime := detectImageMimeFromNameOrMagic(relPath, raw); mime != "" {
			images = append(images, ai.ImageContent{
				MimeType: mime,
				Data:     base64.StdEncoding.EncodeToString(raw),
			})
			continue
		}
		if !isLikelyUTF8Text(raw) {
			continue
		}
		snippet, truncated := formatNumberedSnippet(string(raw), maxReferencedFileLines, maxReferencedFileBytes)
		attr := ""
		if truncated {
			attr = ` truncated="true"`
		}
		blocks = append(blocks, fmt.Sprintf("<referenced_file path=%q%s>\n%s\n</referenced_file>", relPath, attr, snippet))
	}

	if len(blocks) == 0 {
		return prompt, images
	}
	return prompt + "\n\n" + attachedContextMarkerStart + " @file references]\n" + strings.Join(blocks, "\n\n"), images
}

func readWorkspaceFileBounded(ctx context.Context, target store.WorkspaceTarget, relPath string, maxBytes int) ([]byte, error) {
	cleanRel := filepath.ToSlash(filepath.Clean(relPath))
	if strings.HasPrefix(cleanRel, "../") || cleanRel == ".." || filepath.IsAbs(cleanRel) {
		return nil, fmt.Errorf("path escapes workspace: %s", relPath)
	}
	if target.IsRemote {
		remoteFull := resolveRemotePath(target.WorkDir, cleanRel)
		readCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		script := fmt.Sprintf("if [ -f %s ]; then head -c %d -- %s; else exit 1; fi",
			shellQuotePath(remoteFull), maxBytes, shellQuotePath(remoteFull))
		args := sshBashArgs(target.SSHHost, false, script)
		return exec.CommandContext(readCtx, sshBinary, args...).Output()
	}

	workDir := store.ExpandLocalPath(target.WorkDir)
	fullPath := filepath.Join(workDir, filepath.FromSlash(cleanRel))
	relCheck, err := filepath.Rel(workDir, fullPath)
	if err != nil || strings.HasPrefix(relCheck, "..") {
		return nil, fmt.Errorf("outside workspace")
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("not a regular file")
	}
	limit := int64(maxBytes)
	if info.Size() < limit {
		limit = info.Size()
	}
	buf := make([]byte, limit)
	n, _ := f.Read(buf)
	return buf[:n], nil
}

func formatNumberedSnippet(text string, maxLines, maxBytes int) (string, bool) {
	truncated := false
	if len(text) > maxBytes {
		text = text[:maxBytes]
		for len(text) > 0 && !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		truncated = true
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%4d | %s", i+1, line)
	}
	return b.String(), truncated
}

// ProbeGitStatus inspects Git branch, ahead/behind, and working-tree file changes on either Local or SSH workspaces.
func ProbeGitStatus(ctx context.Context, target store.WorkspaceTarget) GitWorkspaceStatus {
	st := GitWorkspaceStatus{CheckedAt: time.Now()}
	probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	var statusOut, numstatOut string
	if target.IsRemote {
		if strings.TrimSpace(target.SSHHost) == "" {
			st.Error = "未配置 SSH 主机"
			return st
		}
		wd := strings.TrimSpace(target.WorkDir)
		if wd == "" {
			wd = "~"
		}
		script := fmt.Sprintf(`
__wd=%s
if [ "$__wd" = "~" ]; then
  __wd="$HOME"
elif [ "${__wd#\~/}" != "$__wd" ]; then
  __wd="$HOME/${__wd#\~/}"
fi
cd "$__wd" 2>/dev/null || exit 2
command -v git >/dev/null 2>&1 || exit 3
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || exit 4
git status --porcelain=v1 -b 2>/dev/null
printf '\n__PIUI_NUMSTAT__\n'
git diff HEAD --numstat 2>/dev/null || git diff --numstat 2>/dev/null
`, shellQuote(wd))
		out, err := exec.CommandContext(probeCtx, sshBinary, sshBashArgs(target.SSHHost, false, script)...).Output()
		if err != nil {
			return st
		}
		parts := strings.SplitN(string(out), "\n__PIUI_NUMSTAT__\n", 2)
		statusOut = parts[0]
		if len(parts) > 1 {
			numstatOut = parts[1]
		}
	} else {
		workDir := store.ExpandLocalPath(target.WorkDir)
		if workDir == "" {
			return st
		}
		out, err := exec.CommandContext(probeCtx, "git", "-C", workDir, "status", "--porcelain=v1", "-b").Output()
		if err != nil {
			return st
		}
		statusOut = string(out)
		if ns, err := exec.CommandContext(probeCtx, "git", "-C", workDir, "diff", "HEAD", "--numstat").Output(); err == nil {
			numstatOut = string(ns)
		} else if ns2, err2 := exec.CommandContext(probeCtx, "git", "-C", workDir, "diff", "--numstat").Output(); err2 == nil {
			numstatOut = string(ns2)
		}
	}

	return parseGitStatusOutputs(statusOut, numstatOut)
}

func parseGitStatusOutputs(statusOut, numstatOut string) GitWorkspaceStatus {
	st := GitWorkspaceStatus{
		IsRepo:    true,
		CheckedAt: time.Now(),
	}

	type lineStat struct {
		added, removed int
	}
	statsByPath := map[string]lineStat{}
	for _, line := range strings.Split(strings.ReplaceAll(numstatOut, "\r\n", "\n"), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 3 {
			continue
		}
		add, _ := strconv.Atoi(fields[0])
		rem, _ := strconv.Atoi(fields[1])
		p := strings.TrimSpace(fields[2])
		statsByPath[p] = lineStat{added: add, removed: rem}
		st.TotalAdded += add
		st.TotalRemoved += rem
	}

	lines := strings.Split(strings.ReplaceAll(statusOut, "\r\n", "\n"), "\n")
	for _, rawLine := range lines {
		if strings.TrimSpace(rawLine) == "" {
			continue
		}
		if strings.HasPrefix(rawLine, "## ") {
			parseGitBranchHeader(rawLine[3:], &st)
			continue
		}
		if len(rawLine) < 3 {
			continue
		}
		xy := rawLine[:2]
		pathPart := strings.TrimSpace(rawLine[3:])
		if arrow := strings.Index(pathPart, " -> "); arrow != -1 {
			pathPart = strings.TrimSpace(pathPart[arrow+4:])
		}
		pathPart = strings.Trim(pathPart, `"`)

		ch := GitFileChange{
			Path:   pathPart,
			Status: normalizeGitXY(xy),
			Staged: xy[0] != ' ' && xy[0] != '?',
		}
		if ls, ok := statsByPath[pathPart]; ok {
			ch.Added = ls.added
			ch.Removed = ls.removed
		}
		if ch.Status == "?" {
			st.UntrackedCount++
		} else {
			st.ModifiedCount++
		}
		st.Changes = append(st.Changes, ch)
	}

	if st.Branch == "" {
		st.Branch = "HEAD"
	}
	return st
}

func parseGitBranchHeader(hdr string, st *GitWorkspaceStatus) {
	hdr = strings.TrimSpace(hdr)
	if bracket := strings.IndexByte(hdr, '['); bracket != -1 {
		meta := strings.TrimSuffix(hdr[bracket+1:], "]")
		hdr = strings.TrimSpace(hdr[:bracket])
		for _, part := range strings.Split(meta, ",") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "ahead ") {
				st.Ahead, _ = strconv.Atoi(strings.TrimPrefix(part, "ahead "))
			} else if strings.HasPrefix(part, "behind ") {
				st.Behind, _ = strconv.Atoi(strings.TrimPrefix(part, "behind "))
			}
		}
	}
	if strings.HasPrefix(hdr, "No commits yet on ") {
		st.Branch = strings.TrimPrefix(hdr, "No commits yet on ")
		return
	}
	if strings.HasPrefix(hdr, "Initial commit on ") {
		st.Branch = strings.TrimPrefix(hdr, "Initial commit on ")
		return
	}
	if dots := strings.Index(hdr, "..."); dots != -1 {
		st.Branch = hdr[:dots]
		return
	}
	st.Branch = hdr
}

func normalizeGitXY(xy string) string {
	if xy == "??" {
		return "?"
	}
	for _, c := range []byte{xy[1], xy[0]} {
		switch c {
		case 'M', 'A', 'D', 'R', 'C', 'U':
			return string(c)
		}
	}
	return "M"
}
