package xray

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/perfect-panel/moment/xray-agent/internal/config"
)

const minimalXrayConfig = `{
  "inbounds": [],
  "outbounds": [
    {
      "protocol": "freedom",
      "tag": "direct"
    }
  ]
}`

func TestRuntimeApplyValidatesWritesAndStartsEmbeddedCore(t *testing.T) {
	dir := t.TempDir()
	runtime := NewRuntime(config.XrayConfig{
		WorkDir:        dir,
		AssetDir:       filepath.Join(dir, "assets"),
		ConfigPath:     filepath.Join(dir, "current.json"),
		LastGoodPath:   filepath.Join(dir, "last_good.json"),
		ValidateConfig: true,
	}, config.LogConfig{})
	defer runtime.Stop()

	configData := []byte(minimalXrayConfig)
	if err := runtime.Apply(context.Background(), configData); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !runtime.Running() {
		t.Fatalf("runtime should be running after Apply")
	}
	if got := string(mustRead(t, filepath.Join(dir, "current.json"))); got != string(configData) {
		t.Fatalf("current = %s", got)
	}
	if got := string(mustRead(t, filepath.Join(dir, "last_good.json"))); got != string(configData) {
		t.Fatalf("last_good = %s", got)
	}
}

func TestRuntimeApplyRejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	runtime := NewRuntime(config.XrayConfig{
		WorkDir:        dir,
		AssetDir:       filepath.Join(dir, "assets"),
		ConfigPath:     filepath.Join(dir, "current.json"),
		LastGoodPath:   filepath.Join(dir, "last_good.json"),
		ValidateConfig: true,
	}, config.LogConfig{})
	defer runtime.Stop()

	if err := runtime.Apply(context.Background(), []byte(`{"inbounds": [`)); err == nil {
		t.Fatalf("Apply() should reject invalid json")
	}
	if _, err := os.Stat(filepath.Join(dir, "current.json")); !os.IsNotExist(err) {
		t.Fatalf("current config should not be written after validation failure, stat error = %v", err)
	}
}

func TestRuntimeStartFallsBackToLastGood(t *testing.T) {
	dir := t.TempDir()
	lastGoodPath := filepath.Join(dir, "last_good.json")
	if err := os.WriteFile(lastGoodPath, []byte(minimalXrayConfig), 0o600); err != nil {
		t.Fatalf("write last_good: %v", err)
	}
	runtime := NewRuntime(config.XrayConfig{
		WorkDir:      dir,
		AssetDir:     filepath.Join(dir, "assets"),
		ConfigPath:   filepath.Join(dir, "missing-current.json"),
		LastGoodPath: lastGoodPath,
	}, config.LogConfig{})
	defer runtime.Stop()

	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !runtime.Running() {
		t.Fatalf("runtime should be running after last_good fallback")
	}
}

func TestRuntimeStartFallsBackToLastGoodWhenCurrentIsInvalid(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "current.json")
	lastGoodPath := filepath.Join(dir, "last_good.json")
	if err := os.WriteFile(currentPath, []byte(`{"inbounds":[`), 0o600); err != nil {
		t.Fatalf("write current: %v", err)
	}
	if err := os.WriteFile(lastGoodPath, []byte(minimalXrayConfig), 0o600); err != nil {
		t.Fatalf("write last_good: %v", err)
	}
	runtime := NewRuntime(config.XrayConfig{
		WorkDir:      dir,
		AssetDir:     filepath.Join(dir, "assets"),
		ConfigPath:   currentPath,
		LastGoodPath: lastGoodPath,
	}, config.LogConfig{})
	defer runtime.Stop()

	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !runtime.Running() {
		t.Fatalf("runtime should be running after last_good fallback")
	}
	if got := string(mustRead(t, currentPath)); got != minimalXrayConfig {
		t.Fatalf("current should be restored from last_good, got %s", got)
	}
}

func TestRuntimeApplyRestoresCurrentWhenRestartFallsBackToLastGood(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "current.json")
	lastGoodPath := filepath.Join(dir, "last_good.json")
	if err := os.WriteFile(lastGoodPath, []byte(minimalXrayConfig), 0o600); err != nil {
		t.Fatalf("write last_good: %v", err)
	}
	runtime := NewRuntime(config.XrayConfig{
		WorkDir:        dir,
		AssetDir:       filepath.Join(dir, "assets"),
		ConfigPath:     currentPath,
		LastGoodPath:   lastGoodPath,
		ValidateConfig: false,
	}, config.LogConfig{})
	defer runtime.Stop()

	if err := runtime.Apply(context.Background(), []byte(`{"inbounds":[`)); err == nil {
		t.Fatalf("Apply() error is nil, want invalid config failure")
	}
	if !runtime.Running() {
		t.Fatalf("runtime should be running after last_good fallback")
	}
	if got := string(mustRead(t, currentPath)); got != minimalXrayConfig {
		t.Fatalf("current should be restored from last_good, got %s", got)
	}
}

func TestRuntimeVersionReportsEmbeddedDependency(t *testing.T) {
	runtime := NewRuntime(config.XrayConfig{}, config.LogConfig{})
	got, err := runtime.Version(context.Background())
	if err != nil {
		t.Fatalf("Version() error = %v", err)
	}
	if !strings.Contains(got, "xray-core") || !strings.Contains(got, "embedded") {
		t.Fatalf("Version() = %q", got)
	}
}

func TestRuntimeSyncUsersUpdatesRunningInbound(t *testing.T) {
	dir := t.TempDir()
	port := pickFreeTCPPort(t)
	runtime := NewRuntime(config.XrayConfig{
		WorkDir:        dir,
		AssetDir:       filepath.Join(dir, "assets"),
		ConfigPath:     filepath.Join(dir, "current.json"),
		LastGoodPath:   filepath.Join(dir, "last_good.json"),
		ValidateConfig: true,
	}, config.LogConfig{})
	defer runtime.Stop()

	initialConfig := []byte(`{
	  "inbounds": [{
	    "tag": "sync-vless",
	    "listen": "127.0.0.1",
	    "port": ` + strconv.Itoa(port) + `,
	    "protocol": "vless",
	    "settings": {
	      "clients": [{"email":"u_11","id":"11111111-1111-4111-8111-111111111111"}],
	      "decryption": "none"
	    }
	  }],
	  "outbounds": [{"protocol":"freedom","tag":"direct"}]
	}`)
	targetConfig := []byte(`{
	  "inbounds": [{
	    "tag": "sync-vless",
	    "listen": "127.0.0.1",
	    "port": ` + strconv.Itoa(port) + `,
	    "protocol": "vless",
	    "settings": {
	      "clients": [{"email":"u_12","id":"22222222-2222-4222-8222-222222222222"}],
	      "decryption": "none"
	    }
	  }],
	  "outbounds": [{"protocol":"freedom","tag":"direct"}]
	}`)

	if err := runtime.Apply(context.Background(), initialConfig); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := runtime.SyncUsers(context.Background(), targetConfig); err != nil {
		t.Fatalf("SyncUsers() error = %v", err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	manager, err := runtime.inboundManagerLocked()
	if err != nil {
		t.Fatalf("inboundManagerLocked() error = %v", err)
	}
	userManager, err := runtimeUserManager(context.Background(), manager, "sync-vless")
	if err != nil {
		t.Fatalf("runtimeUserManager() error = %v", err)
	}
	if user := userManager.GetUser(context.Background(), "u_11"); user != nil {
		t.Fatalf("old user still exists: %+v", user)
	}
	if user := userManager.GetUser(context.Background(), "u_12"); user == nil {
		t.Fatalf("new user was not added")
	}
	if got := string(mustRead(t, filepath.Join(dir, "current.json"))); got != string(targetConfig) {
		t.Fatalf("current config was not updated:\n%s", got)
	}
}

func TestExtractDynamicInboundUsersSupportsRuntimeProtocols(t *testing.T) {
	users, err := extractDynamicInboundUsers([]byte(`{
	  "inbounds": [
	    {
	      "tag": "vless-in",
	      "listen": "127.0.0.1",
	      "port": 21001,
	      "protocol": "vless",
	      "settings": {
	        "clients": [{"email":"u_11","id":"11111111-1111-4111-8111-111111111111","flow":"xtls-rprx-vision"}],
	        "decryption": "none"
	      }
	    },
	    {
	      "tag": "trojan-in",
	      "listen": "127.0.0.1",
	      "port": 21002,
	      "protocol": "trojan",
	      "settings": {"clients": [{"email":"u_12","password":"secret"}]}
	    },
	    {
	      "tag": "direct",
	      "listen": "127.0.0.1",
	      "port": 21003,
	      "protocol": "dokodemo-door",
	      "settings": {"address": "127.0.0.1"}
	    }
	  ],
	  "outbounds": [{"protocol":"freedom","tag":"direct"}]
	}`))
	if err != nil {
		t.Fatalf("extractDynamicInboundUsers() error = %v", err)
	}
	if !users.supported {
		t.Fatalf("users should be hot-syncable")
	}
	if users.byTag["vless-in"]["u_11"].email != "u_11" || users.byTag["trojan-in"]["u_12"].email != "u_12" {
		t.Fatalf("users = %+v", users.byTag)
	}
	if _, ok := users.byTag["direct"]; ok {
		t.Fatalf("non user-managed inbound should not be included: %+v", users.byTag)
	}
}

func TestExtractDynamicInboundUsersRejectsUntaggedManagedInbound(t *testing.T) {
	_, err := extractDynamicInboundUsers([]byte(`{
	  "inbounds": [{
	    "listen": "127.0.0.1",
	    "port": 21004,
	    "protocol": "vless",
	    "settings": {
	      "clients": [{"email":"u_11","id":"11111111-1111-4111-8111-111111111111"}],
	      "decryption": "none"
	    }
	  }],
	  "outbounds": [{"protocol":"freedom","tag":"direct"}]
	}`))
	if err == nil || !strings.Contains(err.Error(), "must have a tag") {
		t.Fatalf("extractDynamicInboundUsers() error = %v, want missing tag error", err)
	}
}

func TestExtractDynamicInboundUsersTreatsEmptyManagedInboundAsSupported(t *testing.T) {
	users, err := extractDynamicInboundUsers([]byte(`{
	  "inbounds": [{
	    "tag": "vless-in",
	    "listen": "127.0.0.1",
	    "port": 21006,
	    "protocol": "vless",
	    "settings": {
	      "clients": [],
	      "decryption": "none"
	    }
	  }],
	  "outbounds": [{"protocol":"freedom","tag":"direct"}]
	}`))
	if err != nil {
		t.Fatalf("extractDynamicInboundUsers() error = %v", err)
	}
	if !users.supported {
		t.Fatalf("empty managed inbound should still be hot-syncable")
	}
	if got := len(users.byTag["vless-in"]); got != 0 {
		t.Fatalf("users = %d, want 0", got)
	}
}

func TestDiffDynamicInboundUsersDetectsAddRemoveAndAccountChange(t *testing.T) {
	current := mustExtractDynamicUsers(t, []byte(`{
	  "inbounds": [{
	    "tag": "vless-in",
	    "listen": "127.0.0.1",
	    "port": 21005,
	    "protocol": "vless",
	    "settings": {
	      "clients": [
	        {"email":"u_11","id":"11111111-1111-4111-8111-111111111111"},
	        {"email":"u_12","id":"22222222-2222-4222-8222-222222222222"}
	      ],
	      "decryption": "none"
	    }
	  }],
	  "outbounds": [{"protocol":"freedom","tag":"direct"}]
	}`))
	target := mustExtractDynamicUsers(t, []byte(`{
	  "inbounds": [{
	    "tag": "vless-in",
	    "listen": "127.0.0.1",
	    "port": 21005,
	    "protocol": "vless",
	    "settings": {
	      "clients": [
	        {"email":"u_11","id":"33333333-3333-4333-8333-333333333333"},
	        {"email":"u_13","id":"44444444-4444-4444-8444-444444444444"}
	      ],
	      "decryption": "none"
	    }
	  }],
	  "outbounds": [{"protocol":"freedom","tag":"direct"}]
	}`))
	changes := diffDynamicInboundUsers(current, target)
	if len(changes.remove) != 2 || len(changes.add) != 2 {
		t.Fatalf("changes = %+v", changes)
	}
	if changes.remove[0].email != "u_11" || changes.remove[1].email != "u_12" {
		t.Fatalf("remove = %+v", changes.remove)
	}
	if changes.add[0].user.Email != "u_11" || changes.add[1].user.Email != "u_13" {
		t.Fatalf("add = %+v", changes.add)
	}
}

func mustExtractDynamicUsers(t *testing.T, raw []byte) dynamicInboundUsers {
	t.Helper()
	users, err := extractDynamicInboundUsers(raw)
	if err != nil {
		t.Fatalf("extractDynamicInboundUsers() error = %v", err)
	}
	return users
}

func pickFreeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on random tcp port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			return raw
		}
		if time.Now().After(deadline) {
			t.Fatalf("read %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
