package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/perfect-panel/moment/xray-agent/internal/serverclient"
)

func TestSyncRuntimeGeodataAssetsDownloadsAndVerifies(t *testing.T) {
	dir := t.TempDir()
	body := []byte("geosite-data")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	oldHTTPClient := geodataHTTPClient
	geodataHTTPClient = server.Client()
	t.Cleanup(func() {
		geodataHTTPClient = oldHTTPClient
	})

	err := syncRuntimeGeodataAssets(context.Background(), dir, []serverclient.GeodataAsset{{
		File:   "geosite.dat",
		URL:    server.URL + "/geosite.dat",
		SHA256: sha256Hex(body),
	}})
	if err != nil {
		t.Fatalf("syncRuntimeGeodataAssets() error = %v", err)
	}
	if got := string(mustReadFile(t, filepath.Join(dir, "geosite.dat"))); got != string(body) {
		t.Fatalf("geosite.dat = %q", got)
	}
}

func TestSyncRuntimeGeodataAssetsRejectsInvalidAsset(t *testing.T) {
	err := syncRuntimeGeodataAssets(context.Background(), t.TempDir(), []serverclient.GeodataAsset{{
		File: "../geoip.dat",
		URL:  "https://example.com/geoip.dat",
	}})
	if err == nil {
		t.Fatal("syncRuntimeGeodataAssets() error = nil, want invalid asset")
	}
}

func TestSyncRuntimeGeodataAssetsKeepsExistingMatchingSHA(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "geoip.dat")
	body := []byte("existing")
	if err := os.WriteFile(target, body, 0o644); err != nil {
		t.Fatalf("write existing asset: %v", err)
	}
	err := syncRuntimeGeodataAssets(context.Background(), dir, []serverclient.GeodataAsset{{
		File:   "geoip.dat",
		URL:    "https://127.0.0.1:1/should-not-download",
		SHA256: sha256Hex(body),
	}})
	if err != nil {
		t.Fatalf("syncRuntimeGeodataAssets() error = %v", err)
	}
	if got := string(mustReadFile(t, target)); got != string(body) {
		t.Fatalf("geoip.dat = %q", got)
	}
}
