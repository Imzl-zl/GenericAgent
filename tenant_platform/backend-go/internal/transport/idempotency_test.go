package transport

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// newTestCache builds an idempotencyCache with overridden TTL and shard cap so
// tests can exercise expiry and eviction without waiting 30 minutes or pushing
// 4096 entries. Production code uses newIdempotencyCache (defaults).
func newTestCache(ttl time.Duration, maxPerShard int) *idempotencyCache {
	c := &idempotencyCache{
		shards:      make([]idempotencyShard, defaultIdempotencyShards),
		shardMask:   defaultIdempotencyShards - 1,
		ttl:         ttl,
		maxPerShard: maxPerShard,
	}
	for i := range c.shards {
		c.shards[i].seen = make(map[string]time.Time)
	}
	return c
}

func TestIdempotencyFirstSeenReturnsTrue(t *testing.T) {
	c := newTestCache(time.Minute, 100)
	if !c.Record("bot-1", "msg-1") {
		t.Fatal("first record should return true")
	}
	if c.Record("bot-1", "msg-1") {
		t.Fatal("duplicate record should return false")
	}
}

func TestIdempotencyDifferentBotsIndependent(t *testing.T) {
	c := newTestCache(time.Minute, 100)
	if !c.Record("bot-1", "msg-1") {
		t.Fatal("bot-1 msg-1 first")
	}
	if !c.Record("bot-2", "msg-1") {
		t.Fatal("bot-2 msg-1 should be independent of bot-1")
	}
	// Same message id under different bots must not collide.
	if c.Record("bot-2", "msg-1") {
		t.Fatal("bot-2 msg-1 duplicate")
	}
}

func TestIdempotencyTTLExpiryReAdmits(t *testing.T) {
	c := newTestCache(50*time.Millisecond, 100)
	if !c.Record("bot-1", "msg-1") {
		t.Fatal("first")
	}
	if c.Record("bot-1", "msg-1") {
		t.Fatal("duplicate within TTL")
	}
	time.Sleep(60 * time.Millisecond)
	// After TTL expires the entry is eligible for re-admission. In production
	// this is rare (iLink doesn't replay 30-min-old messages), but the cache
	// must not permanently block a key after TTL.
	if !c.Record("bot-1", "msg-1") {
		t.Fatal("should re-admit after TTL expiry")
	}
}

func TestIdempotencyEvictionCapBounded(t *testing.T) {
	// Force every key into the same shard by using a 1-shard cache. Achieved
	// by overriding shardMask to 0 via a custom construction.
	c := &idempotencyCache{
		shards:      make([]idempotencyShard, 1),
		shardMask:   0,
		ttl:         time.Minute,
		maxPerShard: 3,
	}
	for i := range c.shards {
		c.shards[i].seen = make(map[string]time.Time)
	}
	// Fill the shard to capacity with distinct keys.
	for i := 0; i < 3; i++ {
		c.Record("bot-1", "msg-"+strconv.Itoa(i))
	}
	// Next write triggers evictExpired; with all entries live (not expired),
	// the oldest-expiring one is dropped to make room. The cache must not grow
	// beyond maxPerShard.
	c.Record("bot-1", "msg-new")
	s := &c.shards[0]
	s.mu.Lock()
	got := len(s.seen)
	s.mu.Unlock()
	if got > 3 {
		t.Fatalf("shard grew beyond cap: %d entries (max=3)", got)
	}
}

func TestIdempotencyConcurrentRecordsAreSafe(t *testing.T) {
	// Race detector + concurrent writers must not panic or corrupt state.
	c := newTestCache(time.Minute, 1000)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Record("bot-1", "msg-"+strconv.Itoa(n*100+j))
			}
		}(i)
	}
	wg.Wait()
}
