// Attachment staging shared by the headless and ACP Grok transports.
package grok

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// stageTurnAttachments persists inbound images and files so the Grok agent can
// open them by path, and appends those paths to the prompt (the CLI has no
// inline attachment channel on either transport). The returned cleanup removes
// the staged files once the turn is over; it is safe to call more than once.
func stageTurnAttachments(workDir, prompt string, images []core.ImageAttachment, files []core.FileAttachment) (string, func()) {
	if len(images) == 0 && len(files) == 0 {
		return prompt, func() {}
	}

	attachDir := filepath.Join(workDir, ".cc-connect", "attachments")
	if os.MkdirAll(attachDir, 0o755) != nil {
		attachDir = os.TempDir()
	}

	var staged []string
	var imageRefs []string
	for i, img := range images {
		ext := ".png"
		switch img.MimeType {
		case "image/jpeg":
			ext = ".jpg"
		case "image/gif":
			ext = ".gif"
		case "image/webp":
			ext = ".webp"
		}
		fname := fmt.Sprintf("img_%d_%d%s", time.Now().UnixMilli(), i, ext)
		fpath := filepath.Join(attachDir, fname)
		if err := os.WriteFile(fpath, img.Data, 0o644); err != nil {
			slog.Warn("grok: failed to save image", "error", err)
			continue
		}
		staged = append(staged, fpath)
		imageRefs = append(imageRefs, fpath)
	}

	var fileRefs []string
	for i, f := range files {
		fname := filepath.Base(f.FileName)
		if fname == "" || fname == "." || fname == ".." {
			fname = fmt.Sprintf("file_%d_%d", time.Now().UnixMilli(), i)
		}
		fpath := filepath.Join(attachDir, fname)
		if err := os.WriteFile(fpath, f.Data, 0o644); err != nil {
			slog.Warn("grok: failed to save file", "error", err)
			continue
		}
		staged = append(staged, fpath)
		fileRefs = append(fileRefs, fpath)
	}

	fullPrompt := prompt
	if len(imageRefs) > 0 {
		if fullPrompt == "" {
			fullPrompt = "Please analyze the attached image(s)."
		}
		fullPrompt += "\n\n[Attached images saved at: " + strings.Join(imageRefs, ", ") + "]"
	}
	if len(fileRefs) > 0 {
		if fullPrompt == "" {
			fullPrompt = "Please analyze the attached file(s)."
		}
		fullPrompt += "\n\n[Attached files saved at: " + strings.Join(fileRefs, ", ") + "]"
	}

	cleanup := func() {
		for _, fpath := range staged {
			_ = os.Remove(fpath)
		}
	}
	return fullPrompt, cleanup
}
