package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/perfect-panel/moment/xray-agent/internal/config"
	"github.com/perfect-panel/moment/xray-agent/internal/serverclient"
	agentstatus "github.com/perfect-panel/moment/xray-agent/internal/status"
	xrayruntime "github.com/perfect-panel/moment/xray-agent/internal/xray"
)

// AgentVersion is injected by release builds with -ldflags "-X .../internal/app.AgentVersion=vX.Y.Z".
var AgentVersion = "dev"

type App struct {
	cfg                config.Config
	logger             *slog.Logger
	runtimeClient      runtimeClient
	runtimeVersion     string
	runtimeConfigJSON  []byte
	desiredVersion     string
	pulledVersion      string
	appliedVersion     string
	applyState         string
	agentHealth        string
	lastRuntimeError   string
	lastRuntimeErrorAt *time.Time
	startedAt          time.Time
	usersVersion       string
	runtimeUsers       []serverclient.RuntimeUser
	xray               xrayRuntime
	statusCollector    statusCollector
	trafficCollector   trafficCollector
	onlineUsers        *onlineUserTracker
	xrayVersion        string
}

type runtimeClient interface {
	GetRuntimeConfig(context.Context) (serverclient.RuntimeConfig, error)
	GetUsers(context.Context, string) (serverclient.RuntimeUsers, error)
	WatchRuntime(context.Context, string, func(serverclient.RuntimeChangeEvent) error) error
	ReportTraffic(context.Context, []serverclient.TrafficDelta) error
	ReportStatus(context.Context, serverclient.StatusReport) error
}

type registeringRuntimeClient interface {
	RegisterAgent(context.Context, serverclient.RegisterInfo) (serverclient.RegisteredAgent, error)
}

type xrayRuntime interface {
	Apply(context.Context, []byte) error
	SyncUsers(context.Context, []byte) error
	Start(context.Context) error
	Stop()
	Running() bool
	Version(context.Context) (string, error)
}

type statusCollector interface {
	Collect(context.Context) (serverclient.StatusReport, error)
}

type trafficCollector interface {
	Collect(context.Context) ([]xrayruntime.TrafficDelta, error)
	Commit() error
	Close() error
}

func New(cfg config.Config, logger *slog.Logger) *App {
	client, err := serverclient.New(cfg.Server.BaseURL, "", "")
	if err != nil {
		logger.Warn("runtime client disabled", slog.String("error", err.Error()))
	}
	return &App{
		cfg:             cfg,
		logger:          logger,
		runtimeClient:   client,
		applyState:      serverclient.ApplyStatePending,
		agentHealth:     serverclient.HealthHealthy,
		startedAt:       time.Now().UTC(),
		xray:            xrayruntime.NewRuntime(cfg.Xray, cfg.Log),
		statusCollector: agentstatus.NewCollector(cfg.Xray),
		trafficCollector: xrayruntime.NewGRPCStatsCollector(
			cfg.Xray.APIAddress,
			cfg.Xray.StatsCursorPath,
		),
		onlineUsers: newOnlineUserTracker(onlineUserWindow(cfg.Agent)),
	}
}

func (a *App) Run(ctx context.Context) error {
	if strings.TrimSpace(a.cfg.Server.EnrollmentKey) == "" {
		return errors.New("MOMENT_AGENT_ENROLLMENT_KEY is required; create an Agent enrollment key in admin and restart moment-agent")
	}
	a.logger.Info("agent started",
		slog.String("server_url", a.cfg.Server.BaseURL),
		slog.Duration("pull_interval", a.cfg.Agent.PullInterval),
		slog.Duration("status_interval", a.cfg.Agent.StatusInterval),
	)
	if a.xray != nil {
		defer a.xray.Stop()
	}
	if a.trafficCollector != nil {
		defer func() {
			if err := a.trafficCollector.Close(); err != nil {
				a.logger.Warn("close traffic collector failed", slog.String("error", err.Error()))
			}
		}()
	}

	if err := a.registerAgentProcess(ctx); err != nil {
		a.logger.Warn("agent enrollment failed", slog.String("error", err.Error()))
	}
	if err := a.loadRuntimeUsersSnapshot(); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.logger.Warn("load runtime users snapshot failed", slog.String("error", err.Error()))
	}
	if err := a.pullRuntimeUsers(ctx); err != nil {
		a.logger.Warn("initial runtime users pull failed", slog.String("error", err.Error()))
	}
	if err := a.pullRuntimeConfig(ctx); err != nil {
		a.logger.Warn("initial runtime config pull failed", slog.String("error", err.Error()))
		if restoreErr := restoreLastGoodConfig(a.cfg.Xray.LastGoodPath, a.cfg.Xray.ConfigPath); restoreErr != nil {
			a.logger.Warn("restore last_good config failed", slog.String("error", restoreErr.Error()))
		} else if startErr := a.startXrayFromCurrent(ctx); startErr != nil {
			a.logger.Warn("start xray from current config failed", slog.String("error", startErr.Error()))
		}
	}

	pullTicker := time.NewTicker(a.cfg.Agent.PullInterval)
	defer pullTicker.Stop()
	statusTicker := time.NewTicker(a.cfg.Agent.StatusInterval)
	defer statusTicker.Stop()
	trafficTicker := time.NewTicker(a.cfg.Agent.TrafficInterval)
	defer trafficTicker.Stop()
	runtimeChangeCh := make(chan serverclient.RuntimeChangeEvent, 1)
	if a.runtimeClient != nil {
		go a.watchRuntimeChanges(ctx, runtimeChangeCh)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-runtimeChangeCh:
			a.logger.Info("runtime change event received",
				slog.String("event_type", event.EventType),
				slog.String("config_version", event.ConfigVersion),
			)
			if err := a.pullRuntimeConfig(ctx); err != nil {
				a.logger.Warn("pull runtime config after watch event failed", slog.String("error", err.Error()))
			}
			if err := a.pullRuntimeUsers(ctx); err != nil {
				a.logger.Warn("pull runtime users after watch event failed", slog.String("error", err.Error()))
			}
		case <-pullTicker.C:
			if err := a.pullRuntimeConfig(ctx); err != nil {
				a.logger.Warn("pull runtime config failed", slog.String("error", err.Error()))
			}
			if err := a.pullRuntimeUsers(ctx); err != nil {
				a.logger.Warn("pull runtime users failed", slog.String("error", err.Error()))
			}
		case <-statusTicker.C:
			if err := a.reportStatus(ctx); err != nil {
				a.logger.Warn("report status failed", slog.String("error", err.Error()))
			}
		case <-trafficTicker.C:
			if err := a.reportTraffic(ctx); err != nil {
				a.logger.Warn("report traffic failed", slog.String("error", err.Error()))
			}
		}
	}
}

func (a *App) registerAgentProcess(ctx context.Context) error {
	key := strings.TrimSpace(a.cfg.Server.EnrollmentKey)
	if key == "" || a.runtimeClient == nil {
		return nil
	}
	registrar, ok := a.runtimeClient.(registeringRuntimeClient)
	if !ok {
		return nil
	}
	hostname, _ := os.Hostname()
	xrayVersion := a.xrayVersion
	if xrayVersion == "" && a.xray != nil {
		if version, err := a.xray.Version(ctx); err == nil {
			xrayVersion = version
			a.xrayVersion = version
		}
	}
	registered, err := registrar.RegisterAgent(ctx, serverclient.RegisterInfo{
		EnrollmentKey: key,
		ProcessUID:    agentProcessUID(a.cfg, hostname),
		Hostname:      hostname,
		PublicIP:      a.cfg.Server.PublicIP,
		PID:           os.Getpid(),
		StartedAt:     a.startedAt,
		AgentVersion:  AgentVersion,
		XrayVersion:   xrayVersion,
	})
	if err != nil {
		return err
	}
	a.logger.Info("agent enrolled",
		slog.Int64("agent_process_id", registered.AgentProcessID),
		slog.Int64("server_id", registered.ServerID),
		slog.String("status", registered.Status),
		slog.String("token_hint", registered.ProcessTokenHint),
	)
	if registered.Status != "approved" {
		a.logger.Warn("agent process is waiting for admin approval", slog.Int64("agent_process_id", registered.AgentProcessID))
	}
	return nil
}

func agentProcessUID(cfg config.Config, hostname string) string {
	if value := strings.TrimSpace(cfg.Server.ProcessUID); value != "" {
		return value
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(hostname) + "|" + strings.TrimSpace(cfg.Xray.WorkDir) + "|" + strings.TrimSpace(cfg.Xray.ConfigPath)))
	return "moment-agent-" + hex.EncodeToString(sum[:8])
}

func (a *App) watchRuntimeChanges(ctx context.Context, out chan<- serverclient.RuntimeChangeEvent) {
	reconnectDelay := a.cfg.Agent.PullInterval / 3
	if reconnectDelay < 3*time.Second {
		reconnectDelay = 3 * time.Second
	}
	if reconnectDelay > 15*time.Second {
		reconnectDelay = 15 * time.Second
	}
	for {
		if ctx.Err() != nil {
			return
		}
		knownVersion := a.runtimeVersion
		err := a.runtimeClient.WatchRuntime(ctx, knownVersion, func(event serverclient.RuntimeChangeEvent) error {
			if event.ConfigVersion != "" && event.ConfigVersion == knownVersion && event.EventType == "runtime.snapshot" {
				return nil
			}
			select {
			case out <- event:
			default:
			}
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			a.logger.Warn("watch runtime stream disconnected", slog.String("error", err.Error()), slog.Duration("reconnect_delay", reconnectDelay))
		} else {
			a.logger.Debug("watch runtime stream closed", slog.Duration("reconnect_delay", reconnectDelay))
		}
		timer := time.NewTimer(reconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (a *App) pullRuntimeConfig(ctx context.Context) error {
	if a.runtimeClient == nil {
		return errors.New("runtime client is not configured")
	}
	runtimeConfig, err := a.runtimeClient.GetRuntimeConfig(ctx)
	if err != nil {
		return err
	}
	if runtimeConfig.Version != "" {
		a.desiredVersion = runtimeConfig.Version
	}
	if runtimeConfig.Version != "" && runtimeConfig.Version == a.runtimeVersion {
		a.logger.Debug("runtime config unchanged", slog.String("version", runtimeConfig.Version))
		if a.xray != nil && !a.xray.Running() {
			if err := a.startXrayFromCurrent(ctx); err != nil {
				a.markRuntimeFailed(runtimeConfig.Version, err)
				a.reportStatusBestEffort(ctx, "runtime_config_restart_failed")
				return err
			}
			a.markRuntimeApplied(runtimeConfig.Version)
			a.reportStatusBestEffort(ctx, "runtime_config_restarted")
		}
		return nil
	}
	if len(runtimeConfig.XrayJSON) == 0 || !json.Valid(runtimeConfig.XrayJSON) {
		err := errors.New("runtime config xray json is empty or invalid")
		a.markRuntimeFailed(runtimeConfig.Version, err)
		a.reportStatusBestEffort(ctx, "runtime_config_invalid")
		return err
	}
	if err := syncRuntimeGeodataAssets(ctx, a.cfg.Xray.AssetDir, runtimeConfig.GeodataAssets); err != nil {
		a.markRuntimeFailed(runtimeConfig.Version, err)
		a.reportStatusBestEffort(ctx, "runtime_geodata_failed")
		return err
	}
	previousConfig := a.runtimeConfigJSON
	a.runtimeConfigJSON = cloneBytes(runtimeConfig.XrayJSON)
	if err := a.writeEffectiveRuntimeConfig(ctx, time.Now().UTC()); err != nil {
		a.runtimeConfigJSON = previousConfig
		a.markRuntimeFailed(runtimeConfig.Version, err)
		a.reportStatusBestEffort(ctx, "runtime_config_apply_failed")
		return err
	}
	a.runtimeVersion = runtimeConfig.Version
	a.markRuntimeApplied(runtimeConfig.Version)
	a.logger.Info("runtime config updated",
		slog.String("version", runtimeConfig.Version),
		slog.String("config_path", a.cfg.Xray.ConfigPath),
		slog.Int("bytes", len(runtimeConfig.XrayJSON)),
	)
	a.reportStatusBestEffort(ctx, "runtime_config_updated")
	return nil
}

func (a *App) pullRuntimeUsers(ctx context.Context) error {
	if a.runtimeClient == nil {
		return errors.New("runtime client is not configured")
	}
	users, err := a.runtimeClient.GetUsers(ctx, a.usersVersion)
	if err != nil {
		return err
	}
	if users.Version != "" && users.Version == a.usersVersion {
		a.logger.Debug("runtime users unchanged", slog.String("version", users.Version))
		return nil
	}
	if err := validateRuntimeUsers(users.Users); err != nil {
		return err
	}
	previousVersion := a.usersVersion
	previousUsers := a.runtimeUsers
	a.runtimeUsers = append([]serverclient.RuntimeUser(nil), users.Users...)
	if err := a.applyRuntimeUsers(ctx, time.Now().UTC()); err != nil {
		a.runtimeUsers = previousUsers
		a.usersVersion = previousVersion
		a.markRuntimeFailed(a.runtimeVersion, err)
		a.reportStatusBestEffort(ctx, "runtime_users_apply_failed")
		return err
	}
	snapshot := runtimeUsersSnapshot{
		Version:           users.Version,
		GeneratedAtUnixMs: time.Now().UTC().UnixMilli(),
		Users:             runtimeUserSnapshots(a.runtimeUsers),
	}
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.cfg.Xray.UsersSnapshotPath, raw, 0o600); err != nil {
		return fmt.Errorf("write users snapshot: %w", err)
	}
	a.usersVersion = users.Version
	a.logger.Info("runtime users updated",
		slog.String("version", users.Version),
		slog.String("snapshot_path", a.cfg.Xray.UsersSnapshotPath),
		slog.Int("users", len(users.Users)),
	)
	a.reportStatusBestEffort(ctx, "runtime_users_updated")
	return nil
}

func (a *App) applyRuntimeUsers(ctx context.Context, now time.Time) error {
	if err := a.syncRuntimeUsers(ctx, now); err == nil {
		return nil
	} else if shouldLogUserSyncFallback(err) {
		a.logger.Warn("runtime users hot sync failed, falling back to full runtime apply", slog.String("error", err.Error()))
	}
	return a.writeEffectiveRuntimeConfig(ctx, now)
}

func (a *App) syncRuntimeUsers(ctx context.Context, now time.Time) error {
	if len(a.runtimeConfigJSON) == 0 {
		return errors.New("runtime config is not loaded")
	}
	if a.xray == nil {
		return errors.New("xray runtime is not configured")
	}
	if !a.xray.Running() {
		return errors.New("xray runtime is not running")
	}
	effectiveConfig, err := renderEffectiveRuntimeConfig(
		a.runtimeConfigJSON,
		a.runtimeUsers,
		now,
		runtimeAPIConfig{Enabled: a.cfg.Xray.InjectAPI, Port: a.cfg.Xray.APIPort},
		runtimeLogConfig{AccessPath: a.cfg.Log.XrayAccessPath, ErrorPath: a.cfg.Log.XrayErrorPath, Level: a.cfg.Log.XrayLevel},
	)
	if err != nil {
		return fmt.Errorf("render runtime config users: %w", err)
	}
	return a.xray.SyncUsers(ctx, effectiveConfig)
}

func shouldLogUserSyncFallback(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return !strings.Contains(message, "runtime config is not loaded") &&
		!strings.Contains(message, "xray runtime is not configured") &&
		!strings.Contains(message, "xray runtime is not running")
}

func (a *App) loadRuntimeUsersSnapshot() error {
	snapshot, err := readRuntimeUsersSnapshot(a.cfg.Xray.UsersSnapshotPath)
	if err != nil {
		return err
	}
	if err := validateRuntimeUsers(snapshot.Users); err != nil {
		return err
	}
	a.usersVersion = snapshot.Version
	a.runtimeUsers = append([]serverclient.RuntimeUser(nil), snapshot.Users...)
	a.logger.Info("runtime users snapshot loaded",
		slog.String("version", snapshot.Version),
		slog.String("snapshot_path", a.cfg.Xray.UsersSnapshotPath),
		slog.Int("users", len(snapshot.Users)),
	)
	return nil
}

func (a *App) writeEffectiveRuntimeConfig(ctx context.Context, now time.Time) error {
	if len(a.runtimeConfigJSON) == 0 {
		return nil
	}
	effectiveConfig, err := renderEffectiveRuntimeConfig(
		a.runtimeConfigJSON,
		a.runtimeUsers,
		now,
		runtimeAPIConfig{Enabled: a.cfg.Xray.InjectAPI, Port: a.cfg.Xray.APIPort},
		runtimeLogConfig{AccessPath: a.cfg.Log.XrayAccessPath, ErrorPath: a.cfg.Log.XrayErrorPath, Level: a.cfg.Log.XrayLevel},
	)
	if err != nil {
		return fmt.Errorf("render runtime config users: %w", err)
	}
	if a.xray != nil {
		return a.xray.Apply(ctx, effectiveConfig)
	}
	if err := writeFileAtomic(a.cfg.Xray.ConfigPath, effectiveConfig, 0o600); err != nil {
		return fmt.Errorf("write current config: %w", err)
	}
	if err := writeFileAtomic(a.cfg.Xray.LastGoodPath, effectiveConfig, 0o600); err != nil {
		return fmt.Errorf("write last_good config: %w", err)
	}
	return nil
}

func (a *App) startXrayFromCurrent(ctx context.Context) error {
	if a.xray == nil {
		return nil
	}
	if _, err := os.Stat(a.cfg.Xray.ConfigPath); err != nil {
		return err
	}
	if err := a.xray.Start(ctx); err != nil {
		a.markRuntimeFailed(a.runtimeVersion, err)
		return err
	}
	a.markRuntimeApplied(a.runtimeVersion)
	return nil
}

func (a *App) reportStatus(ctx context.Context) error {
	if a.runtimeClient == nil {
		return errors.New("runtime client is not configured")
	}
	if a.statusCollector == nil {
		return errors.New("status collector is not configured")
	}
	report, err := a.statusCollector.Collect(ctx)
	if err != nil {
		return err
	}
	if a.onlineUsers != nil {
		report.OnlineUserCount = a.onlineUsers.Count(time.Now().UTC())
	}
	a.enrichStatusReport(ctx, &report)
	return a.runtimeClient.ReportStatus(ctx, report)
}

func (a *App) reportStatusBestEffort(ctx context.Context, reason string) {
	if err := a.reportStatus(ctx); err != nil {
		a.logger.Warn("report status after runtime update failed",
			slog.String("reason", reason),
			slog.String("error", err.Error()),
		)
		return
	}
	a.logger.Debug("runtime status reported", slog.String("reason", reason))
}

func (a *App) markRuntimeApplied(version string) {
	version = strings.TrimSpace(version)
	if version != "" {
		a.desiredVersion = version
		a.pulledVersion = version
		a.appliedVersion = version
	}
	a.applyState = serverclient.ApplyStateApplied
	a.agentHealth = serverclient.HealthHealthy
	a.lastRuntimeError = ""
	a.lastRuntimeErrorAt = nil
}

func (a *App) markRuntimeFailed(version string, err error) {
	version = strings.TrimSpace(version)
	if version != "" {
		a.desiredVersion = version
	}
	a.applyState = serverclient.ApplyStateFailed
	a.agentHealth = serverclient.HealthError
	if err != nil {
		a.lastRuntimeError = strings.TrimSpace(err.Error())
		now := time.Now().UTC()
		a.lastRuntimeErrorAt = &now
	}
}

func (a *App) enrichStatusReport(ctx context.Context, report *serverclient.StatusReport) {
	report.DesiredConfigVersion = a.desiredVersion
	report.PulledConfigVersion = a.pulledVersion
	report.AppliedConfigVersion = a.appliedVersion
	report.ApplyState = a.applyState
	report.Health = a.agentHealth
	report.AgentVersion = strings.TrimSpace(AgentVersion)
	report.XrayVersion = a.currentXrayVersion(ctx)
	report.LastError = a.lastRuntimeError
	if a.lastRuntimeErrorAt != nil {
		report.LastErrorUnixMs = a.lastRuntimeErrorAt.UnixMilli()
	}
	if !a.startedAt.IsZero() {
		report.StartedAtUnixMs = a.startedAt.UnixMilli()
	}
}

func (a *App) currentXrayVersion(ctx context.Context) string {
	if strings.TrimSpace(a.xrayVersion) != "" {
		return a.xrayVersion
	}
	if a.xray == nil {
		return ""
	}
	versionCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	version, err := a.xray.Version(versionCtx)
	if err != nil {
		a.logger.Debug("xray version probe failed", slog.String("error", err.Error()))
		return ""
	}
	a.xrayVersion = strings.TrimSpace(version)
	return a.xrayVersion
}

func (a *App) reportTraffic(ctx context.Context) error {
	if a.runtimeClient == nil {
		return errors.New("runtime client is not configured")
	}
	if a.trafficCollector == nil {
		return errors.New("traffic collector is not configured")
	}
	deltas, err := a.trafficCollector.Collect(ctx)
	if err != nil {
		return err
	}
	if a.onlineUsers != nil {
		a.onlineUsers.Observe(deltas, time.Now().UTC())
	}
	if len(deltas) > 0 {
		if err := a.runtimeClient.ReportTraffic(ctx, trafficDeltasForServer(deltas)); err != nil {
			return err
		}
	}
	return a.trafficCollector.Commit()
}

func onlineUserWindow(cfg config.AgentConfig) time.Duration {
	window := cfg.TrafficInterval * 3
	if cfg.StatusInterval*2 > window {
		window = cfg.StatusInterval * 2
	}
	if window < 30*time.Second {
		window = 30 * time.Second
	}
	return window
}

func trafficDeltasForServer(deltas []xrayruntime.TrafficDelta) []serverclient.TrafficDelta {
	out := make([]serverclient.TrafficDelta, 0, len(deltas))
	for _, delta := range deltas {
		out = append(out, serverclient.TrafficDelta{
			SubscriptionID: delta.SubscriptionID,
			UploadBytes:    delta.UploadBytes,
			DownloadBytes:  delta.DownloadBytes,
		})
	}
	return out
}

func validateRuntimeUsers(users []serverclient.RuntimeUser) error {
	for _, user := range users {
		if user.SubscriptionID <= 0 {
			return fmt.Errorf("runtime user has invalid subscription id: %d", user.SubscriptionID)
		}
		if strings.TrimSpace(user.UUID) == "" {
			return fmt.Errorf("runtime user %d has empty uuid", user.SubscriptionID)
		}
		if strings.TrimSpace(user.Password) == "" {
			return fmt.Errorf("runtime user %d has empty password", user.SubscriptionID)
		}
	}
	return nil
}

type runtimeUsersSnapshot struct {
	Version           string                `json:"version"`
	GeneratedAtUnixMs int64                 `json:"generated_at_unix_ms"`
	Users             []runtimeUserSnapshot `json:"users"`
}

type runtimeUserSnapshot struct {
	SubscriptionID  int64  `json:"subscription_id"`
	Email           string `json:"email"`
	UUID            string `json:"uuid"`
	Password        string `json:"password"`
	SpeedLimitBPS   uint64 `json:"speed_limit_bps"`
	Enabled         bool   `json:"enabled"`
	ExpiredAtUnixMs int64  `json:"expired_at_unix_ms"`
}

func runtimeUserSnapshots(users []serverclient.RuntimeUser) []runtimeUserSnapshot {
	snapshots := make([]runtimeUserSnapshot, 0, len(users))
	for _, user := range users {
		snapshots = append(snapshots, runtimeUserSnapshot{
			SubscriptionID:  user.SubscriptionID,
			Email:           user.Email,
			UUID:            user.UUID,
			Password:        user.Password,
			SpeedLimitBPS:   user.SpeedLimitBPS,
			Enabled:         user.Enabled,
			ExpiredAtUnixMs: user.ExpiredAtUnixMs,
		})
	}
	return snapshots
}

func readRuntimeUsersSnapshot(path string) (serverclient.RuntimeUsers, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return serverclient.RuntimeUsers{}, err
	}
	var snapshot runtimeUsersSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return serverclient.RuntimeUsers{}, err
	}
	users := make([]serverclient.RuntimeUser, 0, len(snapshot.Users))
	for _, user := range snapshot.Users {
		users = append(users, serverclient.RuntimeUser{
			SubscriptionID:  user.SubscriptionID,
			Email:           user.Email,
			UUID:            user.UUID,
			Password:        user.Password,
			SpeedLimitBPS:   user.SpeedLimitBPS,
			Enabled:         user.Enabled,
			ExpiredAtUnixMs: user.ExpiredAtUnixMs,
		})
	}
	return serverclient.RuntimeUsers{Version: snapshot.Version, Users: users}, nil
}

func restoreLastGoodConfig(lastGoodPath string, configPath string) error {
	if lastGoodPath == "" || configPath == "" {
		return errors.New("last_good or current config path is empty")
	}
	if _, err := os.Stat(configPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := os.ReadFile(lastGoodPath)
	if err != nil {
		return err
	}
	if len(raw) == 0 || !json.Valid(raw) {
		return errors.New("last_good config is empty or invalid")
	}
	return writeFileAtomic(configPath, raw, 0o600)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if path == "" {
		return errors.New("path is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func cloneBytes(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	return append([]byte(nil), raw...)
}
