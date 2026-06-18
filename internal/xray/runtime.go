package xray

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"

	"github.com/perfect-panel/moment/xray-agent/internal/config"
	xraycore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all"
)

type Runtime struct {
	cfg config.XrayConfig

	mu       sync.Mutex
	instance *xraycore.Instance
	users    dynamicInboundUsers
}

func NewRuntime(cfg config.XrayConfig, _ config.LogConfig) *Runtime {
	return &Runtime{cfg: cfg}
}

func (r *Runtime) Apply(ctx context.Context, configData []byte) error {
	if len(configData) == 0 {
		return errors.New("xray config is empty")
	}
	if r.cfg.ValidateConfig {
		if err := r.validate(ctx, configData); err != nil {
			return err
		}
	}
	if err := writeFileAtomic(r.cfg.ConfigPath, configData, 0o600); err != nil {
		return fmt.Errorf("write current config: %w", err)
	}
	if err := r.restart(ctx, configData); err != nil {
		if fallbackErr := r.startLastGood(ctx); fallbackErr != nil {
			return fmt.Errorf("%w; last_good restart also failed: %v", err, fallbackErr)
		}
		if restoreErr := r.restoreCurrentFromLastGood(); restoreErr != nil {
			return fmt.Errorf("%w; last_good restarted but current restore failed: %v", err, restoreErr)
		}
		return err
	}
	if err := writeFileAtomic(r.cfg.LastGoodPath, configData, 0o600); err != nil {
		return fmt.Errorf("write last_good config: %w", err)
	}
	r.rememberDynamicUsers(configData)
	return nil
}

func (r *Runtime) SyncUsers(ctx context.Context, configData []byte) error {
	if len(configData) == 0 {
		return errors.New("xray config is empty")
	}
	targetUsers, err := extractDynamicInboundUsers(configData)
	if err != nil {
		return err
	}
	if !targetUsers.supported {
		return errors.New("runtime config has no tag-addressable dynamic user inbounds")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.instance == nil || !r.instance.IsRunning() {
		return errors.New("xray runtime is not running")
	}
	if !r.users.supported {
		return errors.New("current runtime users are not hot-syncable")
	}
	changes := diffDynamicInboundUsers(r.users, targetUsers)
	if changes.empty() {
		if err := writeFileAtomic(r.cfg.ConfigPath, configData, 0o600); err != nil {
			return fmt.Errorf("write current config: %w", err)
		}
		if err := writeFileAtomic(r.cfg.LastGoodPath, configData, 0o600); err != nil {
			return fmt.Errorf("write last_good config: %w", err)
		}
		r.users = targetUsers.clone()
		return nil
	}
	if err := r.applyUserChangesLocked(ctx, changes); err != nil {
		return err
	}
	if err := writeFileAtomic(r.cfg.ConfigPath, configData, 0o600); err != nil {
		return fmt.Errorf("write current config: %w", err)
	}
	if err := writeFileAtomic(r.cfg.LastGoodPath, configData, 0o600); err != nil {
		return fmt.Errorf("write last_good config: %w", err)
	}
	r.users = targetUsers.clone()
	return nil
}

func (r *Runtime) Start(ctx context.Context) error {
	configData, err := readRuntimeConfig(r.cfg.ConfigPath)
	if err != nil {
		if r.cfg.LastGoodPath == "" || r.cfg.LastGoodPath == r.cfg.ConfigPath {
			return err
		}
		configData, err = readRuntimeConfig(r.cfg.LastGoodPath)
		if err != nil {
			return err
		}
	}
	if err := r.restart(ctx, configData); err != nil {
		if r.cfg.LastGoodPath == "" || r.cfg.LastGoodPath == r.cfg.ConfigPath {
			return err
		}
		if fallbackErr := r.startLastGood(ctx); fallbackErr != nil {
			return fmt.Errorf("%w; last_good restart also failed: %v", err, fallbackErr)
		}
		if restoreErr := r.restoreCurrentFromLastGood(); restoreErr != nil {
			return fmt.Errorf("%w; last_good restarted but current restore failed: %v", err, restoreErr)
		}
		return nil
	}
	return nil
}

func (r *Runtime) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
}

func (r *Runtime) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.instance != nil && r.instance.IsRunning()
}

func (r *Runtime) Version(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/xtls/xray-core" {
				version := dep.Version
				if dep.Replace != nil && dep.Replace.Version != "" {
					version = dep.Replace.Version
				}
				if version == "" {
					version = "unknown"
				}
				return "xray-core " + version + " embedded", nil
			}
		}
	}
	return "xray-core embedded", nil
}

func (r *Runtime) validate(ctx context.Context, configData []byte) error {
	instance, err := r.newInstance(ctx, configData)
	if err != nil {
		return fmt.Errorf("xray config validation failed: %w", err)
	}
	if instance != nil {
		_ = instance.Close()
	}
	return nil
}

func (r *Runtime) restart(ctx context.Context, configData []byte) error {
	instance, err := r.newInstance(ctx, configData)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
	if err := instance.Start(); err != nil {
		_ = instance.Close()
		return fmt.Errorf("start embedded xray-core: %w", err)
	}
	r.instance = instance
	r.rememberDynamicUsers(configData)
	return nil
}

func (r *Runtime) rememberDynamicUsers(configData []byte) {
	users, err := extractDynamicInboundUsers(configData)
	if err != nil {
		r.users = dynamicInboundUsers{}
		return
	}
	r.users = users.clone()
}

func (r *Runtime) stopLocked() {
	if r.instance == nil {
		return
	}
	_ = r.instance.Close()
	r.instance = nil
}

func (r *Runtime) startLastGood(ctx context.Context) error {
	if r.cfg.LastGoodPath == "" {
		return errors.New("last_good config path is empty")
	}
	configData, err := readRuntimeConfig(r.cfg.LastGoodPath)
	if err != nil {
		return err
	}
	return r.restart(ctx, configData)
}

func (r *Runtime) restoreCurrentFromLastGood() error {
	if r.cfg.ConfigPath == "" || r.cfg.LastGoodPath == "" || r.cfg.ConfigPath == r.cfg.LastGoodPath {
		return nil
	}
	configData, err := readRuntimeConfig(r.cfg.LastGoodPath)
	if err != nil {
		return err
	}
	return writeFileAtomic(r.cfg.ConfigPath, configData, 0o600)
}

func (r *Runtime) newInstance(ctx context.Context, configData []byte) (*xraycore.Instance, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if err := configureAssetDir(r.cfg.AssetDir); err != nil {
		return nil, err
	}
	coreConfig, err := serial.LoadJSONConfig(bytes.NewReader(configData))
	if err != nil {
		return nil, err
	}
	instance, err := xraycore.New(coreConfig)
	if err != nil {
		return nil, err
	}
	return instance, nil
}

func configureAssetDir(assetDir string) error {
	if assetDir != "" {
		if err := os.MkdirAll(assetDir, 0o755); err != nil {
			return fmt.Errorf("create xray asset dir: %w", err)
		}
		return os.Setenv("XRAY_LOCATION_ASSET", assetDir)
	}
	return nil
}

func readRuntimeConfig(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("xray config path is empty")
	}
	configData, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read xray config %s: %w", path, err)
	}
	if len(configData) == 0 {
		return nil, fmt.Errorf("xray config %s is empty", path)
	}
	return configData, nil
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
