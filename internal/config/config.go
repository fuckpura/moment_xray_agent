package config

import (
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	Server ServerConfig
	Agent  AgentConfig
	Xray   XrayConfig
	Log    LogConfig
}

type ServerConfig struct {
	BaseURL       string
	EnrollmentKey string
	ProcessUID    string
	PublicIP      string
}

type AgentConfig struct {
	PullInterval    time.Duration
	StatusInterval  time.Duration
	TrafficInterval time.Duration
}

type XrayConfig struct {
	WorkDir           string
	AssetDir          string
	ConfigPath        string
	LastGoodPath      string
	UsersSnapshotPath string
	APIPort           string
	APIAddress        string
	InjectAPI         bool
	ValidateConfig    bool
	StatsCursorPath   string
}

type LogConfig struct {
	Level          string
	AgentPath      string
	XrayErrorPath  string
	XrayAccessPath string
	XrayLevel      string
}

func Load() Config {
	workDir := getenv("MOMENT_AGENT_WORK_DIR", "/var/lib/moment/xray-agent")
	return Config{
		Server: ServerConfig{
			BaseURL:       getenv("MOMENT_SERVER_URL", "http://127.0.0.1:28080"),
			EnrollmentKey: os.Getenv("MOMENT_AGENT_ENROLLMENT_KEY"),
			ProcessUID:    os.Getenv("MOMENT_AGENT_PROCESS_UID"),
			PublicIP:      os.Getenv("MOMENT_AGENT_PUBLIC_IP"),
		},
		Agent: AgentConfig{
			PullInterval:    getenvDuration("MOMENT_AGENT_PULL_INTERVAL", 30*time.Second),
			StatusInterval:  getenvDuration("MOMENT_AGENT_STATUS_INTERVAL", 5*time.Second),
			TrafficInterval: getenvDuration("MOMENT_AGENT_TRAFFIC_INTERVAL", 30*time.Second),
		},
		Xray: XrayConfig{
			WorkDir:           workDir,
			AssetDir:          getenv("MOMENT_XRAY_ASSET_DIR", "/usr/local/share/xray"),
			ConfigPath:        getenv("MOMENT_XRAY_CONFIG_PATH", workDir+"/current.json"),
			LastGoodPath:      getenv("MOMENT_XRAY_LAST_GOOD_PATH", workDir+"/last_good.json"),
			UsersSnapshotPath: getenv("MOMENT_XRAY_USERS_SNAPSHOT_PATH", workDir+"/users_snapshot.json"),
			APIPort:           getenv("MOMENT_XRAY_API_PORT", "10085"),
			APIAddress:        getenv("MOMENT_XRAY_API_ADDRESS", "127.0.0.1:"+getenv("MOMENT_XRAY_API_PORT", "10085")),
			InjectAPI:         getenvBool("MOMENT_XRAY_INJECT_API", true),
			ValidateConfig:    getenvBool("MOMENT_XRAY_VALIDATE_CONFIG", true),
			StatsCursorPath:   getenv("MOMENT_XRAY_STATS_CURSOR_PATH", workDir+"/traffic_cursor.json"),
		},
		Log: LogConfig{
			Level:          getenv("MOMENT_AGENT_LOG_LEVEL", "info"),
			AgentPath:      getenv("MOMENT_AGENT_LOG_PATH", "/var/log/moment/xray-agent/agent.log"),
			XrayErrorPath:  getenv("MOMENT_XRAY_ERROR_LOG_PATH", "/var/log/moment/xray-agent/xray-error.log"),
			XrayAccessPath: getenv("MOMENT_XRAY_ACCESS_LOG_PATH", "/var/log/moment/xray-agent/xray-access.log"),
			XrayLevel:      getenv("MOMENT_XRAY_LOG_LEVEL", "warning"),
		},
	}
}

func (c Config) LogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(c.Log.Level)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "no", "NO", "off", "OFF":
		return false
	default:
		return fallback
	}
}
