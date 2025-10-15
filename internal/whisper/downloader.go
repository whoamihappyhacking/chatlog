package whisper

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// DefaultModel is the fallback whisper.cpp model we ensure is present.
const DefaultModel = "ggml-tiny.bin"

// DefaultBaseURL is the upstream location for official whisper.cpp models.
const DefaultBaseURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/"

// DownloadResult describes the state of the ensured model file.
type DownloadResult struct {
	Path    string
	Existed bool
}

// Downloader retrieves whisper.cpp models into a local cache directory.
type Downloader struct {
	dest    string
	baseURL string
	client  *http.Client
}

// NewDownloader initialises a Downloader targeting the provided destination directory.
func NewDownloader(dest string) *Downloader {
	return &Downloader{
		dest:    dest,
		baseURL: DefaultBaseURL,
		client: &http.Client{
			Timeout: 30 * time.Minute,
		},
	}
}

// EnsureModel guarantees the named model exists locally and returns its location.
func (d *Downloader) EnsureModel(ctx context.Context, modelName string) (DownloadResult, error) {
	if err := os.MkdirAll(d.dest, 0o755); err != nil {
		return DownloadResult{}, err
	}

	localName := normalizeModelName(modelName)
	localPath := filepath.Join(d.dest, localName)

	if info, err := os.Stat(localPath); err == nil && info.Size() > 0 {
		return DownloadResult{Path: localPath, Existed: true}, nil
	}

	url := d.baseURL + localName
	tmpPath := localPath + ".downloading"

	if err := d.download(ctx, url, tmpPath); err != nil {
		return DownloadResult{}, err
	}

	if err := os.Rename(tmpPath, localPath); err != nil {
		return DownloadResult{}, err
	}

	return DownloadResult{Path: localPath, Existed: false}, nil
}

func (d *Downloader) download(ctx context.Context, url, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download model: %s", resp.Status)
	}

	file, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer file.Close()

	written, err := io.Copy(file, resp.Body)
	if err != nil {
		return err
	}

	log.Info().Str("url", url).Str("path", destPath).Int64("bytes", written).Msg("downloaded whisper model")
	return nil
}

func normalizeModelName(name string) string {
	normalized := strings.TrimSpace(name)
	if !strings.HasSuffix(normalized, ".bin") {
		normalized += ".bin"
	}
	if !strings.HasPrefix(normalized, "ggml-") {
		normalized = "ggml-" + normalized
	}
	return normalized
}
