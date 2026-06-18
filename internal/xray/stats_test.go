package xray

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	statscmd "github.com/xtls/xray-core/app/stats/command"
)

func TestRuntimeUserEmailParsing(t *testing.T) {
	if got := RuntimeUserEmail(11); got != "u_11" {
		t.Fatalf("RuntimeUserEmail(11) = %q", got)
	}
	if got := RuntimeUserEmail(0); got != "" {
		t.Fatalf("RuntimeUserEmail(0) = %q", got)
	}
	id, ok := ParseRuntimeUserEmail("u_11")
	if !ok || id != 11 {
		t.Fatalf("ParseRuntimeUserEmail() = %d %v", id, ok)
	}
	for _, value := range []string{"", "user@example.com", "u_0", "u_-1", "u_abc"} {
		if id, ok := ParseRuntimeUserEmail(value); ok || id != 0 {
			t.Fatalf("ParseRuntimeUserEmail(%q) = %d %v, want invalid", value, id, ok)
		}
	}
}

func TestParseTrafficStatName(t *testing.T) {
	id, direction, ok := parseTrafficStatName("user>>>u_11>>>traffic>>>uplink")
	if !ok || id != 11 || direction != "uplink" {
		t.Fatalf("parse uplink = %d %q %v", id, direction, ok)
	}
	id, direction, ok = parseTrafficStatName("user>>>u_11>>>traffic>>>downlink")
	if !ok || id != 11 || direction != "downlink" {
		t.Fatalf("parse downlink = %d %q %v", id, direction, ok)
	}
	for _, name := range []string{
		"",
		"user>>>user@example.com>>>traffic>>>uplink",
		"inbound>>>tag>>>traffic>>>uplink",
		"user>>>u_11>>>stats>>>uplink",
		"user>>>u_11>>>traffic>>>total",
	} {
		if id, direction, ok := parseTrafficStatName(name); ok || id != 0 || direction != "" {
			t.Fatalf("parseTrafficStatName(%q) = %d %q %v, want invalid", name, id, direction, ok)
		}
	}
}

func TestGRPCStatsCollectorCollectsDeltasAndCommitsCursor(t *testing.T) {
	cursorFile := filepath.Join(t.TempDir(), "traffic_cursor.json")
	collector := &GRPCStatsCollector{
		cursorFile: cursorFile,
		cursor:     map[string]int64{},
	}

	first := collector.collectFromResponse(&statscmd.QueryStatsResponse{Stat: []*statscmd.Stat{
		{Name: "user>>>u_11>>>traffic>>>uplink", Value: 100},
		{Name: "user>>>u_11>>>traffic>>>downlink", Value: 250},
		{Name: "user>>>u_12>>>traffic>>>uplink", Value: 0},
		{Name: "user>>>u_abc>>>traffic>>>uplink", Value: 100},
	}})
	assertTrafficDelta(t, first, 11, 100, 250)
	if err := collector.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	cursor := readCursorFile(t, cursorFile)
	if cursor["user>>>u_11>>>traffic>>>uplink"] != 100 || cursor["user>>>u_11>>>traffic>>>downlink"] != 250 {
		t.Fatalf("cursor = %+v", cursor)
	}
	if _, ok := cursor["user>>>u_abc>>>traffic>>>uplink"]; ok {
		t.Fatalf("cursor contains invalid stat name: %+v", cursor)
	}

	second := collector.collectFromResponse(&statscmd.QueryStatsResponse{Stat: []*statscmd.Stat{
		{Name: "user>>>u_11>>>traffic>>>uplink", Value: 150},
		{Name: "user>>>u_11>>>traffic>>>downlink", Value: 200},
	}})
	assertTrafficDelta(t, second, 11, 50, 200)
}

func TestGRPCStatsCollectorIgnoresNullCursorFile(t *testing.T) {
	cursorFile := filepath.Join(t.TempDir(), "traffic_cursor.json")
	if err := os.WriteFile(cursorFile, []byte(`null`), 0o600); err != nil {
		t.Fatalf("write cursor: %v", err)
	}
	collector := NewGRPCStatsCollector("127.0.0.1:10085", cursorFile)

	deltas := collector.collectFromResponse(&statscmd.QueryStatsResponse{Stat: []*statscmd.Stat{
		{Name: "user>>>u_11>>>traffic>>>uplink", Value: 100},
	}})
	assertTrafficDelta(t, deltas, 11, 100, 0)
	if err := collector.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func assertTrafficDelta(t *testing.T, deltas []TrafficDelta, subscriptionID int64, upload uint64, download uint64) {
	t.Helper()
	for _, delta := range deltas {
		if delta.SubscriptionID == subscriptionID {
			if delta.UploadBytes != upload || delta.DownloadBytes != download {
				t.Fatalf("delta = %+v, want upload=%d download=%d", delta, upload, download)
			}
			return
		}
	}
	t.Fatalf("subscription %d not found in %+v", subscriptionID, deltas)
}

func readCursorFile(t *testing.T, path string) map[string]int64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	var cursor map[string]int64
	if err := json.Unmarshal(raw, &cursor); err != nil {
		t.Fatalf("decode cursor: %v\n%s", err, raw)
	}
	return cursor
}
