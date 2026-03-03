package media

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SaveAttachment downloads a URL and saves it persistently to destDir with a
// timestamped, sanitized filename. Returns the full saved path.
// httpClientMedia is a shared HTTP client with timeout for media downloads.
var httpClientMedia = &http.Client{Timeout: 120 * time.Second}

// maxAttachmentSize is the maximum download size for attachments (100 MB).
const maxAttachmentSize = 100 << 20

// maxDownloadSize is the maximum download size for temporary files (50 MB).
const maxDownloadSize = 50 << 20

func SaveAttachment(url, originalFilename, destDir string) (string, error) {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create attachments dir: %w", err)
	}

	resp, err := httpClientMedia.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to download attachment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	// Sanitize filename
	base := filepath.Base(originalFilename)
	base = strings.ReplaceAll(base, " ", "_")
	// Strip any path traversal attempts
	base = strings.ReplaceAll(base, "..", "")
	if base == "" || base == "." {
		base = "file.bin"
	}

	ts := time.Now().Format("20060102_150405")
	filename := ts + "_" + base
	destPath := filepath.Join(destDir, filename)

	dst, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("failed to create destination file: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, io.LimitReader(resp.Body, maxAttachmentSize)); err != nil {
		os.Remove(destPath)
		return "", fmt.Errorf("failed to write attachment: %w", err)
	}

	return destPath, nil
}

// DownloadFile downloads a URL to a temporary file and returns the path.
// The caller is responsible for removing the file when done.
func DownloadFile(url string, prefix string) (string, error) {
	resp, err := httpClientMedia.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download file: %s", resp.Status)
	}

	// Detect extension from URL
	ext := filepath.Ext(url)
	if ext == "" {
		ext = ".bin"
	}

	tempFile, err := os.CreateTemp("", prefix+"_*"+ext)
	if err != nil {
		return "", err
	}
	defer tempFile.Close()

	if _, err := io.Copy(tempFile, io.LimitReader(resp.Body, maxDownloadSize)); err != nil {
		os.Remove(tempFile.Name())
		return "", err
	}

	return tempFile.Name(), nil
}

// DetectMimeType returns a MIME type based on file extension.
func DetectMimeType(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".ogg"):
		return "audio/ogg"
	case strings.HasSuffix(lower, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(lower, ".wav"):
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

// IsImageContentType checks if a content type represents an image.
func IsImageContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "image/")
}

// IsAudioContentType checks if a content type represents audio.
func IsAudioContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "audio/") ||
		strings.Contains(ct, "ogg") ||
		strings.Contains(ct, "voice")
}

// IsAudioFilename checks if a filename has a voice/audio extension.
func IsAudioFilename(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".ogg") ||
		strings.HasSuffix(lower, ".mp3") ||
		strings.HasSuffix(lower, ".wav") ||
		strings.HasSuffix(lower, ".flac") ||
		strings.HasSuffix(lower, ".m4a") ||
		strings.HasSuffix(lower, ".opus")
}

// IsImageFilename checks if a filename has an image extension.
func IsImageFilename(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".jpg") ||
		strings.HasSuffix(lower, ".jpeg") ||
		strings.HasSuffix(lower, ".png") ||
		strings.HasSuffix(lower, ".gif") ||
		strings.HasSuffix(lower, ".webp")
}
