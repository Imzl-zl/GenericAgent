package transport

import (
	"hash/fnv"
	"sync"
	"time"
)

// Idempotency cache tunables. Defaults handle 10-20 active bots with room for
// bursts: 16 shards × 4096 entries = up to 65k concurrent dedup keys before
// eviction kicks in. TTL matches iLink's practical replay window.
const (
	defaultIdempotencyTTL         = 30 * time.Minute
	defaultIdempotencyShards      = 16
	defaultIdempotencyMaxPerShard = 4096
)

// idempotencyShard is one shard of the sharded dedup map. Each shard has its
// own mutex so concurrent messages on different bots don't contend.
type idempotencyShard struct {
	mu   sync.Mutex
	seen map[string]time.Time // key -> expireAt
}

// idempotencyCache is a sharded, TTL-bounded dedup cache. It replaces the
// unbounded global map+mutex in ILinkAdapter. Sharding reduces lock contention;
// TTL caps memory growth for long-running processes.
type idempotencyCache struct {
	shards      []idempotencyShard
	shardMask   int
	ttl         time.Duration
	maxPerShard int
}

func newIdempotencyCache() *idempotencyCache {
	c := &idempotencyCache{
		shards:      make([]idempotencyShard, defaultIdempotencyShards),
		shardMask:   defaultIdempotencyShards - 1,
		ttl:         defaultIdempotencyTTL,
		maxPerShard: defaultIdempotencyMaxPerShard,
	}
	for i := range c.shards {
		c.shards[i].seen = make(map[string]time.Time)
	}
	return c
}

// Record returns true the first time (botUUID, messageID) is seen within TTL.
// Subsequent calls within TTL return false. Expired entries are evicted lazily
// when a shard exceeds maxPerShard, so memory stays bounded without a background
// goroutine.
//
// Round8 审查: 拆分为 Check(只读)与 Mark(写入)——处理失败路径不得提前
// 消费消息, 否则 Poller 重试会被判 Duplicate 而永久丢消息。
func (c *idempotencyCache) Check(botUUID, messageID string) bool {
	key := botUUID + "|" + messageID
	idx := c.shardIndex(key)
	now := time.Now()
	s := &c.shards[idx]
	s.mu.Lock()
	defer s.mu.Unlock()
	if exp, ok := s.seen[key]; ok && now.Before(exp) {
		return true
	}
	return false
}

// Mark 记录 (botUUID, messageID) 已成功处理(幂等; TTL 内重复标记无害)。
func (c *idempotencyCache) Mark(botUUID, messageID string) {
	key := botUUID + "|" + messageID
	idx := c.shardIndex(key)
	now := time.Now()
	s := &c.shards[idx]
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) >= c.maxPerShard {
		c.evictExpired(s, now)
	}
	s.seen[key] = now.Add(c.ttl)
}

func (c *idempotencyCache) shardIndex(key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32()) & c.shardMask
}

// evictExpired removes TTL-expired entries from a shard. If the shard is still
// over capacity after removing expired entries, the earliest-expiring live
// entry is dropped (approximate LRU). Called under the shard's mutex.
func (c *idempotencyCache) evictExpired(s *idempotencyShard, now time.Time) {
	for k, exp := range s.seen {
		if !now.Before(exp) {
			delete(s.seen, k)
		}
	}
	if len(s.seen) < c.maxPerShard {
		return
	}
	var oldestKey string
	var oldestExp time.Time
	first := true
	for k, exp := range s.seen {
		if first || exp.Before(oldestExp) {
			oldestKey = k
			oldestExp = exp
			first = false
		}
	}
	delete(s.seen, oldestKey)
}
