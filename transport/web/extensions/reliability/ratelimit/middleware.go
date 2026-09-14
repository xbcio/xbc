package ratelimit

import (
	"context"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/xbcio/xbc/transport/web"
)

const (
	clientIdleTTL = 10 * time.Minute
	sweepInterval = time.Minute
	maxInt        = int(^uint(0) >> 1)
)

type clientLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type limiterState struct {
	cfg    Config
	global *rate.Limiter

	clientsMu sync.Mutex
	clients   map[string]*clientLimiter
	lastSweep time.Time
}

func newLimiterState(cfg Config) (*limiterState, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	state := &limiterState{cfg: cfg}
	if cfg.Scope == ScopeGlobal {
		state.global = rate.NewLimiter(rate.Limit(cfg.Rate), cfg.Burst)
	} else {
		state.clients = make(map[string]*clientLimiter)
	}
	return state, nil
}

func (p *Plugin) handle(_ context.Context, c *web.Ctx) error {
	gc := c.Gin()
	p.mu.RLock()
	state := p.state
	p.mu.RUnlock()
	if state == nil {
		web.AbortProblem(gc, web.NewProblem(http.StatusInternalServerError, "internal_server_error"))
		return nil
	}

	now := time.Now()
	limiter := state.limiterFor(c, now)
	if limiter.AllowN(now, 1) {
		c.Next()
		return nil
	}

	c.SetHeader("Retry-After", strconv.Itoa(retryAfterSeconds(limiter, now)))
	web.AbortProblem(gc, web.NewProblem(http.StatusTooManyRequests, "rate_limit_exceeded"))
	return nil
}

func (s *limiterState) limiterFor(c *web.Ctx, now time.Time) *rate.Limiter {
	if s.cfg.Scope == ScopeGlobal {
		return s.global
	}

	key := clientIP(c)
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()

	entry := s.clients[key]
	if entry == nil {
		entry = &clientLimiter{limiter: rate.NewLimiter(rate.Limit(s.cfg.Rate), s.cfg.Burst)}
		s.clients[key] = entry
	}
	entry.lastSeen = now
	if s.lastSweep.IsZero() || now.Sub(s.lastSweep) >= sweepInterval {
		for ip, candidate := range s.clients {
			if now.Sub(candidate.lastSeen) > clientIdleTTL {
				delete(s.clients, ip)
			}
		}
		s.lastSweep = now
	}
	return entry.limiter
}

func clientIP(c *web.Ctx) string {
	// Phase 4 debt: ClientIP has no neutral RequestContext equivalent yet --
	// the spec's RequestContext method list omits it (see plan §2 debt
	// table) -- so it stays on the underlying gin.Context until that
	// contract is defined.
	gc := c.Gin()
	if ip := net.ParseIP(strings.TrimSpace(gc.ClientIP())); ip != nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(c.Request().RemoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	// Requests without a parseable peer address share a bucket rather than
	// receiving an accidental unlimited bypass.
	return "unknown"
}

func retryAfterSeconds(limiter *rate.Limiter, now time.Time) int {
	missing := 1 - limiter.TokensAt(now)
	if missing < 0 {
		missing = 0
	}
	seconds := math.Ceil(missing / float64(limiter.Limit()))
	if seconds < 1 {
		return 1
	}
	if seconds >= float64(maxInt) {
		return maxInt
	}
	return int(seconds)
}
