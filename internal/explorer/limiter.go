package explorer

import (
	"context"
	"sync"
	"time"
)

// rateLimit is a token bucket: reads per minute, with a small burst.
type rateLimit struct {
	mu       sync.Mutex
	perMin   float64
	burst    float64
	tokens   float64
	lastFill time.Time
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

// NewRateLimit makes a budget of perMinute reads a minute. A burst of
// reads up to burst may go at once after a quiet spell.
func NewRateLimit(perMinute float64, burst int) RateLimit {
	if burst < 1 {
		burst = 1
	}
	return &rateLimit{
		perMin: perMinute, burst: float64(burst), tokens: float64(burst),
		lastFill: time.Now(), now: time.Now, sleep: sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wait implements RateLimit.
func (l *rateLimit) Wait(ctx context.Context, n int) error {
	if n <= 0 || l.perMin <= 0 {
		return nil
	}
	for {
		l.mu.Lock()
		now := l.now()
		l.tokens = min(l.burst, l.tokens+l.perMin*now.Sub(l.lastFill).Minutes())
		l.lastFill = now
		need := float64(n) - l.tokens
		if need <= 0 {
			l.tokens -= float64(n)
			l.mu.Unlock()
			return nil
		}
		// Take what there is and wait for the rest, so a request larger
		// than the burst still goes, after its due time.
		l.tokens = 0
		n = int(need + 0.5)
		if n < 1 {
			n = 1
		}
		wait := time.Duration(float64(n) / l.perMin * float64(time.Minute))
		l.mu.Unlock()
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
		l.mu.Lock()
		// The wait earned the rest; spend it.
		l.tokens = min(l.burst, l.tokens+l.perMin*l.now().Sub(l.lastFill).Minutes())
		l.lastFill = l.now()
		l.tokens = max(0, l.tokens-float64(n))
		l.mu.Unlock()
		return nil
	}
}
