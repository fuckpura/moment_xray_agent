package status

import (
	"bufio"
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/perfect-panel/moment/xray-agent/internal/config"
	"github.com/perfect-panel/moment/xray-agent/internal/serverclient"
)

type Collector struct {
	diskPath string
	cpuPath  string
	memPath  string

	mu      sync.Mutex
	lastCPU cpuSample
	hasCPU  bool
}

func NewCollector(cfg config.XrayConfig) *Collector {
	diskPath := cfg.WorkDir
	if diskPath == "" {
		diskPath = "/"
	}
	return &Collector{
		diskPath: diskPath,
		cpuPath:  "/proc/stat",
		memPath:  "/proc/meminfo",
	}
}

func (c *Collector) Collect(_ context.Context) (serverclient.StatusReport, error) {
	cpuPercent, cpuErr := c.cpuPercent()
	memoryPercent, memoryErr := c.memoryPercent()
	diskPercent, diskErr := c.diskPercent()
	if cpuErr != nil && memoryErr != nil && diskErr != nil {
		return serverclient.StatusReport{}, errors.Join(cpuErr, memoryErr, diskErr)
	}
	return serverclient.StatusReport{
		CPUPercent:    clampPercent(cpuPercent),
		MemoryPercent: clampPercent(memoryPercent),
		DiskPercent:   clampPercent(diskPercent),
	}, nil
}

func (c *Collector) cpuPercent() (float64, error) {
	if runtime.GOOS != "linux" {
		return 0, nil
	}
	current, err := readCPUSample(c.cpuPath)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hasCPU {
		c.lastCPU = current
		c.hasCPU = true
		return 0, nil
	}
	previous := c.lastCPU
	c.lastCPU = current
	totalDelta := current.total - previous.total
	idleDelta := current.idle - previous.idle
	if totalDelta == 0 || idleDelta > totalDelta {
		return 0, nil
	}
	return (float64(totalDelta-idleDelta) / float64(totalDelta)) * 100, nil
}

func (c *Collector) memoryPercent() (float64, error) {
	if runtime.GOOS != "linux" {
		return 0, nil
	}
	total, available, err := readMemInfo(c.memPath)
	if err != nil {
		return 0, err
	}
	if total == 0 || available > total {
		return 0, nil
	}
	return (float64(total-available) / float64(total)) * 100, nil
}

func (c *Collector) diskPercent() (float64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(c.diskPath, &stat); err != nil {
		return 0, err
	}
	total := stat.Blocks
	free := stat.Bavail
	if total == 0 || free > total {
		return 0, nil
	}
	return (float64(total-free) / float64(total)) * 100, nil
}

type cpuSample struct {
	total uint64
	idle  uint64
}

func readCPUSample(path string) (cpuSample, error) {
	file, err := os.Open(path)
	if err != nil {
		return cpuSample{}, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return cpuSample{}, err
		}
		return cpuSample{}, errors.New("empty cpu stat")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, errors.New("invalid cpu stat")
	}
	var values []uint64
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuSample{}, err
		}
		values = append(values, value)
	}
	var total uint64
	for _, value := range values {
		total += value
	}
	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	return cpuSample{total: total, idle: idle}, nil
}

func readMemInfo(path string) (uint64, uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	var total uint64
	var available uint64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, 0, err
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			total = value
		case "MemAvailable":
			available = value
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	if total == 0 {
		return 0, 0, errors.New("missing MemTotal")
	}
	if available == 0 {
		return 0, 0, errors.New("missing MemAvailable")
	}
	return total, available, nil
}

func clampPercent(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 100:
		return 100
	default:
		return value
	}
}
