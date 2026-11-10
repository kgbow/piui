package harness

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// maxRemoteReadFullBytes caps full-file transfers over SSH (10 MiB).
	// Files larger than this are read via a bounded server-side line/byte window.
	maxRemoteReadFullBytes = 10 * 1024 * 1024
	remoteGrepMaxLineRunes = 500
)

var remotePNGSignature = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

// detectRemoteImageMimeType sniffs magic bytes to identify supported image formats
// (PNG, JPEG, GIF, WebP, BMP), matching sky-valley/pi/coding's read tool behavior.
func detectRemoteImageMimeType(buf []byte) string {
	if hasBytePrefix(buf, []byte{0xff, 0xd8, 0xff}) {
		if len(buf) > 3 && buf[3] == 0xf7 {
			return "" // CMYK JPEG unsupported
		}
		return "image/jpeg"
	}
	if hasBytePrefix(buf, remotePNGSignature) {
		if isRemotePNG(buf) && !isRemoteAnimatedPNG(buf) {
			return "image/png"
		}
		return ""
	}
	if hasASCIIAt(buf, 0, "GIF87a") || hasASCIIAt(buf, 0, "GIF89a") {
		return "image/gif"
	}
	if hasASCIIAt(buf, 0, "RIFF") && hasASCIIAt(buf, 8, "WEBP") {
		return "image/webp"
	}
	if hasASCIIAt(buf, 0, "BM") && isRemoteBMP(buf) {
		return "image/bmp"
	}
	return ""
}

func hasBytePrefix(buf, prefix []byte) bool {
	if len(buf) < len(prefix) {
		return false
	}
	for i := range prefix {
		if buf[i] != prefix[i] {
			return false
		}
	}
	return true
}

func hasASCIIAt(buf []byte, offset int, text string) bool {
	if len(buf) < offset+len(text) {
		return false
	}
	for i := 0; i < len(text); i++ {
		if buf[offset+i] != text[i] {
			return false
		}
	}
	return true
}

func readU32BE(buf []byte, off int) int {
	b := func(i int) int {
		if i < len(buf) {
			return int(buf[i])
		}
		return 0
	}
	return b(off)*0x1000000 + (b(off+1) << 16) + (b(off+2) << 8) + b(off+3)
}

func readU16LE(buf []byte, off int) int {
	b := func(i int) int {
		if i < len(buf) {
			return int(buf[i])
		}
		return 0
	}
	return b(off) + (b(off+1) << 8)
}

func readU32LE(buf []byte, off int) int {
	b := func(i int) int {
		if i < len(buf) {
			return int(buf[i])
		}
		return 0
	}
	return b(off) + (b(off+1) << 8) + (b(off+2) << 16) + b(off+3)*0x1000000
}

func isRemotePNG(buf []byte) bool {
	return len(buf) >= 16 && readU32BE(buf, len(remotePNGSignature)) == 13 && hasASCIIAt(buf, 12, "IHDR")
}

func isRemoteAnimatedPNG(buf []byte) bool {
	off := len(remotePNGSignature)
	for off+8 <= len(buf) {
		chunkLen := readU32BE(buf, off)
		if hasASCIIAt(buf, off+4, "acTL") {
			return true
		}
		if hasASCIIAt(buf, off+4, "IDAT") {
			return false
		}
		if chunkLen < 0 || off+12+chunkLen <= off {
			return false
		}
		off += 12 + chunkLen
	}
	return false
}

func isRemoteBMP(buf []byte) bool {
	if len(buf) < 26 {
		return false
	}
	fileSize := readU32LE(buf, 2)
	pixelOff := readU32LE(buf, 10)
	dibSize := readU32LE(buf, 14)
	if (fileSize != 0 && fileSize < 26) || pixelOff < 14+dibSize || (fileSize != 0 && pixelOff >= fileSize) {
		return false
	}
	var planes, bpp int
	if dibSize == 12 {
		planes = readU16LE(buf, 22)
		bpp = readU16LE(buf, 24)
	} else if dibSize >= 40 && dibSize <= 124 {
		if len(buf) < 30 {
			return false
		}
		planes = readU16LE(buf, 26)
		bpp = readU16LE(buf, 28)
	} else {
		return false
	}
	if planes != 1 {
		return false
	}
	switch bpp {
	case 1, 4, 8, 16, 24, 32:
		return true
	default:
		return false
	}
}

// compileGlobMatcher compiles a POSIX/doublestar glob (such as "*.go", "src/**/*.go",
// "**/*.test.{ts,tsx}", or "internal/*/config.go") into a slash-path matcher.
// Patterns without "/" match against either the basename or full path (matching fd/pi).
func compileGlobMatcher(pattern string) (func(string) bool, error) {
	pat := strings.TrimSpace(strings.ReplaceAll(pattern, "\\", "/"))
	pat = strings.TrimPrefix(pat, "./")
	pat = strings.TrimPrefix(pat, "/")
	if pat == "" {
		return nil, fmt.Errorf("empty glob pattern")
	}

	hasSlash := strings.Contains(pat, "/")
	reBody := globFragmentToRegex(pat)
	var fullExpr string
	if hasSlash {
		fullExpr = "^" + reBody + "$"
	} else {
		fullExpr = "(?:^|/)" + reBody + "$"
	}
	re, err := regexp.Compile(fullExpr)
	if err != nil {
		return nil, fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
	}
	return func(relPath string) bool {
		clean := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(relPath), "\\", "/"), "./")
		clean = strings.TrimPrefix(clean, "/")
		if clean == "" {
			return false
		}
		return re.MatchString(clean)
	}, nil
}

func globFragmentToRegex(pat string) string {
	var b strings.Builder
	n := len(pat)
	for i := 0; i < n; {
		// Handle "**/" (zero or more directories), "/**" at end, and standalone "**".
		if i+1 < n && pat[i] == '*' && pat[i+1] == '*' {
			if i+2 < n && pat[i+2] == '/' {
				b.WriteString("(?:.+/)?")
				i += 3
				continue
			}
			if i > 0 && pat[i-1] == '/' && i+2 == n {
				b.WriteString(".*")
				i += 2
				continue
			}
			b.WriteString(".*")
			i += 2
			continue
		}

		c := pat[i]
		switch c {
		case '*':
			b.WriteString("[^/]*")
			i++
		case '?':
			b.WriteString("[^/]")
			i++
		case '[':
			end := strings.IndexByte(pat[i+1:], ']')
			if end == -1 {
				b.WriteString(`\[`)
				i++
			} else {
				cls := pat[i+1 : i+1+end]
				if strings.HasPrefix(cls, "!") {
					cls = "^" + cls[1:]
				}
				b.WriteByte('[')
				b.WriteString(cls)
				b.WriteByte(']')
				i += end + 2
			}
		case '{':
			end := strings.IndexByte(pat[i+1:], '}')
			if end == -1 {
				b.WriteString(`\{`)
				i++
			} else {
				parts := strings.Split(pat[i+1:i+1+end], ",")
				b.WriteString("(?:")
				for idx, part := range parts {
					if idx > 0 {
						b.WriteByte('|')
					}
					b.WriteString(globFragmentToRegex(part))
				}
				b.WriteByte(')')
				i += end + 2
			}
		case '.', '+', '^', '$', '(', ')', '|', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

var grepMatchLineRe = regexp.MustCompile(`^(.+?):([0-9]+):(.*)$`)

// filterAndFormatRemoteGrepOutput filters raw `rg -nH` / `git grep -n` / `grep -rnH`
// output by optional glob, enforces match count limit across context blocks, and truncates long lines.
func filterAndFormatRemoteGrepOutput(raw, globPat string, limit int, hasContext bool) (string, error) {
	var globMatch func(string) bool
	if strings.TrimSpace(globPat) != "" {
		var err error
		globMatch, err = compileGlobMatcher(globPat)
		if err != nil {
			return "", err
		}
	}

	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	trimmed := strings.TrimRight(normalized, "\n")
	if strings.TrimSpace(trimmed) == "" {
		return "", nil
	}

	truncateGrepLine := func(s string) string {
		if utf8.RuneCountInString(s) <= remoteGrepMaxLineRunes {
			return s
		}
		runes := []rune(s)
		return string(runes[:remoteGrepMaxLineRunes]) + "... [truncated]"
	}

	if !hasContext {
		var outLines []string
		matches := 0
		for _, line := range strings.Split(trimmed, "\n") {
			if line == "" {
				continue
			}
			m := grepMatchLineRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			filePath := strings.TrimPrefix(m[1], "./")
			if globMatch != nil && !globMatch(filePath) {
				continue
			}
			outLines = append(outLines, fmt.Sprintf("%s:%s: %s", filePath, m[2], truncateGrepLine(m[3])))
			matches++
			if matches >= limit {
				break
			}
		}
		return strings.Join(outLines, "\n"), nil
	}

	// With context (-C N), blocks are separated by "--" lines.
	var blocks [][]string
	var cur []string
	for _, line := range strings.Split(trimmed, "\n") {
		if line == "--" {
			if len(cur) > 0 {
				blocks = append(blocks, cur)
				cur = nil
			}
			continue
		}
		if line != "" {
			cur = append(cur, line)
		}
	}
	if len(cur) > 0 {
		blocks = append(blocks, cur)
	}

	var keptBlocks []string
	totalMatches := 0
	for _, blk := range blocks {
		if totalMatches >= limit {
			break
		}
		// Identify file path from the first match line in this block.
		blockFile := ""
		for _, line := range blk {
			if m := grepMatchLineRe.FindStringSubmatch(line); m != nil {
				blockFile = m[1]
				break
			}
		}
		cleanFile := strings.TrimPrefix(blockFile, "./")
		if cleanFile != "" && globMatch != nil && !globMatch(cleanFile) {
			continue
		}

		var formatted []string
		for _, line := range blk {
			if blockFile != "" && strings.HasPrefix(line, blockFile+":") {
				rest := line[len(blockFile)+1:]
				if colon := strings.IndexByte(rest, ':'); colon > 0 && isDigitsOnly(rest[:colon]) {
					if totalMatches >= limit {
						break
					}
					totalMatches++
					formatted = append(formatted, fmt.Sprintf("%s:%s: %s", cleanFile, rest[:colon], truncateGrepLine(rest[colon+1:])))
					continue
				}
			}
			if blockFile != "" && strings.HasPrefix(line, blockFile+"-") {
				rest := line[len(blockFile)+1:]
				if dash := strings.IndexByte(rest, '-'); dash > 0 && isDigitsOnly(rest[:dash]) {
					formatted = append(formatted, fmt.Sprintf("%s-%s- %s", cleanFile, rest[:dash], truncateGrepLine(rest[dash+1:])))
					continue
				}
			}
			formatted = append(formatted, truncateGrepLine(strings.TrimPrefix(line, "./")))
		}
		if len(formatted) > 0 {
			keptBlocks = append(keptBlocks, strings.Join(formatted, "\n"))
		}
	}
	return strings.Join(keptBlocks, "\n--\n"), nil
}

func isDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
}

// cleanRelativeFindPaths normalizes remote file listing lines and filters them by glob.
func cleanRelativeFindPaths(raw, pattern string, limit int) ([]string, error) {
	matcher, err := compileGlobMatcher(pattern)
	if err != nil {
		return nil, err
	}
	var matched []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		p := strings.TrimSpace(line)
		p = strings.TrimPrefix(p, "./")
		p = path.Clean(p)
		if p == "" || p == "." || seen[p] {
			continue
		}
		if matcher(p) {
			seen[p] = true
			matched = append(matched, p)
			if limit > 0 && len(matched) >= limit {
				break
			}
		}
	}
	return matched, nil
}
