package search

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/vision"
)

func isImageData(data []byte) bool {
	switch vision.DetectImageMIME("", data) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

func (t *ReadFileTool) readImage(workDir, inputPath string) (agentsdk.ToolResult, error) {
	root, path, err := t.resolveImagePath(workDir, inputPath)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	data, err := readImageNoFollow(root, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return agentsdk.ToolResult{Content: notFoundMessage(workDir, inputPath), IsError: true}, nil
		}
		return agentsdk.ToolResult{}, err
	}
	img, err := vision.PrepareImage(data)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to read image %s: %v", inputPath, err), IsError: true}, nil
	}
	display := path
	if rel, ok := relativizeToWorkdir(workDir, path); ok {
		display = rel
	}
	size := fmt.Sprintf("%dx%d, %s", img.Width, img.Height, img.MediaType)
	if img.Resized() {
		size += fmt.Sprintf("; downscaled from %dx%d", img.OriginalWidth, img.OriginalHeight)
	}
	return agentsdk.ToolResult{
		Content: fmt.Sprintf("Image file %s (%s) attached.", display, size),
		Images: []agentsdk.ImageAttachment{{
			MediaType: img.MediaType,
			Data:      base64.StdEncoding.EncodeToString(img.Data),
			Detail:    "high",
		}},
	}, nil
}

// resolveImagePath returns the confinement root and resolved path for an
// image read. Relative paths always resolve against the workspace; allowed
// image dirs only authorize absolute paths they contain.
func (t *ReadFileTool) resolveImagePath(workDir, inputPath string) (string, string, error) {
	path, workspaceErr := workspacePath(workDir, inputPath)
	if workspaceErr == nil {
		return workDir, path, nil
	}
	clean := strings.TrimSpace(inputPath)
	if !filepath.IsAbs(clean) {
		return "", "", workspaceErr
	}
	for _, dir := range t.AllowedImageDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if path, err := pathutil.ResolveWorkspace(dir, clean); err == nil {
			return dir, path, nil
		}
	}
	return "", "", workspaceErr
}

func readImageNoFollow(root, path string) ([]byte, error) {
	f, err := pathutil.OpenInWorkspace(root, path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if err := pathutil.RequireSingleLink(info); err != nil {
		return nil, fmt.Errorf("refusing image read of %s: %w", path, err)
	}
	if info.Size() > vision.MaxImageFileSize {
		return nil, fmt.Errorf("image too large (%d bytes, max %d)", info.Size(), vision.MaxImageFileSize)
	}
	data, err := io.ReadAll(io.LimitReader(f, vision.MaxImageFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > vision.MaxImageFileSize {
		return nil, fmt.Errorf("image too large (> %d bytes)", vision.MaxImageFileSize)
	}
	return data, nil
}
