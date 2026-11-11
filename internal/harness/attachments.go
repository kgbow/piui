package harness

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/coding"

	"piui/internal/store"
)

// AttachmentKind distinguishes inline multimodal images from text/code file attachments.
type AttachmentKind string

const (
	AttachmentImage AttachmentKind = "image"
	AttachmentFile  AttachmentKind = "file"

	maxImageAttachmentBytes = 10 * 1024 * 1024 // 10 MiB
	maxTextAttachmentBytes  = 32 * 1024        // 32 KiB inline context per attached file
	maxTextAttachmentLines  = 350
)

// PromptAttachment represents a user-attached image (screenshot/paste/drop) or local file.
type PromptAttachment struct {
	ID           string
	Kind         AttachmentKind
	Name         string
	Path         string
	MimeType     string
	SizeBytes    int64
	ImageBase64  string
	PreviewBytes []byte
	TextSnippet  string
	Truncated    bool
}

// NewImageAttachmentFromBytes constructs a multimodal image attachment from raw PNG/JPEG/GIF/WebP bytes.
func NewImageAttachmentFromBytes(name, mimeType string, raw []byte) (PromptAttachment, error) {
	if len(raw) == 0 {
		return PromptAttachment{}, fmt.Errorf("图片内容为空")
	}
	if len(raw) > maxImageAttachmentBytes {
		return PromptAttachment{}, fmt.Errorf("图片体积超过 10MB 限制 (%s)", coding.FormatSize(len(raw)))
	}
	detected := detectImageMimeFromNameOrMagic(name, raw)
	if detected == "" {
		detected = strings.TrimSpace(mimeType)
	}
	if detected == "" {
		return PromptAttachment{}, fmt.Errorf("不支持的图片格式（仅支持 PNG / JPEG / GIF / WebP）")
	}
	if strings.TrimSpace(name) == "" {
		ext := ".png"
		switch detected {
		case "image/jpeg":
			ext = ".jpg"
		case "image/gif":
			ext = ".gif"
		case "image/webp":
			ext = ".webp"
		}
		name = "clipboard-image" + ext
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	return PromptAttachment{
		Kind:         AttachmentImage,
		Name:         name,
		MimeType:     detected,
		SizeBytes:    int64(len(raw)),
		ImageBase64:  base64.StdEncoding.EncodeToString(raw),
		PreviewBytes: cp,
	}, nil
}

// LoadAttachmentFromPath inspects a local file path (from Drag & Drop or File Picker) and builds a PromptAttachment.
func LoadAttachmentFromPath(rawPath string) (PromptAttachment, error) {
	fullPath := store.ExpandLocalPath(rawPath)
	info, err := os.Stat(fullPath)
	if err != nil {
		return PromptAttachment{}, fmt.Errorf("无法读取附件 %s: %w", filepath.Base(fullPath), err)
	}
	if info.IsDir() {
		return PromptAttachment{}, fmt.Errorf("暂不支持直接附加文件夹 (%s)", filepath.Base(fullPath))
	}

	name := filepath.Base(fullPath)

	// Check supported multimodal image first (via pi's magic-byte sniffer + fallback).
	if mime := coding.DetectSupportedImageMimeTypeFromFile(fullPath); mime != "" {
		if info.Size() > maxImageAttachmentBytes {
			return PromptAttachment{}, fmt.Errorf("图片 %s 超过 10MB 限制", name)
		}
		raw, err := os.ReadFile(fullPath)
		if err != nil {
			return PromptAttachment{}, err
		}
		att, err := NewImageAttachmentFromBytes(name, mime, raw)
		if err != nil {
			return PromptAttachment{}, err
		}
		att.Path = fullPath
		return att, nil
	}

	// Read bounded prefix for text/code attachment.
	f, err := os.Open(fullPath)
	if err != nil {
		return PromptAttachment{}, err
	}
	defer f.Close()

	readLen := info.Size()
	truncated := false
	if readLen > maxTextAttachmentBytes {
		readLen = maxTextAttachmentBytes
		truncated = true
	}
	buf := make([]byte, readLen)
	n, _ := f.Read(buf)
	buf = buf[:n]

	if mime := detectImageMimeFromNameOrMagic(name, buf); mime != "" {
		if info.Size() > maxImageAttachmentBytes {
			return PromptAttachment{}, fmt.Errorf("图片 %s 超过 10MB 限制", name)
		}
		raw, err := os.ReadFile(fullPath)
		if err != nil {
			return PromptAttachment{}, err
		}
		att, err := NewImageAttachmentFromBytes(name, mime, raw)
		if err != nil {
			return PromptAttachment{}, err
		}
		att.Path = fullPath
		return att, nil
	}

	if !isLikelyUTF8Text(buf) {
		return PromptAttachment{}, fmt.Errorf("不支持附加二进制文件: %s", name)
	}

	snippet, lineTrunc := formatNumberedSnippet(string(buf), maxTextAttachmentLines, maxTextAttachmentBytes)
	return PromptAttachment{
		Kind:        AttachmentFile,
		Name:        name,
		Path:        fullPath,
		MimeType:    "text/plain",
		SizeBytes:   info.Size(),
		TextSnippet: snippet,
		Truncated:   truncated || lineTrunc,
	}, nil
}

// BuildAttachmentPayload combines user text with any attached files and images.
func BuildAttachmentPayload(prompt string, attachments []PromptAttachment) (augmentedPrompt string, images []ai.ImageContent) {
	augmentedPrompt = strings.TrimSpace(prompt)
	if augmentedPrompt == "" && len(attachments) > 0 {
		augmentedPrompt = "请结合我附加的文件与图片内容进行分析处理。"
	}
	if len(attachments) == 0 {
		return augmentedPrompt, nil
	}

	var fileBlocks []string
	var summaryLabels []string
	for _, att := range attachments {
		switch att.Kind {
		case AttachmentImage:
			if att.ImageBase64 != "" {
				images = append(images, ai.ImageContent{
					MimeType: att.MimeType,
					Data:     att.ImageBase64,
				})
				summaryLabels = append(summaryLabels, fmt.Sprintf("[图片: %s]", att.Name))
			}
		case AttachmentFile:
			summaryLabels = append(summaryLabels, fmt.Sprintf("[文件: %s]", att.Name))
			truncAttr := ""
			if att.Truncated {
				truncAttr = ` truncated="true"`
			}
			fileBlocks = append(fileBlocks, fmt.Sprintf("<attached_file name=%q%s>\n%s\n</attached_file>", att.Name, truncAttr, att.TextSnippet))
		}
	}

	if len(fileBlocks) > 0 {
		augmentedPrompt += "\n\n" + attachedContextMarkerStart + " " + strings.Join(summaryLabels, " ") + "]\n" + strings.Join(fileBlocks, "\n\n")
	}
	return augmentedPrompt, images
}

func detectImageMimeFromNameOrMagic(name string, raw []byte) string {
	switch {
	case len(raw) >= 8 && bytes.Equal(raw[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png"
	case len(raw) >= 3 && raw[0] == 0xff && raw[1] == 0xd8 && raw[2] == 0xff:
		return "image/jpeg"
	case len(raw) >= 6 && (bytes.Equal(raw[:6], []byte("GIF87a")) || bytes.Equal(raw[:6], []byte("GIF89a"))):
		return "image/gif"
	case len(raw) >= 12 && bytes.Equal(raw[:4], []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP")):
		return "image/webp"
	}
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(name)))
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}

func isLikelyUTF8Text(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	if bytes.IndexByte(b, 0) != -1 {
		return false
	}
	// Trim possible trailing incomplete UTF-8 rune if read was capped mid-rune.
	check := b
	for i := 0; i < 3 && len(check) > 0 && !utf8.Valid(check); i++ {
		check = check[:len(check)-1]
	}
	return utf8.Valid(check)
}
