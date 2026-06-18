package serverclient

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetRuntimeConfigUsesConnectJSONAndDecodesBytes(t *testing.T) {
	xrayJSON := []byte(`{"inbounds":[],"outbounds":[{"protocol":"freedom","tag":"direct"}]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != getRuntimeConfigPath {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Connect-Protocol-Version") != "1" {
			t.Fatalf("method/headers = %s content-type=%q connect=%q", r.Method, r.Header.Get("Content-Type"), r.Header.Get("Connect-Protocol-Version"))
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		var request getRuntimeConfigRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.ServerID != "42" {
			t.Fatalf("server id = %q", request.ServerID)
		}
		_ = json.NewEncoder(w).Encode(getRuntimeConfigResponse{
			Version:  "v1",
			XrayJSON: base64.StdEncoding.EncodeToString(xrayJSON),
			GeodataAssets: geodataAssetResponses{
				{File: "geoip.dat", URL: "https://example.com/geoip.dat", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			},
		})
	}))
	defer server.Close()

	client, err := New(server.URL+"/", "42", "secret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	config, err := client.GetRuntimeConfig(t.Context())
	if err != nil {
		t.Fatalf("GetRuntimeConfig() error = %v", err)
	}
	if config.Version != "v1" || string(config.XrayJSON) != string(xrayJSON) {
		t.Fatalf("config = %+v json=%s", config, string(config.XrayJSON))
	}
	if len(config.GeodataAssets) != 1 || config.GeodataAssets[0].File != "geoip.dat" || config.GeodataAssets[0].URL != "https://example.com/geoip.dat" || config.GeodataAssets[0].SHA256 == "" {
		t.Fatalf("geodata assets = %+v", config.GeodataAssets)
	}
}

func TestGetUsersUsesConnectJSONAndDecodesProtoJSONNumbers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != getUsersPath {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Connect-Protocol-Version") != "1" {
			t.Fatalf("method/headers = %s content-type=%q connect=%q", r.Method, r.Header.Get("Content-Type"), r.Header.Get("Connect-Protocol-Version"))
		}
		var request getUsersRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.ServerID != "42" || request.KnownVersion != "old" {
			t.Fatalf("request = %+v", request)
		}
		_, _ = w.Write([]byte(`{
			"version":"users-v1",
			"users":[{
				"subscriptionId":"11",
				"email":"user@example.com",
				"uuid":"11111111-1111-4111-8111-111111111111",
				"password":"runtime-password",
				"speedLimitBps":"1024",
				"enabled":true,
				"expiredAtUnixMs":"1800000000000"
			}]
		}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "42", "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	users, err := client.GetUsers(t.Context(), "old")
	if err != nil {
		t.Fatalf("GetUsers() error = %v", err)
	}
	if users.Version != "users-v1" || len(users.Users) != 1 {
		t.Fatalf("users = %+v", users)
	}
	user := users.Users[0]
	if user.SubscriptionID != 11 || user.SpeedLimitBPS != 1024 || user.ExpiredAtUnixMs != 1800000000000 || !user.Enabled || user.UUID == "" || user.Password == "" {
		t.Fatalf("user = %+v", user)
	}
}

func TestReportTrafficUsesConnectJSONAndProtoJSONNumbers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != reportTrafficPath {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Connect-Protocol-Version") != "1" {
			t.Fatalf("method/headers = %s content-type=%q connect=%q", r.Method, r.Header.Get("Content-Type"), r.Header.Get("Connect-Protocol-Version"))
		}
		var request reportTrafficRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.ServerID != "42" || len(request.UserDeltas) != 1 {
			t.Fatalf("request = %+v", request)
		}
		delta := request.UserDeltas[0]
		if delta.SubscriptionID != "11" || delta.Delta.Upload != "1024" || delta.Delta.Download != "2048" {
			t.Fatalf("delta = %+v", delta)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "42", "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = client.ReportTraffic(t.Context(), []TrafficDelta{
		{SubscriptionID: 0, UploadBytes: 99, DownloadBytes: 99},
		{SubscriptionID: 11, UploadBytes: 1024, DownloadBytes: 2048},
		{SubscriptionID: 12},
	})
	if err != nil {
		t.Fatalf("ReportTraffic() error = %v", err)
	}
}

func TestReportStatusUsesConnectJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != reportStatusPath {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var request reportStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.ServerID != "42" || request.CPUPercent != 10.5 || request.MemoryPercent != 20.25 || request.DiskPercent != 30.75 || request.OnlineUserCount != "3" {
			t.Fatalf("request = %+v", request)
		}
		if request.DesiredConfigVersion != "want-v1" || request.PulledConfigVersion != "want-v1" || request.AppliedConfigVersion != "want-v1" || request.ApplyState != "RUNTIME_APPLY_STATE_APPLIED" || request.Health != "AGENT_HEALTH_HEALTHY" || request.AgentVersion != "moment-agent 0.4.0" || request.XrayVersion != "Xray 25.1.1" || request.StartedAtUnixMs != "1800000000000" {
			t.Fatalf("runtime request = %+v", request)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "42", "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := client.ReportStatus(t.Context(), StatusReport{
		CPUPercent:           10.5,
		MemoryPercent:        20.25,
		DiskPercent:          30.75,
		OnlineUserCount:      3,
		DesiredConfigVersion: "want-v1",
		PulledConfigVersion:  "want-v1",
		AppliedConfigVersion: "want-v1",
		ApplyState:           ApplyStateApplied,
		AgentVersion:         "moment-agent 0.4.0",
		Health:               HealthHealthy,
		XrayVersion:          "Xray 25.1.1",
		StartedAtUnixMs:      1800000000000,
	}); err != nil {
		t.Fatalf("ReportStatus() error = %v", err)
	}
}

func TestWatchRuntimeUsesConnectJSONStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != watchRuntimePath {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/connect+json" || r.Header.Get("Connect-Protocol-Version") != "1" {
			t.Fatalf("method/headers = %s accept=%q connect=%q", r.Method, r.Header.Get("Accept"), r.Header.Get("Connect-Protocol-Version"))
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		var request watchRuntimeRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.ServerID != "42" || request.KnownConfigVersion != "old-version" {
			t.Fatalf("request = %+v", request)
		}
		w.Header().Set("Content-Type", "application/connect+json")
		writeConnectJSONFrame(t, w, []byte(`{"eventType":"runtime.config_changed","configVersion":"new-version","occurredAtUnixMs":"1800000000000"}`), 0)
		writeConnectJSONFrame(t, w, []byte(`{}`), 0x02)
	}))
	defer server.Close()

	client, err := New(server.URL, "42", "secret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	var events []RuntimeChangeEvent
	if err := client.WatchRuntime(t.Context(), "old-version", func(event RuntimeChangeEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatalf("WatchRuntime() error = %v", err)
	}
	if len(events) != 1 || events[0].EventType != "runtime.config_changed" || events[0].ConfigVersion != "new-version" || events[0].OccurredAtUnixMs != 1800000000000 {
		t.Fatalf("events = %+v", events)
	}
}

func writeConnectJSONFrame(t *testing.T, w http.ResponseWriter, payload []byte, flags byte) {
	t.Helper()
	var header [5]byte
	header[0] = flags
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
}

func TestNewRejectsInvalidServerID(t *testing.T) {
	if _, err := New("http://127.0.0.1:28080", "bad", ""); err == nil {
		t.Fatalf("New() error is nil, want invalid server id")
	}
}
