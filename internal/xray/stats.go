package xray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	statscmd "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type TrafficDelta struct {
	SubscriptionID int64
	UploadBytes    uint64
	DownloadBytes  uint64
}

type StatsCollector interface {
	Collect(context.Context) ([]TrafficDelta, error)
	Commit() error
	Close() error
}

type GRPCStatsCollector struct {
	address    string
	cursorFile string

	queryMu sync.Mutex
	conn    *grpc.ClientConn
	client  statscmd.StatsServiceClient

	mu      sync.Mutex
	cursor  map[string]int64
	pending map[string]int64
}

func NewGRPCStatsCollector(apiAddress string, cursorFile string) *GRPCStatsCollector {
	collector := &GRPCStatsCollector{
		address:    strings.TrimSpace(apiAddress),
		cursorFile: strings.TrimSpace(cursorFile),
		cursor:     map[string]int64{},
	}
	collector.loadCursor()
	return collector
}

func (c *GRPCStatsCollector) Collect(ctx context.Context) ([]TrafficDelta, error) {
	resp, err := c.queryUserStats(ctx)
	if err != nil {
		return nil, err
	}
	return c.collectFromResponse(resp), nil
}

func (c *GRPCStatsCollector) Commit() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		return nil
	}
	for name, value := range c.pending {
		c.cursor[name] = value
	}
	c.pending = nil
	return c.saveCursor()
}

func (c *GRPCStatsCollector) Close() error {
	c.queryMu.Lock()
	defer c.queryMu.Unlock()
	return c.closeConnLocked()
}

func (c *GRPCStatsCollector) collectFromResponse(resp *statscmd.QueryStatsResponse) []TrafficDelta {
	c.mu.Lock()
	defer c.mu.Unlock()

	bySubscription := make(map[int64]*TrafficDelta)
	pending := make(map[string]int64)
	for _, stat := range resp.GetStat() {
		if stat == nil {
			continue
		}
		name := stat.GetName()
		value := stat.GetValue()
		subscriptionID, direction, ok := parseTrafficStatName(name)
		if !ok {
			continue
		}
		pending[name] = value
		previous := c.cursor[name]
		deltaValue := value - previous
		if deltaValue < 0 {
			deltaValue = value
		}
		if deltaValue <= 0 {
			continue
		}
		delta := bySubscription[subscriptionID]
		if delta == nil {
			delta = &TrafficDelta{SubscriptionID: subscriptionID}
			bySubscription[subscriptionID] = delta
		}
		switch direction {
		case "uplink":
			delta.UploadBytes += uint64(deltaValue)
		case "downlink":
			delta.DownloadBytes += uint64(deltaValue)
		}
	}
	c.pending = pending
	result := make([]TrafficDelta, 0, len(bySubscription))
	for _, delta := range bySubscription {
		result = append(result, *delta)
	}
	return result
}

func (c *GRPCStatsCollector) queryUserStats(ctx context.Context) (*statscmd.QueryStatsResponse, error) {
	c.queryMu.Lock()
	defer c.queryMu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	client, err := c.statsClientLocked(callCtx)
	if err != nil {
		return nil, friendlyStatsError(c.address, err)
	}
	resp, err := client.QueryStats(callCtx, &statscmd.QueryStatsRequest{
		Pattern: "user>>>",
		Reset_:  false,
	})
	if err != nil {
		_ = c.closeConnLocked()
		return nil, friendlyStatsError(c.address, err)
	}
	return resp, nil
}

func (c *GRPCStatsCollector) statsClientLocked(ctx context.Context) (statscmd.StatsServiceClient, error) {
	if c.client != nil {
		return c.client, nil
	}
	if c.address == "" {
		return nil, errors.New("xray api address is empty")
	}
	if err := checkReachable(ctx, c.address); err != nil {
		return nil, err
	}
	conn, err := grpc.DialContext(ctx, c.address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, err
	}
	c.conn = conn
	c.client = statscmd.NewStatsServiceClient(conn)
	return c.client, nil
}

func (c *GRPCStatsCollector) closeConnLocked() error {
	if c.conn == nil {
		c.client = nil
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.client = nil
	return err
}

func (c *GRPCStatsCollector) loadCursor() {
	if c.cursorFile == "" {
		return
	}
	raw, err := os.ReadFile(c.cursorFile)
	if err != nil {
		return
	}
	var cursor map[string]int64
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor == nil {
		return
	}
	c.cursor = cursor
}

func (c *GRPCStatsCollector) saveCursor() error {
	if c.cursorFile == "" {
		return nil
	}
	raw, err := json.MarshalIndent(c.cursor, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(c.cursorFile, raw, 0o600)
}

func checkReachable(ctx context.Context, address string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(checkCtx, "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}

func parseTrafficStatName(name string) (int64, string, bool) {
	parts := strings.Split(name, ">>>")
	if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" {
		return 0, "", false
	}
	subscriptionID, ok := ParseRuntimeUserEmail(parts[1])
	if !ok {
		return 0, "", false
	}
	if parts[3] != "uplink" && parts[3] != "downlink" {
		return 0, "", false
	}
	return subscriptionID, parts[3], true
}

func RuntimeUserEmail(subscriptionID int64) string {
	if subscriptionID <= 0 {
		return ""
	}
	return "u_" + strconv.FormatInt(subscriptionID, 10)
}

func ParseRuntimeUserEmail(email string) (int64, bool) {
	if !strings.HasPrefix(email, "u_") {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(email, "u_"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func friendlyStatsError(address string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("xray stats api %s failed: %w", address, err)
}
