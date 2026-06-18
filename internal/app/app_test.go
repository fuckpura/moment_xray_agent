package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/perfect-panel/moment/xray-agent/internal/config"
	"github.com/perfect-panel/moment/xray-agent/internal/serverclient"
	xrayruntime "github.com/perfect-panel/moment/xray-agent/internal/xray"
)

func TestPullRuntimeConfigWritesCurrentAndLastGood(t *testing.T) {
	dir := t.TempDir()
	xrayJSON := []byte(`{"inbounds":[],"outbounds":[{"protocol":"freedom","tag":"direct"}]}`)
	assetData := []byte("geoip-data")
	assetHash := sha256Hex(assetData)
	assetServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(assetData)
	}))
	defer assetServer.Close()
	oldHTTPClient := geodataHTTPClient
	geodataHTTPClient = assetServer.Client()
	t.Cleanup(func() {
		geodataHTTPClient = oldHTTPClient
	})
	app := testApp(dir, &fakeRuntimeClient{config: serverclient.RuntimeConfig{
		Version:  "v1",
		XrayJSON: xrayJSON,
		GeodataAssets: []serverclient.GeodataAsset{
			{File: "geoip.dat", URL: assetServer.URL + "/geoip.dat", SHA256: assetHash},
		},
	}})

	if err := app.pullRuntimeConfig(context.Background()); err != nil {
		t.Fatalf("pullRuntimeConfig() error = %v", err)
	}
	current := mustReadFile(t, filepath.Join(dir, "current.json"))
	lastGood := mustReadFile(t, filepath.Join(dir, "last_good.json"))
	if string(current) != string(xrayJSON) || string(lastGood) != string(xrayJSON) || app.runtimeVersion != "v1" {
		t.Fatalf("current=%s last_good=%s version=%q", current, lastGood, app.runtimeVersion)
	}
	if got := string(mustReadFile(t, filepath.Join(dir, "assets", "geoip.dat"))); got != string(assetData) {
		t.Fatalf("geoip.dat = %q", got)
	}
}

func TestPullRuntimeConfigReportsStatusAfterApply(t *testing.T) {
	dir := t.TempDir()
	client := &fakeRuntimeClient{config: serverclient.RuntimeConfig{
		Version:  "v1",
		XrayJSON: []byte(`{"inbounds":[],"outbounds":[{"protocol":"freedom","tag":"direct"}]}`),
	}}
	app := testApp(dir, client)
	app.statusCollector = fakeStatusCollector{report: serverclient.StatusReport{CPUPercent: 10}}

	if err := app.pullRuntimeConfig(context.Background()); err != nil {
		t.Fatalf("pullRuntimeConfig() error = %v", err)
	}
	if client.statusCalls != 1 || client.status.CPUPercent != 10 {
		t.Fatalf("status calls=%d status=%+v", client.statusCalls, client.status)
	}
	if client.status.DesiredConfigVersion != "v1" || client.status.PulledConfigVersion != "v1" || client.status.AppliedConfigVersion != "v1" || client.status.ApplyState != serverclient.ApplyStateApplied || client.status.Health != serverclient.HealthHealthy {
		t.Fatalf("runtime status = %+v", client.status)
	}
}

func TestPullRuntimeConfigReportsFailedApply(t *testing.T) {
	dir := t.TempDir()
	client := &fakeRuntimeClient{config: serverclient.RuntimeConfig{
		Version:  "bad-v1",
		XrayJSON: []byte(`{"inbounds":[],"outbounds":[]}`),
	}}
	app := testApp(dir, client)
	app.xray = &fakeXrayRuntime{err: errors.New("xray config test failed")}
	app.statusCollector = fakeStatusCollector{report: serverclient.StatusReport{CPUPercent: 10}}

	if err := app.pullRuntimeConfig(context.Background()); err == nil {
		t.Fatalf("pullRuntimeConfig() error is nil, want xray failure")
	}
	if client.statusCalls != 1 {
		t.Fatalf("status calls = %d", client.statusCalls)
	}
	if client.status.DesiredConfigVersion != "bad-v1" || client.status.ApplyState != serverclient.ApplyStateFailed || client.status.Health != serverclient.HealthError || !strings.Contains(client.status.LastError, "xray config test failed") {
		t.Fatalf("runtime status = %+v", client.status)
	}
}

func TestPullRuntimeConfigRejectsInvalidJSONWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "current.json")
	original := []byte(`{"inbounds":[]}`)
	if err := os.WriteFile(currentPath, original, 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	app := testApp(dir, &fakeRuntimeClient{config: serverclient.RuntimeConfig{Version: "bad", XrayJSON: []byte(`{`)}})

	if err := app.pullRuntimeConfig(context.Background()); err == nil {
		t.Fatalf("pullRuntimeConfig() error is nil, want invalid json")
	}
	current := mustReadFile(t, currentPath)
	if string(current) != string(original) {
		t.Fatalf("current overwritten with %s", current)
	}
}

func TestPullRuntimeUsersWritesSnapshotAndKnownVersion(t *testing.T) {
	dir := t.TempDir()
	users := serverclient.RuntimeUsers{
		Version: "users-v1",
		Users: []serverclient.RuntimeUser{
			{
				SubscriptionID:  11,
				Email:           "user@example.com",
				UUID:            "11111111-1111-4111-8111-111111111111",
				Password:        "runtime-password",
				SpeedLimitBPS:   1024,
				Enabled:         true,
				ExpiredAtUnixMs: 1800000000000,
			},
		},
	}
	client := &fakeRuntimeClient{users: users}
	app := testApp(dir, client)

	if err := app.pullRuntimeUsers(context.Background()); err != nil {
		t.Fatalf("pullRuntimeUsers() error = %v", err)
	}
	snapshot := string(mustReadFile(t, filepath.Join(dir, "users_snapshot.json")))
	for _, want := range []string{
		`"version": "users-v1"`,
		`"subscription_id": 11`,
		`"email": "user@example.com"`,
		`"uuid": "11111111-1111-4111-8111-111111111111"`,
		`"password": "runtime-password"`,
		`"speed_limit_bps": 1024`,
		`"expired_at_unix_ms": 1800000000000`,
	} {
		if !strings.Contains(snapshot, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, snapshot)
		}
	}
	if app.usersVersion != "users-v1" || client.knownVersion != "" {
		t.Fatalf("version=%q known=%q", app.usersVersion, client.knownVersion)
	}

	if err := app.pullRuntimeUsers(context.Background()); err != nil {
		t.Fatalf("second pullRuntimeUsers() error = %v", err)
	}
	if client.knownVersion != "users-v1" {
		t.Fatalf("known version = %q", client.knownVersion)
	}
}

func TestPullRuntimeUsersReportsStatusAfterApply(t *testing.T) {
	dir := t.TempDir()
	client := &fakeRuntimeClient{users: serverclient.RuntimeUsers{
		Version: "users-v1",
		Users: []serverclient.RuntimeUser{{
			SubscriptionID:  11,
			Email:           "user@example.com",
			UUID:            "11111111-1111-4111-8111-111111111111",
			Password:        "runtime-password",
			Enabled:         true,
			ExpiredAtUnixMs: time.Now().Add(time.Hour).UnixMilli(),
		}},
	}}
	app := testApp(dir, client)
	app.runtimeConfigJSON = []byte(`{"inbounds":[{"protocol":"trojan","settings":{"clients":[{"password":"template"}]}}],"outbounds":[]}`)
	app.statusCollector = fakeStatusCollector{report: serverclient.StatusReport{MemoryPercent: 21}}

	if err := app.pullRuntimeUsers(context.Background()); err != nil {
		t.Fatalf("pullRuntimeUsers() error = %v", err)
	}
	if client.statusCalls != 1 || client.status.MemoryPercent != 21 {
		t.Fatalf("status calls=%d status=%+v", client.statusCalls, client.status)
	}
}

func TestPullRuntimeUsersHotSyncsWhenRuntimeIsRunning(t *testing.T) {
	dir := t.TempDir()
	client := &fakeRuntimeClient{users: serverclient.RuntimeUsers{
		Version: "users-v2",
		Users: []serverclient.RuntimeUser{{
			SubscriptionID:  12,
			Email:           "next@example.com",
			UUID:            "22222222-2222-4222-8222-222222222222",
			Password:        "next-password",
			Enabled:         true,
			ExpiredAtUnixMs: time.Now().Add(time.Hour).UnixMilli(),
		}},
	}}
	xray := &fakeXrayRuntime{running: true}
	app := testApp(dir, client)
	app.xray = xray
	app.runtimeConfigJSON = []byte(`{
		"inbounds": [{
			"tag": "vision",
			"protocol": "vless",
			"settings": {
				"clients": [{"id":"template","email":"template","flow":"xtls-rprx-vision"}],
				"decryption": "none"
			}
		}],
		"outbounds": [{"protocol":"freedom","tag":"direct"}]
	}`)
	app.runtimeUsers = []serverclient.RuntimeUser{{
		SubscriptionID:  11,
		Email:           "old@example.com",
		UUID:            "11111111-1111-4111-8111-111111111111",
		Password:        "old-password",
		Enabled:         true,
		ExpiredAtUnixMs: time.Now().Add(time.Hour).UnixMilli(),
	}}

	if err := app.pullRuntimeUsers(context.Background()); err != nil {
		t.Fatalf("pullRuntimeUsers() error = %v", err)
	}
	if xray.syncCount != 1 || xray.applyCount != 0 {
		t.Fatalf("sync=%d apply=%d, want hot sync without full apply", xray.syncCount, xray.applyCount)
	}
	config := mustDecodeRuntimeConfig(t, xray.lastSyncedConfig)
	clientObject := config["inbounds"].([]any)[0].(map[string]any)["settings"].(map[string]any)["clients"].([]any)[0].(map[string]any)
	if clientObject["email"] != "u_12" || clientObject["id"] != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("hot sync client = %+v", clientObject)
	}
}

func TestPullRuntimeUsersFallsBackToFullApplyWhenHotSyncFails(t *testing.T) {
	dir := t.TempDir()
	client := &fakeRuntimeClient{users: serverclient.RuntimeUsers{
		Version: "users-v2",
		Users: []serverclient.RuntimeUser{{
			SubscriptionID:  12,
			Email:           "next@example.com",
			UUID:            "22222222-2222-4222-8222-222222222222",
			Password:        "next-password",
			Enabled:         true,
			ExpiredAtUnixMs: time.Now().Add(time.Hour).UnixMilli(),
		}},
	}}
	xray := &fakeXrayRuntime{running: true, syncErr: errors.New("handler service unavailable")}
	app := testApp(dir, client)
	app.xray = xray
	app.runtimeConfigJSON = []byte(`{
		"inbounds": [{
			"tag": "vision",
			"protocol": "vless",
			"settings": {
				"clients": [{"id":"template","email":"template"}],
				"decryption": "none"
			}
		}],
		"outbounds": [{"protocol":"freedom","tag":"direct"}]
	}`)

	if err := app.pullRuntimeUsers(context.Background()); err != nil {
		t.Fatalf("pullRuntimeUsers() error = %v", err)
	}
	if xray.syncCount != 1 || xray.applyCount != 1 {
		t.Fatalf("sync=%d apply=%d, want hot sync then full apply", xray.syncCount, xray.applyCount)
	}
}

func TestPullRuntimeUsersRendersEffectiveXrayConfig(t *testing.T) {
	dir := t.TempDir()
	xrayJSON := []byte(`{
		"inbounds": [{
			"tag": "vision",
			"protocol": "vless",
			"settings": {
				"clients": [{"id":"template","email":"template","flow":"xtls-rprx-vision"}],
				"decryption": "none"
			}
		}],
		"outbounds": [{"protocol":"freedom","tag":"direct"}]
	}`)
	users := serverclient.RuntimeUsers{
		Version: "users-v1",
		Users: []serverclient.RuntimeUser{{
			SubscriptionID:  11,
			Email:           "user@example.com",
			UUID:            "11111111-1111-4111-8111-111111111111",
			Password:        "runtime-password",
			Enabled:         true,
			ExpiredAtUnixMs: time.Now().Add(time.Hour).UnixMilli(),
		}},
	}
	app := testApp(dir, &fakeRuntimeClient{
		config: serverclient.RuntimeConfig{Version: "config-v1", XrayJSON: xrayJSON},
		users:  users,
	})

	if err := app.pullRuntimeConfig(context.Background()); err != nil {
		t.Fatalf("pullRuntimeConfig() error = %v", err)
	}
	if err := app.pullRuntimeUsers(context.Background()); err != nil {
		t.Fatalf("pullRuntimeUsers() error = %v", err)
	}
	config := mustDecodeRuntimeConfig(t, mustReadFile(t, filepath.Join(dir, "current.json")))
	clients := config["inbounds"].([]any)[0].(map[string]any)["settings"].(map[string]any)["clients"].([]any)
	if len(clients) != 1 {
		t.Fatalf("clients = %+v", clients)
	}
	client := clients[0].(map[string]any)
	if client["id"] != "11111111-1111-4111-8111-111111111111" || client["email"] != "u_11" || client["flow"] != "xtls-rprx-vision" {
		t.Fatalf("client = %+v", client)
	}
}

func TestWriteEffectiveRuntimeConfigUsesXrayRuntimeAndInjectsLog(t *testing.T) {
	dir := t.TempDir()
	app := testApp(dir, &fakeRuntimeClient{})
	app.xray = &fakeXrayRuntime{}
	app.cfg.Xray.InjectAPI = true
	app.cfg.Xray.APIPort = "10085"
	app.cfg.Log.XrayAccessPath = "/var/log/moment/xray-access.log"
	app.cfg.Log.XrayErrorPath = "/var/log/moment/xray-error.log"
	app.cfg.Log.XrayLevel = "warn"
	app.runtimeConfigJSON = []byte(`{
		"inbounds": [{
			"protocol": "trojan",
			"settings": {"clients": [{"password":"template"}]}
		}],
		"outbounds": [{"protocol":"freedom","tag":"direct"}]
	}`)
	app.runtimeUsers = []serverclient.RuntimeUser{{
		SubscriptionID:  11,
		Email:           "user@example.com",
		UUID:            "11111111-1111-4111-8111-111111111111",
		Password:        "runtime-password",
		Enabled:         true,
		ExpiredAtUnixMs: time.Now().Add(time.Hour).UnixMilli(),
	}}

	if err := app.writeEffectiveRuntimeConfig(context.Background(), time.Now()); err != nil {
		t.Fatalf("writeEffectiveRuntimeConfig() error = %v", err)
	}
	xray := app.xray.(*fakeXrayRuntime)
	if xray.applyCount != 1 {
		t.Fatalf("applyCount = %d", xray.applyCount)
	}
	config := mustDecodeRuntimeConfig(t, xray.lastConfig)
	logObject := config["log"].(map[string]any)
	if logObject["access"] != "/var/log/moment/xray-access.log" || logObject["error"] != "/var/log/moment/xray-error.log" || logObject["loglevel"] != "warning" {
		t.Fatalf("log = %+v", logObject)
	}
	if _, ok := config["stats"].(map[string]any); !ok {
		t.Fatalf("stats missing: %+v", config)
	}
	api := config["api"].(map[string]any)
	if api["tag"] != "api" || len(api["services"].([]any)) != 3 {
		t.Fatalf("api = %+v", api)
	}
	inbounds := config["inbounds"].([]any)
	apiInbound := inbounds[len(inbounds)-1].(map[string]any)
	if apiInbound["tag"] != "api" || apiInbound["port"] != float64(10085) {
		t.Fatalf("api inbound = %+v", apiInbound)
	}
	client := config["inbounds"].([]any)[0].(map[string]any)["settings"].(map[string]any)["clients"].([]any)[0].(map[string]any)
	if client["password"] != "runtime-password" || client["email"] != "u_11" {
		t.Fatalf("client = %+v", client)
	}
	rules := config["routing"].(map[string]any)["rules"].([]any)
	firstRule := rules[0].(map[string]any)
	if firstRule["outboundTag"] != "api" {
		t.Fatalf("first routing rule = %+v", firstRule)
	}
}

func TestPullRuntimeConfigStartsCurrentWhenVersionUnchangedAndXrayStopped(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "current.json")
	if err := os.WriteFile(currentPath, []byte(`{"inbounds":[],"outbounds":[]}`), 0o600); err != nil {
		t.Fatalf("write current: %v", err)
	}
	xray := &fakeXrayRuntime{running: false}
	app := testApp(dir, &fakeRuntimeClient{
		config: serverclient.RuntimeConfig{Version: "config-v1", XrayJSON: []byte(`{"inbounds":[],"outbounds":[]}`)},
	})
	app.runtimeVersion = "config-v1"
	app.xray = xray

	if err := app.pullRuntimeConfig(context.Background()); err != nil {
		t.Fatalf("pullRuntimeConfig() error = %v", err)
	}
	if xray.startCount != 1 || xray.applyCount != 0 {
		t.Fatalf("start=%d apply=%d", xray.startCount, xray.applyCount)
	}
}

func TestReportStatusCollectsAndPosts(t *testing.T) {
	dir := t.TempDir()
	client := &fakeRuntimeClient{}
	app := testApp(dir, client)
	app.onlineUsers.Observe([]xrayruntime.TrafficDelta{{SubscriptionID: 11, UploadBytes: 1}}, time.Now().UTC())
	app.statusCollector = fakeStatusCollector{report: serverclient.StatusReport{
		CPUPercent:    10.5,
		MemoryPercent: 20.25,
		DiskPercent:   30.75,
	}}
	app.xray = &fakeXrayRuntime{}

	if err := app.reportStatus(context.Background()); err != nil {
		t.Fatalf("reportStatus() error = %v", err)
	}
	if client.status.CPUPercent != 10.5 || client.status.MemoryPercent != 20.25 || client.status.DiskPercent != 30.75 {
		t.Fatalf("status = %+v", client.status)
	}
	if client.status.OnlineUserCount != 1 {
		t.Fatalf("online users = %d, want 1", client.status.OnlineUserCount)
	}
	if client.status.AgentVersion != AgentVersion {
		t.Fatalf("agent version = %q, want %q", client.status.AgentVersion, AgentVersion)
	}
	if client.status.XrayVersion != "Xray test" {
		t.Fatalf("xray version = %q, want Xray test", client.status.XrayVersion)
	}
}

func TestReportTrafficCollectsPostsAndCommits(t *testing.T) {
	dir := t.TempDir()
	client := &fakeRuntimeClient{}
	collector := &fakeTrafficCollector{deltas: []xrayruntime.TrafficDelta{
		{SubscriptionID: 11, UploadBytes: 1024, DownloadBytes: 2048},
	}}
	app := testApp(dir, client)
	app.trafficCollector = collector

	if err := app.reportTraffic(context.Background()); err != nil {
		t.Fatalf("reportTraffic() error = %v", err)
	}
	if len(client.traffic) != 1 || client.traffic[0].SubscriptionID != 11 || client.traffic[0].UploadBytes != 1024 || client.traffic[0].DownloadBytes != 2048 {
		t.Fatalf("traffic = %+v", client.traffic)
	}
	if got := app.onlineUsers.Count(time.Now().UTC()); got != 1 {
		t.Fatalf("online users = %d, want 1", got)
	}
	if collector.commitCount != 1 {
		t.Fatalf("commitCount = %d", collector.commitCount)
	}
}

func TestOnlineUserTrackerExpiresInactiveUsers(t *testing.T) {
	tracker := newOnlineUserTracker(time.Minute)
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	tracker.Observe([]xrayruntime.TrafficDelta{
		{SubscriptionID: 11, UploadBytes: 1},
		{SubscriptionID: 12, DownloadBytes: 1},
		{SubscriptionID: 0, DownloadBytes: 1},
	}, now)
	if got := tracker.Count(now.Add(30 * time.Second)); got != 2 {
		t.Fatalf("online users = %d, want 2", got)
	}
	if got := tracker.Count(now.Add(61 * time.Second)); got != 0 {
		t.Fatalf("online users = %d, want 0", got)
	}
}

func TestReportTrafficDoesNotCommitWhenPostFails(t *testing.T) {
	dir := t.TempDir()
	client := &fakeRuntimeClient{err: errors.New("server down")}
	collector := &fakeTrafficCollector{deltas: []xrayruntime.TrafficDelta{
		{SubscriptionID: 11, UploadBytes: 1024},
	}}
	app := testApp(dir, client)
	app.trafficCollector = collector

	if err := app.reportTraffic(context.Background()); err == nil {
		t.Fatalf("reportTraffic() error is nil, want post failure")
	}
	if collector.commitCount != 0 {
		t.Fatalf("commitCount = %d, want 0", collector.commitCount)
	}
}

func TestReportTrafficCommitsEmptyDeltas(t *testing.T) {
	dir := t.TempDir()
	client := &fakeRuntimeClient{}
	collector := &fakeTrafficCollector{}
	app := testApp(dir, client)
	app.trafficCollector = collector

	if err := app.reportTraffic(context.Background()); err != nil {
		t.Fatalf("reportTraffic() error = %v", err)
	}
	if len(client.traffic) != 0 {
		t.Fatalf("traffic = %+v, want empty", client.traffic)
	}
	if collector.commitCount != 1 {
		t.Fatalf("commitCount = %d", collector.commitCount)
	}
}

func TestPullRuntimeUsersRejectsInvalidUserWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	snapshotPath := filepath.Join(dir, "users_snapshot.json")
	original := []byte(`{"version":"old","users":[]}`)
	if err := os.WriteFile(snapshotPath, original, 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	app := testApp(dir, &fakeRuntimeClient{users: serverclient.RuntimeUsers{
		Version: "bad",
		Users:   []serverclient.RuntimeUser{{SubscriptionID: 11, UUID: "", Password: "runtime-password"}},
	}})

	if err := app.pullRuntimeUsers(context.Background()); err == nil {
		t.Fatalf("pullRuntimeUsers() error is nil, want invalid user")
	}
	snapshot := mustReadFile(t, snapshotPath)
	if string(snapshot) != string(original) {
		t.Fatalf("snapshot overwritten with %s", snapshot)
	}
}

func TestInjectRuntimeUsersSupportsProtocolUserShapes(t *testing.T) {
	raw, err := injectRuntimeUsers([]byte(`{
		"inbounds": [
			{"protocol":"hysteria","settings":{"version":2,"users":[{"auth":"template","email":"template","level":2}]}},
			{"protocol":"shadowsocks","settings":{"method":"aes-256-gcm","password":"server-password","users":[{"method":"aes-128-gcm","password":"template"}]}},
			{"protocol":"trojan","settings":{"clients":[{"password":"template","level":1}]}}
		],
		"outbounds": [{"protocol":"freedom","tag":"direct"}]
	}`), []serverclient.RuntimeUser{
		{
			SubscriptionID:  11,
			Email:           "active@example.com",
			UUID:            "11111111-1111-4111-8111-111111111111",
			Password:        "runtime-password",
			Enabled:         true,
			ExpiredAtUnixMs: time.Now().Add(time.Hour).UnixMilli(),
		},
		{
			SubscriptionID:  12,
			Email:           "expired@example.com",
			UUID:            "22222222-2222-4222-8222-222222222222",
			Password:        "expired-password",
			Enabled:         true,
			ExpiredAtUnixMs: time.Now().Add(-time.Hour).UnixMilli(),
		},
	}, time.Now())
	if err != nil {
		t.Fatalf("injectRuntimeUsers() error = %v", err)
	}
	config := mustDecodeRuntimeConfig(t, raw)
	inbounds := config["inbounds"].([]any)
	hysteriaUser := inbounds[0].(map[string]any)["settings"].(map[string]any)["users"].([]any)[0].(map[string]any)
	if hysteriaUser["auth"] != "runtime-password" || hysteriaUser["email"] != "u_11" || hysteriaUser["level"] != float64(2) {
		t.Fatalf("hysteria user = %+v", hysteriaUser)
	}
	ssUser := inbounds[1].(map[string]any)["settings"].(map[string]any)["users"].([]any)[0].(map[string]any)
	if ssUser["password"] != "runtime-password" || ssUser["email"] != "u_11" || ssUser["method"] != "aes-128-gcm" {
		t.Fatalf("shadowsocks user = %+v", ssUser)
	}
	trojanClient := inbounds[2].(map[string]any)["settings"].(map[string]any)["clients"].([]any)[0].(map[string]any)
	if trojanClient["password"] != "runtime-password" || trojanClient["email"] != "u_11" || trojanClient["level"] != float64(1) {
		t.Fatalf("trojan client = %+v", trojanClient)
	}
}

func TestRestoreLastGoodConfigWhenCurrentMissing(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "current.json")
	lastGoodPath := filepath.Join(dir, "last_good.json")
	lastGood := []byte(`{"outbounds":[{"protocol":"freedom","tag":"direct"}]}`)
	if err := os.WriteFile(lastGoodPath, lastGood, 0o600); err != nil {
		t.Fatalf("write last_good: %v", err)
	}

	if err := restoreLastGoodConfig(lastGoodPath, currentPath); err != nil {
		t.Fatalf("restoreLastGoodConfig() error = %v", err)
	}
	current := mustReadFile(t, currentPath)
	if string(current) != string(lastGood) {
		t.Fatalf("current=%s want=%s", current, lastGood)
	}
}

func TestRunRestoresLastGoodAfterInitialPullFailure(t *testing.T) {
	dir := t.TempDir()
	lastGood := []byte(`{"outbounds":[{"protocol":"freedom","tag":"direct"}]}`)
	if err := os.WriteFile(filepath.Join(dir, "last_good.json"), lastGood, 0o600); err != nil {
		t.Fatalf("write last_good: %v", err)
	}
	app := testApp(dir, &fakeRuntimeClient{err: errors.New("network down")})
	app.cfg.Agent.PullInterval = time.Hour
	app.cfg.Agent.StatusInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := app.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	current := mustReadFile(t, filepath.Join(dir, "current.json"))
	if string(current) != string(lastGood) {
		t.Fatalf("current=%s want=%s", current, lastGood)
	}
}

func TestRunRequiresEnrollmentKey(t *testing.T) {
	app := testApp(t.TempDir(), &fakeRuntimeClient{})
	app.cfg.Server.EnrollmentKey = ""

	err := app.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "MOMENT_AGENT_ENROLLMENT_KEY") {
		t.Fatalf("Run() error = %v", err)
	}
}

func testApp(dir string, client runtimeClient) *App {
	return &App{
		cfg: config.Config{
			Server: config.ServerConfig{EnrollmentKey: "test-enrollment-key"},
			Agent:  config.AgentConfig{PullInterval: time.Hour, StatusInterval: time.Hour, TrafficInterval: time.Hour},
			Xray: config.XrayConfig{
				AssetDir:          filepath.Join(dir, "assets"),
				ConfigPath:        filepath.Join(dir, "current.json"),
				LastGoodPath:      filepath.Join(dir, "last_good.json"),
				UsersSnapshotPath: filepath.Join(dir, "users_snapshot.json"),
			},
		},
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		runtimeClient: client,
		applyState:    serverclient.ApplyStatePending,
		agentHealth:   serverclient.HealthHealthy,
		startedAt:     time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		onlineUsers:   newOnlineUserTracker(time.Hour),
	}
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

func mustDecodeRuntimeConfig(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("decode runtime config: %v\n%s", err, raw)
	}
	return config
}

type fakeRuntimeClient struct {
	config       serverclient.RuntimeConfig
	users        serverclient.RuntimeUsers
	err          error
	knownVersion string
	status       serverclient.StatusReport
	statusCalls  int
	traffic      []serverclient.TrafficDelta
}

type fakeStatusCollector struct {
	report serverclient.StatusReport
	err    error
}

type fakeTrafficCollector struct {
	deltas      []xrayruntime.TrafficDelta
	err         error
	commitErr   error
	commitCount int
	closeCount  int
}

type fakeXrayRuntime struct {
	applyCount       int
	syncCount        int
	startCount       int
	stopCount        int
	lastConfig       []byte
	lastSyncedConfig []byte
	err              error
	syncErr          error
	running          bool
	version          string
}

func (x *fakeXrayRuntime) Apply(_ context.Context, configData []byte) error {
	if x.err != nil {
		return x.err
	}
	x.applyCount++
	x.lastConfig = append([]byte(nil), configData...)
	x.running = true
	return nil
}

func (x *fakeXrayRuntime) SyncUsers(_ context.Context, configData []byte) error {
	x.syncCount++
	if x.syncErr != nil {
		return x.syncErr
	}
	x.lastSyncedConfig = append([]byte(nil), configData...)
	return nil
}

func (x *fakeXrayRuntime) Start(context.Context) error {
	if x.err != nil {
		return x.err
	}
	x.startCount++
	x.running = true
	return nil
}

func (x *fakeXrayRuntime) Stop() {
	x.stopCount++
	x.running = false
}

func (x *fakeXrayRuntime) Running() bool {
	return x.running
}

func (x *fakeXrayRuntime) Version(context.Context) (string, error) {
	if x.err != nil {
		return "", x.err
	}
	if x.version != "" {
		return x.version, nil
	}
	return "Xray test", nil
}

func (c *fakeRuntimeClient) GetRuntimeConfig(context.Context) (serverclient.RuntimeConfig, error) {
	if c.err != nil {
		return serverclient.RuntimeConfig{}, c.err
	}
	return c.config, nil
}

func (c *fakeRuntimeClient) GetUsers(_ context.Context, knownVersion string) (serverclient.RuntimeUsers, error) {
	c.knownVersion = knownVersion
	if c.err != nil {
		return serverclient.RuntimeUsers{}, c.err
	}
	if knownVersion != "" && knownVersion == c.users.Version {
		return serverclient.RuntimeUsers{Version: c.users.Version}, nil
	}
	return c.users, nil
}

func (c *fakeRuntimeClient) WatchRuntime(ctx context.Context, _ string, _ func(serverclient.RuntimeChangeEvent) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func (c *fakeRuntimeClient) ReportTraffic(_ context.Context, deltas []serverclient.TrafficDelta) error {
	if c.err != nil {
		return c.err
	}
	c.traffic = append([]serverclient.TrafficDelta(nil), deltas...)
	return nil
}

func (c *fakeRuntimeClient) ReportStatus(_ context.Context, report serverclient.StatusReport) error {
	if c.err != nil {
		return c.err
	}
	c.statusCalls++
	c.status = report
	return nil
}

func (c fakeStatusCollector) Collect(context.Context) (serverclient.StatusReport, error) {
	if c.err != nil {
		return serverclient.StatusReport{}, c.err
	}
	return c.report, nil
}

func (c *fakeTrafficCollector) Collect(context.Context) ([]xrayruntime.TrafficDelta, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.deltas, nil
}

func (c *fakeTrafficCollector) Commit() error {
	if c.commitErr != nil {
		return c.commitErr
	}
	c.commitCount++
	return nil
}

func (c *fakeTrafficCollector) Close() error {
	c.closeCount++
	return nil
}
