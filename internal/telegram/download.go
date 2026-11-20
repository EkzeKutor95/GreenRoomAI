package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
)

// MaxDownloadBytes bounds both Telegram metadata and the actual streamed body.
const MaxDownloadBytes = 10 << 20

// DownloadFile obtains getFile metadata and downloads bytes into bounded memory.
// It never writes attachments or token-bearing URLs to disk or diagnostics.
func (c *Client) DownloadFile(ctx context.Context, fileID string) ([]byte, error) {
	if !validFileID(fileID) {
		return nil, ErrFileID
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var metadata struct {
		FileID   string `json:"file_id"`
		FileSize int64  `json:"file_size"`
		FilePath string `json:"file_path"`
	}
	if err := c.json(ctx, "getFile", map[string]string{"file_id": fileID}, &metadata); err != nil {
		return nil, fmt.Errorf("resolve Telegram file: %w", err)
	}
	if !validFileID(metadata.FileID) || metadata.FileSize < 0 {
		return nil, fmt.Errorf("file metadata: %w", ErrResponse)
	}
	if metadata.FileSize > MaxDownloadBytes {
		return nil, ErrFileTooLarge
	}
	if !validFilePath(metadata.FilePath) {
		return nil, ErrFilePath
	}
	data, err := c.download(ctx, metadata.FilePath)
	if err != nil {
		return nil, err
	}
	if metadata.FileSize > 0 && int64(len(data)) != metadata.FileSize {
		return nil, fmt.Errorf("downloaded file size differs from metadata: %w", ErrResponse)
	}
	return data, nil
}

func validFileID(fileID string) bool {
	if fileID == "" || len(fileID) > 4096 || strings.IndexFunc(fileID, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}

	return true
}

func validFilePath(filePath string) bool {
	if filePath == "" || len(filePath) > 4096 || strings.HasPrefix(filePath, "/") || strings.ContainsAny(filePath, "\\%?#") || path.Clean(filePath) != filePath {
		return false
	}
	if strings.IndexFunc(filePath, func(r rune) bool { return unicode.IsControl(r) }) >= 0 {
		return false
	}
	parsed, err := url.Parse(filePath)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	for _, segment := range strings.Split(filePath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func (c *Client) download(ctx context.Context, filePath string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.fileBaseURL+filePath, nil)
	if err != nil {
		return nil, fmt.Errorf("prepare file download: %w", ErrFilePath)
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("download Telegram file: %w", ErrTransport)
	}
	defer response.Body.Close() // Read-only network cleanup cannot lose local data.
	if response.StatusCode != http.StatusOK {
		return nil, &APIError{Method: "downloadFile", Code: response.StatusCode}
	}
	if response.ContentLength > MaxDownloadBytes {
		return nil, ErrFileTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxDownloadBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read Telegram file: %w", ErrResponse)
	}
	if len(data) > MaxDownloadBytes {
		return nil, ErrFileTooLarge
	}
	return data, nil
}
