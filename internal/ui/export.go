package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"piui/internal/diag"
	"piui/internal/store"
)

// FormatSessionMarkdown renders the current conversation timeline (including user prompts,
// assistant responses, reasoning summaries, tool calls, and unified file diffs) as clean Markdown.
func FormatSessionMarkdown(title string, target store.WorkspaceTarget, modelLabel string, entries []*ChatEntry) string {
	var b strings.Builder
	if strings.TrimSpace(title) == "" {
		title = "PiUI 会话导出"
	}
	fmt.Fprintf(&b, "# %s\n\n", strings.TrimSpace(title))
	fmt.Fprintf(&b, "- **工作区**: `%s` (%s)\n", target.ShortName(), target.DisplaySub())
	if strings.TrimSpace(modelLabel) != "" {
		fmt.Fprintf(&b, "- **模型**: `%s`\n", strings.TrimSpace(modelLabel))
	}
	fmt.Fprintf(&b, "- **导出时间**: `%s`\n\n---\n\n", time.Now().Format("2006-01-02 15:04:05"))

	for _, entry := range entries {
		if entry == nil {
			continue
		}
		switch entry.Kind {
		case EntryUser:
			b.WriteString("## 提问 (User)\n\n")
			if len(entry.Attachments) > 0 {
				var names []string
				for _, att := range entry.Attachments {
					names = append(names, "`"+att.Name+"`")
				}
				fmt.Fprintf(&b, "> 附件: %s\n\n", strings.Join(names, ", "))
			}
			if strings.TrimSpace(entry.Text) != "" {
				b.WriteString(strings.TrimSpace(entry.Text) + "\n\n")
			}

		case EntryAssistant:
			b.WriteString("## 助手 (Assistant)\n\n")
			if strings.TrimSpace(entry.Thinking) != "" {
				b.WriteString("<details>\n<summary>思考过程</summary>\n\n")
				b.WriteString(strings.TrimSpace(entry.Thinking) + "\n\n</details>\n\n")
			}
			if strings.TrimSpace(entry.Text) != "" {
				b.WriteString(strings.TrimSpace(entry.Text) + "\n\n")
			}
			if strings.TrimSpace(entry.ErrorText) != "" {
				fmt.Fprintf(&b, "> **错误**: %s\n\n", strings.TrimSpace(entry.ErrorText))
			}

		case EntryTool:
			_, label := toolVisualMeta(entry.ToolName)
			header := fmt.Sprintf("工具调用 `%s`", label)
			if entry.ToolSummary != "" {
				header += fmt.Sprintf(" — `%s`", entry.ToolSummary)
			}
			if entry.DurationMs > 0 {
				header += fmt.Sprintf(" (%dms)", entry.DurationMs)
			}
			if entry.Reverted {
				header += " [已撤销]"
			}
			b.WriteString("<details>\n<summary>" + header + "</summary>\n\n")
			if len(entry.DiffLines) > 0 {
				b.WriteString("```diff\n")
				for _, dl := range entry.DiffLines {
					switch dl.Kind {
					case '@':
						b.WriteString(dl.Text + "\n")
					case '+':
						b.WriteString("+" + dl.Text + "\n")
					case '-':
						b.WriteString("-" + dl.Text + "\n")
					default:
						b.WriteString(" " + dl.Text + "\n")
					}
				}
				b.WriteString("```\n\n")
			} else if entry.DiffOld != "" || entry.DiffNew != "" {
				b.WriteString("```diff\n")
				for _, l := range strings.Split(entry.DiffOld, "\n") {
					b.WriteString("-" + l + "\n")
				}
				for _, l := range strings.Split(entry.DiffNew, "\n") {
					b.WriteString("+" + l + "\n")
				}
				b.WriteString("```\n\n")
			}
			if strings.TrimSpace(entry.ToolOutput) != "" {
				b.WriteString("```text\n")
				b.WriteString(truncatePreview(StripANSI(entry.ToolOutput), 4000) + "\n")
				b.WriteString("```\n\n")
			}
			b.WriteString("</details>\n\n")
		}
	}
	return b.String()
}

// BuildActiveSessionMarkdown generates the Markdown representation of the active session.
func (a *DeskApp) BuildActiveSessionMarkdown() string {
	var target store.WorkspaceTarget
	if a.mgr != nil {
		target = a.mgr.Router().Target()
	}
	modelLabel := ""
	if prof := a.cfg.ActiveProfile(); prof != nil {
		modelLabel = fmt.Sprintf("%s / %s", prof.Name, prof.ModelID)
	}
	title := ""
	if a.activeSessID != "" {
		title = a.cfg.SessionTitle(a.activeSessID, "")
	}
	if title == "" {
		for _, e := range a.entries {
			if e != nil && e.Kind == EntryUser && strings.TrimSpace(e.Text) != "" {
				title = singleLinePreview(e.Text, 48)
				break
			}
		}
	}
	return FormatSessionMarkdown(title, target, modelLabel, a.entries)
}

// ExportActiveSession exports the current conversation to a Markdown file (via explicit path or
// native Save dialog when a window is bound, falling back to clipboard in headless mode).
func (a *DeskApp) ExportActiveSession(c *ui.Context, explicitPath string) {
	if len(a.entries) == 0 {
		if c != nil {
			c.Toast("当前会话暂无内容可导出")
		}
		return
	}
	md := a.BuildActiveSessionMarkdown()
	explicitPath = strings.TrimSpace(explicitPath)

	if explicitPath != "" {
		outPath := store.ExpandLocalPath(explicitPath)
		if !filepath.IsAbs(outPath) && a.mgr != nil {
			t := a.mgr.Router().Target()
			if !t.IsRemote && t.WorkDir != "" {
				outPath = filepath.Join(store.ExpandLocalPath(t.WorkDir), outPath)
			}
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			a.lastError = "导出会话创建目录失败: " + err.Error()
			return
		}
		if err := os.WriteFile(outPath, []byte(md), 0o644); err != nil {
			a.lastError = "导出会话写入文件失败: " + err.Error()
			return
		}
		diag.Infof("Exported session markdown to %s (%d bytes)", outPath, len(md))
		if c != nil {
			c.Toast("已导出对话至: " + store.ShortenHomePath(outPath))
		}
		return
	}

	if a.win == nil {
		if c != nil {
			c.WriteClipboard(md)
			c.Toast("已复制当前会话 Markdown 到剪贴板")
		}
		return
	}

	defaultDir := ""
	if a.mgr != nil {
		t := a.mgr.Router().Target()
		if !t.IsRemote && t.WorkDir != "" {
			defaultDir = store.ExpandLocalPath(t.WorkDir)
		}
	}
	if defaultDir == "" {
		defaultDir, _ = os.UserHomeDir()
	}
	defaultFile := filepath.Join(defaultDir, fmt.Sprintf("piui-session-%s.md", time.Now().Format("20060102-1504")))
	win := a.win

	go func() {
		savePath, err := mygo.Dialog.Save(mygo.SaveDialogOptions{
			Parent:            win,
			Title:             "导出当前对话为 Markdown",
			DefaultPath:       defaultFile,
			ButtonLabel:       "导出",
			CreateDirectories: true,
			Filters: []mygo.FileFilter{
				{Name: "Markdown 文档", Extensions: []string{"md"}},
			},
		})
		if err != nil || strings.TrimSpace(savePath) == "" {
			return
		}
		writeErr := os.WriteFile(savePath, []byte(md), 0o644)
		a.updateUI(func() {
			if writeErr != nil {
				a.lastError = "导出 Markdown 失败: " + writeErr.Error()
				return
			}
			diag.Infof("Exported session markdown via dialog to %s", savePath)
		})
	}()
}

// CopyDiagnosticSummary generates a sanitized runtime & log report and copies it to the clipboard.
func (a *DeskApp) CopyDiagnosticSummary(c *ui.Context) string {
	var in diag.SummaryInput
	in.ConfigPath = store.ShortenHomePath(store.ConfigPath())
	in.LastError = a.lastError
	in.SessionID = a.activeSessID
	if a.cfg != nil {
		in.MCPServers = len(a.cfg.MCPServers)
		if prof := a.cfg.ActiveProfile(); prof != nil {
			in.ModelLabel = fmt.Sprintf("%s / %s", prof.Name, prof.ModelID)
		}
	}
	if a.mgr != nil {
		t := a.mgr.Router().Target()
		in.WorkspaceLabel = t.DisplaySub()
		in.IsRemote = t.IsRemote
		in.MCPTools = a.mgr.MCPActiveToolCount()
		if t.IsRemote {
			st := a.mgr.SSHStatus()
			if st.Connected {
				in.SSHStatus = fmt.Sprintf("connected (%dms, %s)", st.LatencyMs, st.RemoteOS)
			} else if st.Error != "" {
				in.SSHStatus = "error: " + st.Error
			} else {
				in.SSHStatus = "not probed"
			}
		}
	}
	report := diag.FormatDiagnosticReport(in)
	if c != nil {
		c.WriteClipboard(report)
		c.Toast("已复制运行诊断信息与日志摘要到剪贴板")
	}
	return report
}
