package ui

import (
	"regexp"
	"strings"
)

var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// StripANSI removes terminal CSI/OSC ANSI escape sequences and normalizes carriage returns
// so inline tool cards render clean, readable monospace output.
func StripANSI(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if !strings.ContainsRune(s, '\x1b') {
		return s
	}
	return ansiEscapeRe.ReplaceAllString(s, "")
}

// TailLines returns the last maxLines non-empty lines of s (with ANSI stripped) for compact
// live streaming previews inside running bash tool cards.
func TailLines(s string, maxLines int) (tail string, totalLines int) {
	cleaned := strings.TrimRight(StripANSI(s), "\n ")
	if cleaned == "" || maxLines <= 0 {
		return "", 0
	}
	rawLines := strings.Split(cleaned, "\n")
	totalLines = len(rawLines)
	start := 0
	if totalLines > maxLines {
		start = totalLines - maxLines
	}
	slice := rawLines[start:]
	var b strings.Builder
	for i, line := range slice {
		if i > 0 {
			b.WriteByte('\n')
		}
		runes := []rune(line)
		if len(runes) > 160 {
			b.WriteString(string(runes[:160]) + "…")
		} else {
			b.WriteString(line)
		}
	}
	return b.String(), totalLines
}
