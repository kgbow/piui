package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"piui/internal/store"
)

func makeTinyPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.RGBA{R: 60, G: 120, B: 240, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestWorkspaceFilesFuzzyAtMentionsAndGitStatusLocalAndSSH(t *testing.T) {
	installFakeSSH(t)
	sshAlias := "deploy@prod"
	remoteBase := t.TempDir()

	repoDir := filepath.Join(remoteBase, "proj")
	if err := os.MkdirAll(filepath.Join(repoDir, "internal", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "node_modules", "ignored-pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "internal", "api", "handler.go"), []byte("package api\n\nfunc HandlePing() string {\n\treturn \"pong\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Demo Project\nLine 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "node_modules", "ignored-pkg", "index.js"), []byte("module.exports = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Initialize a real git repo in repoDir
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	runGit("init", "-b", "feat/phase2")
	if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README.md", ".gitignore", "internal/api/handler.go")
	runGit("commit", "-m", "initial")

	// Modify README.md and add an untracked file
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# Demo Project\nUpdated Line 2\nAdded Line 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "notes.txt"), []byte("todo item\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	localTarget := store.WorkspaceTarget{ID: "loc", Name: "Local", WorkDir: repoDir}
	remoteTarget := store.WorkspaceTarget{ID: "rem", Name: "Remote", IsRemote: true, SSHHost: sshAlias, WorkDir: repoDir}

	ctx := context.Background()
	for _, tc := range []struct {
		label  string
		target store.WorkspaceTarget
	}{
		{"local", localTarget},
		{"ssh", remoteTarget},
	} {
		files, err := ListWorkspaceFiles(ctx, tc.target, 500)
		if err != nil {
			t.Fatalf("[%s] ListWorkspaceFiles: %v", tc.label, err)
		}
		matches := FuzzyMatchFiles(files, "hand", 5)
		if len(matches) == 0 || matches[0] != "internal/api/handler.go" {
			t.Fatalf("[%s] expected top fuzzy match internal/api/handler.go, got %v (files=%v)", tc.label, matches, files)
		}
		for _, f := range files {
			if strings.Contains(f, "node_modules") {
				t.Fatalf("[%s] expected .gitignore node_modules to be excluded, got %q", tc.label, f)
			}
		}

		// Verify @file context expansion
		aug, imgs := ExpandAtMentionsContext(ctx, tc.target, "请检查 @internal/api/handler.go 的返回值")
		if len(imgs) != 0 || !strings.Contains(aug, `<referenced_file path="internal/api/handler.go">`) || !strings.Contains(aug, "HandlePing") {
			t.Fatalf("[%s] unexpected @file expansion: %q", tc.label, aug)
		}

		// Verify Git status probe
		gs := ProbeGitStatus(ctx, tc.target)
		if !gs.IsRepo || gs.Branch != "feat/phase2" {
			t.Fatalf("[%s] expected git branch feat/phase2, got %+v", tc.label, gs)
		}
		if gs.ModifiedCount != 1 || gs.UntrackedCount != 1 || gs.TotalAdded < 2 {
			t.Fatalf("[%s] unexpected git change counts: %+v", tc.label, gs)
		}
	}

	// Verify @mention input parser & replacer
	q, start, _, active := ParseActiveAtMention("帮我看一下 @int/ap")
	if !active || q != "int/ap" || start == 0 {
		t.Fatalf("unexpected ParseActiveAtMention result: q=%q start=%d active=%v", q, start, active)
	}
	replaced := ReplaceActiveAtMention("帮我看一下 @int/ap", "internal/api/handler.go")
	if replaced != "帮我看一下 @internal/api/handler.go " {
		t.Fatalf("unexpected ReplaceActiveAtMention: %q", replaced)
	}
	if _, _, _, activeDone := ParseActiveAtMention(replaced); activeDone {
		t.Fatalf("expected completed @mention with trailing space to be inactive")
	}
}

func TestChangeTrackerUnifiedDiffAndRollbackLocalAndSSH(t *testing.T) {
	installFakeSSH(t)
	sshAlias := "deploy@prod"
	remoteBase := t.TempDir()

	for _, isRemote := range []bool{false, true} {
		dir := filepath.Join(remoteBase, map[bool]string{false: "chg-local", true: "chg-ssh"}[isRemote])
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		origContent := "line1\nline2\nline3\nline4\n"
		existingFile := filepath.Join(dir, "service.go")
		if err := os.WriteFile(existingFile, []byte(origContent), 0o644); err != nil {
			t.Fatal(err)
		}

		target := store.WorkspaceTarget{
			ID:       "wt",
			Name:     "WT",
			IsRemote: isRemote,
			SSHHost:  sshAlias,
			WorkDir:  dir,
		}
		router := NewEnvironmentRouter(target, nil)
		tools := router.BuildTools(dir)

		var editToolIdx, writeToolIdx int = -1, -1
		for i, tl := range tools {
			if tl.Name == "edit" {
				editToolIdx = i
			} else if tl.Name == "write" {
				writeToolIdx = i
			}
		}
		if editToolIdx < 0 || writeToolIdx < 0 {
			t.Fatalf("missing edit/write tools")
		}

		ctx := context.Background()
		// 1. Edit existing file -> verify diff snapshot -> revert -> verify original content restored
		_, err := tools[editToolIdx].Execute(ctx, "tc-edit-1", map[string]any{
			"path":    "service.go",
			"oldText": "line2\nline3",
			"newText": "line2_modified\nline3_added_a\nline3_added_b",
		}, nil)
		if err != nil {
			t.Fatalf("[remote=%v] edit failed: %v", isRemote, err)
		}

		snap, ok := router.Changes().Get("tc-edit-1")
		if !ok || snap.AddedLines != 3 || snap.RemovedLines != 2 || len(snap.DiffLines) == 0 {
			t.Fatalf("[remote=%v] unexpected edit snapshot: ok=%v snap=%+v", isRemote, ok, snap)
		}

		revertedSnap, err := router.Changes().RevertMutation(ctx, "tc-edit-1")
		if err != nil || !revertedSnap.Reverted {
			t.Fatalf("[remote=%v] RevertMutation failed: %v", isRemote, err)
		}
		gotBytes, err := os.ReadFile(existingFile)
		if err != nil || string(gotBytes) != origContent {
			t.Fatalf("[remote=%v] expected reverted content %q, got %q (err=%v)", isRemote, origContent, string(gotBytes), err)
		}

		// 2. Write brand-new file -> revert -> verify newly created file is deleted
		newFile := filepath.Join(dir, "created.txt")
		_, err = tools[writeToolIdx].Execute(ctx, "tc-write-1", map[string]any{
			"path":    "created.txt",
			"content": "brand new file\nsecond line\n",
		}, nil)
		if err != nil {
			t.Fatalf("[remote=%v] write failed: %v", isRemote, err)
		}
		wSnap, ok := router.Changes().Get("tc-write-1")
		if !ok || wSnap.ExistedBefore || wSnap.AddedLines != 2 {
			t.Fatalf("[remote=%v] unexpected write snapshot: ok=%v snap=%+v", isRemote, ok, wSnap)
		}
		if _, err := router.Changes().RevertMutation(ctx, "tc-write-1"); err != nil {
			t.Fatalf("[remote=%v] revert newly created file failed: %v", isRemote, err)
		}
		if _, statErr := os.Stat(newFile); !os.IsNotExist(statErr) {
			t.Fatalf("[remote=%v] expected newly created file to be deleted on revert, statErr=%v", isRemote, statErr)
		}
	}
}

func TestMultimodalAttachmentsAndSessionCompactionPreservingTree(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("PIUI_SESSION_DIR", "")
	store.SetSecretStore(store.NewMemorySecretStore())

	var (
		mu          sync.Mutex
		lastRawBody string
	)
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodyStr := string(raw)
		mu.Lock()
		lastRawBody = bodyStr
		mu.Unlock()

		replyText := "已收到图片与代码附件，处理完毕。"
		if strings.Contains(bodyStr, "context summarization assistant") {
			replyText = "## Goal\n- 实现多模态附件与上下文压缩\n\n## Progress\n- [x] 已完成核心验证"
		}

		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{
			"id":      "chatcmpl-p2",
			"object":  "chat.completion.chunk",
			"created": 1700000000,
			"model":   "gpt-4o",
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         map[string]any{"role": "assistant", "content": replyText},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     420,
				"completion_tokens": 85,
				"total_tokens":      505,
			},
		})
		_, _ = w.Write([]byte("data: " + string(chunk) + "\n\ndata: [DONE]\n\n"))
	}))
	defer mockLLM.Close()

	workDir := t.TempDir()
	pngPath := filepath.Join(workDir, "shot.png")
	if err := os.WriteFile(pngPath, makeTinyPNGBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(workDir, "app.yaml")
	if err := os.WriteFile(cfgPath, []byte("port: 8080\nmode: production\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	imgAtt, err := LoadAttachmentFromPath(pngPath)
	if err != nil || imgAtt.Kind != AttachmentImage {
		t.Fatalf("LoadAttachmentFromPath image failed: %v (%+v)", err, imgAtt)
	}
	fileAtt, err := LoadAttachmentFromPath(cfgPath)
	if err != nil || fileAtt.Kind != AttachmentFile {
		t.Fatalf("LoadAttachmentFromPath text failed: %v (%+v)", err, fileAtt)
	}

	t.Setenv("PIUI_CONFIG_PATH", filepath.Join(t.TempDir(), "config.json"))
	cfg := store.LoadOrInit(workDir)
	cfg.Profiles = []store.ProviderProfile{
		{
			ID:            "mm-prof",
			Name:          "Multimodal",
			Format:        store.FormatOpenAICompletions,
			BaseURL:       mockLLM.URL + "/v1",
			APIKey:        "sk-mm",
			ModelID:       "gpt-4o",
			ThinkingLevel: "medium",
		},
	}
	cfg.ActiveProfileID = "mm-prof"

	mgr, err := NewManager(workDir, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	ctx := context.Background()
	if _, err := mgr.RunWithAttachments(ctx, "请查看截图和配置", []PromptAttachment{imgAtt, fileAtt}); err != nil {
		t.Fatalf("RunWithAttachments: %v", err)
	}

	mu.Lock()
	capturedReq := lastRawBody
	mu.Unlock()
	if !strings.Contains(capturedReq, "image_url") || !strings.Contains(capturedReq, "port: 8080") {
		t.Fatalf("expected multimodal image_url and attached app.yaml snippet in LLM request, got: %s", capturedReq)
	}

	usageSnap := mgr.ContextUsageSnapshot()
	if usageSnap.ContextTokens <= 0 || usageSnap.TurnCount != 1 {
		t.Fatalf("expected non-zero usage-anchored ContextUsageSnapshot, got %+v", usageSnap)
	}

	// Add a second turn with a long message so compaction shrinks context AND forks cleanly in the JSONL tree!
	longPrompt := "补充一段较长的历史设计文档说明：\n" + strings.Repeat("Architecture detail line with specific module invariants.\n", 80)
	if _, err := mgr.Run(ctx, longPrompt); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	compRes, err := mgr.CompactSession(ctx, "保留端口与架构关键结论", cfg.ActiveProfile())
	if err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if !strings.Contains(compRes.Summary, "## Goal") {
		t.Fatalf("expected structured checkpoint summary, got: %q", compRes.Summary)
	}
	if compRes.TreeSnap == nil || compRes.TreeSnap.BranchCount < 2 {
		t.Fatalf("expected JSONL session tree to preserve pre-compaction history as a sibling branch (BranchCount>=2), got %+v", compRes.TreeSnap)
	}
}
