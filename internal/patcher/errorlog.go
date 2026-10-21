package patcher

import (
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// maxErrorLogEntries bounds the deduplication cache.
const maxErrorLogEntries = 4096

// errorLog deduplicates evaluation error messages. The same error on the same
// object and the same expression is logged at most once per TTL, so that an
// informer resync does not flood the log.
type errorLog struct {
	mu      sync.Mutex
	seen    map[string]time.Time
	ttl     time.Duration
	clock   clock.Clock
	maxSize int
}

func newErrorLog(ttl time.Duration, c clock.Clock) *errorLog {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if c == nil {
		c = clock.RealClock{}
	}
	return &errorLog{seen: map[string]time.Time{}, ttl: ttl, clock: c, maxSize: maxErrorLogEntries}
}

// shouldLog reports whether this error has not been logged recently.
func (l *errorLog) shouldLog(uid, path, message string) bool {
	key := uid + "\x00" + path + "\x00" + message
	now := l.clock.Now()

	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.seen[key]; ok && now.Sub(last) < l.ttl {
		return false
	}
	if len(l.seen) >= l.maxSize {
		for k, last := range l.seen {
			if now.Sub(last) >= l.ttl {
				delete(l.seen, k)
			}
		}
		if len(l.seen) >= l.maxSize {
			l.seen = map[string]time.Time{}
		}
	}
	l.seen[key] = now
	return true
}
