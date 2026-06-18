package app

import (
	"sync"
	"time"

	xrayruntime "github.com/perfect-panel/moment/xray-agent/internal/xray"
)

type onlineUserTracker struct {
	ttl time.Duration

	mu       sync.Mutex
	lastSeen map[int64]time.Time
}

func newOnlineUserTracker(ttl time.Duration) *onlineUserTracker {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &onlineUserTracker{
		ttl:      ttl,
		lastSeen: make(map[int64]time.Time),
	}
}

func (t *onlineUserTracker) Observe(deltas []xrayruntime.TrafficDelta, now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)
	for _, delta := range deltas {
		if delta.SubscriptionID <= 0 || (delta.UploadBytes == 0 && delta.DownloadBytes == 0) {
			continue
		}
		t.lastSeen[delta.SubscriptionID] = now
	}
}

func (t *onlineUserTracker) Count(now time.Time) uint64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)
	return uint64(len(t.lastSeen))
}

func (t *onlineUserTracker) pruneLocked(now time.Time) {
	cutoff := now.Add(-t.ttl)
	for subscriptionID, seenAt := range t.lastSeen {
		if seenAt.Before(cutoff) {
			delete(t.lastSeen, subscriptionID)
		}
	}
}
