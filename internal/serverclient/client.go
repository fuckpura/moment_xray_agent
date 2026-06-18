package serverclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	registerAgentPath    = "/moment.agent.v1.AgentService/RegisterAgent"
	getRuntimeConfigPath = "/moment.agent.v1.AgentService/GetRuntimeConfig"
	getUsersPath         = "/moment.agent.v1.AgentService/GetUsers"
	watchRuntimePath     = "/moment.agent.v1.AgentService/WatchRuntime"
	reportTrafficPath    = "/moment.agent.v1.AgentService/ReportTraffic"
	reportStatusPath     = "/moment.agent.v1.AgentService/ReportStatus"

	ApplyStatePending    = "pending"
	ApplyStateApplied    = "applied"
	ApplyStateFailed     = "failed"
	ApplyStateRolledBack = "rolled_back"

	HealthHealthy  = "healthy"
	HealthDegraded = "degraded"
	HealthError    = "error"
)

var ErrEmptyBaseURL = errors.New("empty server base url")

type Client struct {
	baseURL    string
	httpClient *http.Client
	secretKey  string
	serverID   string
	processID  string
}

type Option func(*Client)

type RuntimeConfig struct {
	Version       string
	XrayJSON      []byte
	GeodataAssets []GeodataAsset
}

type RuntimeChangeEvent struct {
	ServerID         int64
	AgentProcessID   int64
	EventType        string
	ConfigVersion    string
	OccurredAtUnixMs int64
}

type GeodataAsset struct {
	File   string
	URL    string
	SHA256 string
}

type RuntimeUsers struct {
	Version string
	Users   []RuntimeUser
}

type RuntimeUser struct {
	SubscriptionID  int64
	Email           string
	UUID            string
	Password        string
	SpeedLimitBPS   uint64
	Enabled         bool
	ExpiredAtUnixMs int64
}

type TrafficDelta struct {
	SubscriptionID int64
	UploadBytes    uint64
	DownloadBytes  uint64
}

type StatusReport struct {
	CPUPercent           float64
	MemoryPercent        float64
	DiskPercent          float64
	OnlineUserCount      uint64
	DesiredConfigVersion string
	PulledConfigVersion  string
	AppliedConfigVersion string
	ApplyState           string
	Health               string
	AgentVersion         string
	XrayVersion          string
	LastError            string
	LastErrorUnixMs      int64
	StartedAtUnixMs      int64
}

type RegisterInfo struct {
	EnrollmentKey string
	ProcessUID    string
	Hostname      string
	PublicIP      string
	PID           int
	StartedAt     time.Time
	AgentVersion  string
	XrayVersion   string
}

type RegisteredAgent struct {
	AgentProcessID   int64
	ServerID         int64
	Status           string
	ProcessToken     string
	ProcessTokenHint string
}

func New(baseURL string, serverID string, secretKey string, opts ...Option) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, ErrEmptyBaseURL
	}
	if strings.TrimSpace(serverID) != "" {
		if _, err := strconv.ParseInt(strings.TrimSpace(serverID), 10, 64); err != nil {
			return nil, fmt.Errorf("invalid server id: %w", err)
		}
	}
	client := &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		secretKey:  strings.TrimSpace(secretKey),
		serverID:   strings.TrimSpace(serverID),
	}
	for _, opt := range opts {
		opt(client)
	}
	if client.httpClient == nil {
		client.httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return client, nil
}

func (c *Client) RegisterAgent(ctx context.Context, info RegisterInfo) (RegisteredAgent, error) {
	if strings.TrimSpace(info.EnrollmentKey) == "" {
		return RegisteredAgent{}, errors.New("empty enrollment key")
	}
	requestBody, err := json.Marshal(registerAgentRequest{
		EnrollmentKey:   strings.TrimSpace(info.EnrollmentKey),
		ProcessUID:      strings.TrimSpace(info.ProcessUID),
		Hostname:        strings.TrimSpace(info.Hostname),
		PublicIP:        strings.TrimSpace(info.PublicIP),
		PID:             int32(info.PID),
		StartedAtUnixMs: protoJSONInt64(info.StartedAt.UnixMilli()),
		AgentVersion:    strings.TrimSpace(info.AgentVersion),
		XrayVersion:     strings.TrimSpace(info.XrayVersion),
	})
	if err != nil {
		return RegisteredAgent{}, err
	}
	raw, err := c.postConnectJSONWithoutAuth(ctx, registerAgentPath, requestBody)
	if err != nil {
		return RegisteredAgent{}, err
	}
	var response registerAgentResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return RegisteredAgent{}, err
	}
	registered, err := response.registeredAgent()
	if err != nil {
		return RegisteredAgent{}, err
	}
	if registered.ProcessToken != "" {
		c.secretKey = registered.ProcessToken
	}
	if registered.ServerID > 0 {
		c.serverID = strconv.FormatInt(registered.ServerID, 10)
	}
	if registered.AgentProcessID > 0 {
		c.processID = strconv.FormatInt(registered.AgentProcessID, 10)
	}
	return registered, nil
}

func (c *Client) SetRuntimeIdentity(serverID int64, agentProcessID int64, token string) error {
	if serverID > 0 {
		c.serverID = strconv.FormatInt(serverID, 10)
	}
	if agentProcessID > 0 {
		c.processID = strconv.FormatInt(agentProcessID, 10)
	}
	if strings.TrimSpace(token) != "" {
		c.secretKey = strings.TrimSpace(token)
	}
	if c.serverID == "" && c.processID == "" {
		return errors.New("empty runtime identity")
	}
	return nil
}

func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		c.httpClient = httpClient
	}
}

func (c *Client) GetRuntimeConfig(ctx context.Context) (RuntimeConfig, error) {
	requestBody, err := json.Marshal(getRuntimeConfigRequest{ServerID: c.serverID, AgentProcessID: c.processID})
	if err != nil {
		return RuntimeConfig{}, err
	}
	raw, err := c.postConnectJSON(ctx, getRuntimeConfigPath, requestBody)
	if err != nil {
		return RuntimeConfig{}, err
	}
	var response getRuntimeConfigResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return RuntimeConfig{}, err
	}
	xrayJSON, err := base64.StdEncoding.DecodeString(response.XrayJSON)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("decode xray json: %w", err)
	}
	return RuntimeConfig{Version: response.Version, XrayJSON: xrayJSON, GeodataAssets: response.GeodataAssets.runtimeAssets()}, nil
}

func (c *Client) GetUsers(ctx context.Context, knownVersion string) (RuntimeUsers, error) {
	requestBody, err := json.Marshal(getUsersRequest{ServerID: c.serverID, AgentProcessID: c.processID, KnownVersion: strings.TrimSpace(knownVersion)})
	if err != nil {
		return RuntimeUsers{}, err
	}
	raw, err := c.postConnectJSON(ctx, getUsersPath, requestBody)
	if err != nil {
		return RuntimeUsers{}, err
	}
	var response getUsersResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return RuntimeUsers{}, err
	}
	users := make([]RuntimeUser, 0, len(response.Users))
	for _, rawUser := range response.Users {
		user, err := rawUser.runtimeUser()
		if err != nil {
			return RuntimeUsers{}, err
		}
		users = append(users, user)
	}
	return RuntimeUsers{Version: response.Version, Users: users}, nil
}

func (c *Client) WatchRuntime(ctx context.Context, knownConfigVersion string, handle func(RuntimeChangeEvent) error) error {
	if handle == nil {
		return errors.New("nil runtime change handler")
	}
	requestBody, err := json.Marshal(watchRuntimeRequest{ServerID: c.serverID, AgentProcessID: c.processID, KnownConfigVersion: strings.TrimSpace(knownConfigVersion)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+watchRuntimePath, bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Content-Type", "application/json")
	if c.secretKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.secretKey)
	}
	httpClient := *c.httpClient
	httpClient.Timeout = 0
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("connect stream %s failed: status %d: %s", watchRuntimePath, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	for {
		var header [5]byte
		if _, err := io.ReadFull(resp.Body, header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		flags := header[0]
		size := binary.BigEndian.Uint32(header[1:])
		if size > 4<<20 {
			return fmt.Errorf("connect stream message too large: %d", size)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(resp.Body, payload); err != nil {
			return err
		}
		if flags&0x02 != 0 {
			return nil
		}
		var response runtimeChangeEventResponse
		if err := json.Unmarshal(payload, &response); err != nil {
			return err
		}
		if err := handle(response.runtimeChangeEvent()); err != nil {
			return err
		}
	}
}

func (c *Client) ReportTraffic(ctx context.Context, deltas []TrafficDelta) error {
	request := reportTrafficRequest{
		ServerID:       c.serverID,
		AgentProcessID: c.processID,
		UserDeltas:     make([]userTrafficDeltaRequest, 0, len(deltas)),
	}
	for _, delta := range deltas {
		if delta.SubscriptionID <= 0 || (delta.UploadBytes == 0 && delta.DownloadBytes == 0) {
			continue
		}
		request.UserDeltas = append(request.UserDeltas, userTrafficDeltaRequest{
			SubscriptionID: strconv.FormatInt(delta.SubscriptionID, 10),
			Delta: trafficBytesRequest{
				Upload:   strconv.FormatUint(delta.UploadBytes, 10),
				Download: strconv.FormatUint(delta.DownloadBytes, 10),
			},
		})
	}
	requestBody, err := json.Marshal(request)
	if err != nil {
		return err
	}
	_, err = c.postConnectJSON(ctx, reportTrafficPath, requestBody)
	return err
}

func (c *Client) ReportStatus(ctx context.Context, report StatusReport) error {
	requestBody, err := json.Marshal(reportStatusRequest{
		ServerID:             c.serverID,
		AgentProcessID:       c.processID,
		CPUPercent:           report.CPUPercent,
		MemoryPercent:        report.MemoryPercent,
		DiskPercent:          report.DiskPercent,
		OnlineUserCount:      strconv.FormatUint(report.OnlineUserCount, 10),
		DesiredConfigVersion: report.DesiredConfigVersion,
		PulledConfigVersion:  report.PulledConfigVersion,
		AppliedConfigVersion: report.AppliedConfigVersion,
		ApplyState:           protoRuntimeApplyState(report.ApplyState),
		Health:               protoAgentHealth(report.Health),
		AgentVersion:         report.AgentVersion,
		XrayVersion:          report.XrayVersion,
		LastError:            report.LastError,
		LastErrorUnixMs:      protoJSONInt64(report.LastErrorUnixMs),
		StartedAtUnixMs:      protoJSONInt64(report.StartedAtUnixMs),
	})
	if err != nil {
		return err
	}
	_, err = c.postConnectJSON(ctx, reportStatusPath, requestBody)
	return err
}

func (c *Client) postConnectJSON(ctx context.Context, path string, body []byte) ([]byte, error) {
	return c.postConnectJSONWithAuth(ctx, path, body, true)
}

func (c *Client) postConnectJSONWithoutAuth(ctx context.Context, path string, body []byte) ([]byte, error) {
	return c.postConnectJSONWithAuth(ctx, path, body, false)
}

func (c *Client) postConnectJSONWithAuth(ctx context.Context, path string, body []byte, withAuth bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Content-Type", "application/json")
	if withAuth && c.secretKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.secretKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("connect request %s failed: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

type registerAgentRequest struct {
	EnrollmentKey   string `json:"enrollmentKey"`
	ProcessUID      string `json:"processUid"`
	Hostname        string `json:"hostname"`
	PublicIP        string `json:"publicIp,omitempty"`
	PID             int32  `json:"pid,omitempty"`
	StartedAtUnixMs string `json:"startedAtUnixMs,omitempty"`
	AgentVersion    string `json:"agentVersion,omitempty"`
	XrayVersion     string `json:"xrayVersion,omitempty"`
}

type registerAgentResponse struct {
	AgentProcessID   json.RawMessage `json:"agentProcessId"`
	ServerID         json.RawMessage `json:"serverId"`
	Status           string          `json:"status"`
	ProcessToken     string          `json:"processToken"`
	ProcessTokenHint string          `json:"processTokenHint"`
}

func (r registerAgentResponse) registeredAgent() (RegisteredAgent, error) {
	processID, err := parseJSONInt64(r.AgentProcessID)
	if err != nil {
		return RegisteredAgent{}, fmt.Errorf("agent process id: %w", err)
	}
	serverID, err := parseJSONInt64(r.ServerID)
	if err != nil {
		return RegisteredAgent{}, fmt.Errorf("server id: %w", err)
	}
	return RegisteredAgent{
		AgentProcessID:   processID,
		ServerID:         serverID,
		Status:           registerStatus(r.Status),
		ProcessToken:     r.ProcessToken,
		ProcessTokenHint: r.ProcessTokenHint,
	}, nil
}

func registerStatus(value string) string {
	switch strings.TrimSpace(value) {
	case "AGENT_PROCESS_STATUS_PENDING":
		return "pending"
	case "AGENT_PROCESS_STATUS_APPROVED":
		return "approved"
	case "AGENT_PROCESS_STATUS_REJECTED":
		return "rejected"
	case "AGENT_PROCESS_STATUS_REVOKED":
		return "revoked"
	default:
		return strings.TrimSpace(value)
	}
}

type getRuntimeConfigRequest struct {
	ServerID       string `json:"serverId,omitempty"`
	AgentProcessID string `json:"agentProcessId,omitempty"`
}

type getRuntimeConfigResponse struct {
	Version       string                `json:"version"`
	XrayJSON      string                `json:"xrayJson"`
	GeodataAssets geodataAssetResponses `json:"geodataAssets"`
}

type geodataAssetResponses []geodataAssetResponse

type geodataAssetResponse struct {
	File   string `json:"file"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

func (assets geodataAssetResponses) runtimeAssets() []GeodataAsset {
	out := make([]GeodataAsset, 0, len(assets))
	for _, asset := range assets {
		out = append(out, GeodataAsset{File: asset.File, URL: asset.URL, SHA256: asset.SHA256})
	}
	return out
}

type getUsersRequest struct {
	ServerID       string `json:"serverId,omitempty"`
	AgentProcessID string `json:"agentProcessId,omitempty"`
	KnownVersion   string `json:"knownVersion,omitempty"`
}

type watchRuntimeRequest struct {
	ServerID           string `json:"serverId,omitempty"`
	AgentProcessID     string `json:"agentProcessId,omitempty"`
	KnownConfigVersion string `json:"knownConfigVersion,omitempty"`
}

type runtimeChangeEventResponse struct {
	ServerID         json.RawMessage `json:"serverId"`
	AgentProcessID   json.RawMessage `json:"agentProcessId"`
	EventType        string          `json:"eventType"`
	ConfigVersion    string          `json:"configVersion"`
	OccurredAtUnixMs json.RawMessage `json:"occurredAtUnixMs"`
}

func (e runtimeChangeEventResponse) runtimeChangeEvent() RuntimeChangeEvent {
	serverID, _ := parseJSONInt64(e.ServerID)
	processID, _ := parseJSONInt64(e.AgentProcessID)
	occurredAt, _ := parseJSONInt64(e.OccurredAtUnixMs)
	return RuntimeChangeEvent{
		ServerID:         serverID,
		AgentProcessID:   processID,
		EventType:        e.EventType,
		ConfigVersion:    e.ConfigVersion,
		OccurredAtUnixMs: occurredAt,
	}
}

type getUsersResponse struct {
	Version string                `json:"version"`
	Users   []runtimeUserResponse `json:"users"`
}

type runtimeUserResponse struct {
	SubscriptionID  json.RawMessage `json:"subscriptionId"`
	Email           string          `json:"email"`
	UUID            string          `json:"uuid"`
	Password        string          `json:"password"`
	SpeedLimitBPS   json.RawMessage `json:"speedLimitBps"`
	Enabled         bool            `json:"enabled"`
	ExpiredAtUnixMs json.RawMessage `json:"expiredAtUnixMs"`
}

type reportTrafficRequest struct {
	ServerID       string                    `json:"serverId,omitempty"`
	AgentProcessID string                    `json:"agentProcessId,omitempty"`
	UserDeltas     []userTrafficDeltaRequest `json:"userDeltas"`
}

type userTrafficDeltaRequest struct {
	SubscriptionID string              `json:"subscriptionId"`
	Delta          trafficBytesRequest `json:"delta"`
}

type trafficBytesRequest struct {
	Upload   string `json:"upload"`
	Download string `json:"download"`
}

type reportStatusRequest struct {
	ServerID             string  `json:"serverId,omitempty"`
	AgentProcessID       string  `json:"agentProcessId,omitempty"`
	CPUPercent           float64 `json:"cpuPercent"`
	MemoryPercent        float64 `json:"memoryPercent"`
	DiskPercent          float64 `json:"diskPercent"`
	OnlineUserCount      string  `json:"onlineUserCount"`
	DesiredConfigVersion string  `json:"desiredConfigVersion,omitempty"`
	PulledConfigVersion  string  `json:"pulledConfigVersion,omitempty"`
	AppliedConfigVersion string  `json:"appliedConfigVersion,omitempty"`
	ApplyState           string  `json:"applyState,omitempty"`
	Health               string  `json:"health,omitempty"`
	AgentVersion         string  `json:"agentVersion,omitempty"`
	XrayVersion          string  `json:"xrayVersion,omitempty"`
	LastError            string  `json:"lastError,omitempty"`
	LastErrorUnixMs      string  `json:"lastErrorUnixMs,omitempty"`
	StartedAtUnixMs      string  `json:"startedAtUnixMs,omitempty"`
}

func protoRuntimeApplyState(value string) string {
	switch strings.TrimSpace(value) {
	case ApplyStatePending:
		return "RUNTIME_APPLY_STATE_PENDING"
	case ApplyStateApplied:
		return "RUNTIME_APPLY_STATE_APPLIED"
	case ApplyStateFailed:
		return "RUNTIME_APPLY_STATE_FAILED"
	case ApplyStateRolledBack:
		return "RUNTIME_APPLY_STATE_ROLLED_BACK"
	default:
		return ""
	}
}

func protoAgentHealth(value string) string {
	switch strings.TrimSpace(value) {
	case HealthHealthy:
		return "AGENT_HEALTH_HEALTHY"
	case HealthDegraded:
		return "AGENT_HEALTH_DEGRADED"
	case HealthError:
		return "AGENT_HEALTH_ERROR"
	default:
		return ""
	}
}

func protoJSONInt64(value int64) string {
	if value <= 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func (u runtimeUserResponse) runtimeUser() (RuntimeUser, error) {
	subscriptionID, err := parseJSONInt64(u.SubscriptionID)
	if err != nil {
		return RuntimeUser{}, fmt.Errorf("subscription id: %w", err)
	}
	speedLimit, err := parseJSONUint64(u.SpeedLimitBPS)
	if err != nil {
		return RuntimeUser{}, fmt.Errorf("speed limit: %w", err)
	}
	expiredAt, err := parseJSONInt64(u.ExpiredAtUnixMs)
	if err != nil {
		return RuntimeUser{}, fmt.Errorf("expired at: %w", err)
	}
	return RuntimeUser{
		SubscriptionID:  subscriptionID,
		Email:           u.Email,
		UUID:            u.UUID,
		Password:        u.Password,
		SpeedLimitBPS:   speedLimit,
		Enabled:         u.Enabled,
		ExpiredAtUnixMs: expiredAt,
	}, nil
}

func parseJSONInt64(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return 0, nil
		}
		return strconv.ParseInt(text, 10, 64)
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, err
	}
	return strconv.ParseInt(number.String(), 10, 64)
}

func parseJSONUint64(raw json.RawMessage) (uint64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return 0, nil
		}
		return strconv.ParseUint(text, 10, 64)
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, err
	}
	return strconv.ParseUint(number.String(), 10, 64)
}
