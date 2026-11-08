package harness

import (
	"strings"
	"testing"
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
