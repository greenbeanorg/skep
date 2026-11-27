package forward

import "sync"

// The replay cache (D25): in memory, each (origin, nonce) pair kept until
// 15 s after its envelope's expiry, at most MaxNonces pairs.
const (
	// MaxNonces bounds the cache; a full cache refuses with busy.
	MaxNonces = 100000
	// nonceGraceMillis is how long a pair is kept after its expiry.
	nonceGraceMillis = 15000
)

type nonceCache struct {
	mu    sync.Mutex
	until map[string]int64
	max   int
}

func newNonceCache(max int) *nonceCache {
	return &nonceCache{until: map[string]int64{}, max: max}
}

// add records the pair, unless it is already there (replay) or the cache
// is full of pairs that are still kept (busy). nowMillis is the
// receiver's clock.
func (c *nonceCache) add(origin, nonce string, expiresAt, nowMillis int64) string {
	key := origin + "/" + nonce
	c.mu.Lock()
	defer c.mu.Unlock()
	if until, ok := c.until[key]; ok && until > nowMillis {
		return CodeReplay
	}
	if len(c.until) >= c.max {
		for k, until := range c.until {
			if until <= nowMillis {
				delete(c.until, k)
			}
		}
		if len(c.until) >= c.max {
			return CodeBusy
		}
	}
	c.until[key] = expiresAt + nonceGraceMillis
	return ""
}

func (c *nonceCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.until)
}
