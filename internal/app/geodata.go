package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/perfect-panel/moment/xray-agent/internal/serverclient"
)

const maxGeodataAssetBytes = 256 << 20

var geodataHTTPClient = &http.Client{Timeout: 2 * time.Minute}

func syncRuntimeGeodataAssets(ctx context.Context, assetDir string, assets []serverclient.GeodataAsset) error {
	if len(assets) == 0 {
		return nil
	}
	if strings.TrimSpace(assetDir) == "" {
		return errors.New("xray geodata asset dir is empty")
	}
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		return fmt.Errorf("create xray geodata asset dir: %w", err)
	}
	for _, asset := range assets {
		if err := syncRuntimeGeodataAsset(ctx, assetDir, asset); err != nil {
			return err
		}
	}
	return nil
}

func syncRuntimeGeodataAsset(ctx context.Context, assetDir string, asset serverclient.GeodataAsset) error {
	file := strings.TrimSpace(asset.File)
	rawURL := strings.TrimSpace(asset.URL)
	sha256Value := strings.ToLower(strings.TrimSpace(asset.SHA256))
	if !geodataFileValid(file) || !geodataURLValid(rawURL) || !geodataSHA256Valid(sha256Value) {
		return errors.New("xray geodata asset is invalid")
	}
	targetPath := filepath.Join(assetDir, file)
	if sha256Value != "" {
		matches, err := fileSHA256Matches(targetPath, sha256Value)
		if err != nil {
			return err
		}
		if matches {
			return nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := geodataHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download xray geodata asset %s: %w", file, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download xray geodata asset %s: status %d", file, resp.StatusCode)
	}
	return writeDownloadedGeodataAsset(targetPath, resp.Body, sha256Value)
}

func writeDownloadedGeodataAsset(targetPath string, source io.Reader, sha256Value string) error {
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(targetPath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(source, maxGeodataAssetBytes+1))
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if written > maxGeodataAssetBytes {
		_ = tmp.Close()
		return errors.New("xray geodata asset is too large")
	}
	if sha256Value != "" && hex.EncodeToString(hash.Sum(nil)) != sha256Value {
		_ = tmp.Close()
		return errors.New("xray geodata asset sha256 mismatch")
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, targetPath)
}

func fileSHA256Matches(path string, expected string) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxGeodataAssetBytes+1)); err != nil {
		return false, err
	}
	return hex.EncodeToString(hash.Sum(nil)) == expected, nil
}

func geodataFileValid(file string) bool {
	file = strings.TrimSpace(file)
	return file != "" && len(file) <= 128 && !strings.Contains(file, "..") && !strings.ContainsAny(file, `/\`)
}

func geodataURLValid(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 2048 {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Host != "" && parsed.Scheme == "https"
}

func geodataSHA256Valid(value string) bool {
	if value == "" {
		return true
	}
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
