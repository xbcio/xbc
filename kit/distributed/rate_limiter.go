package distributed

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// rateLimiterModeReserve advances the schedule whether or not the slot is
	// available now; the reserved caller is told how long to wait.
	rateLimiterModeReserve = "reserve"
	// rateLimiterModeAllow advances the schedule only when the slot is
	// available now.
	rateLimiterModeAllow = "allow"
)

// rateLimiterScript runs one GCRA step against the Redis server's clock.
//
// GCRA keeps a theoretical arrival time (TAT) per name. A request arriving at
// t may run without waiting when t >= TAT - tolerance, where the tolerance is
// one emission interval short of the window: that is what admits exactly limit
// requests at the start of a window. Reserving advances TAT to
// max(TAT, t) + interval, so a later request observes the earlier reservation.
// In allow mode the state is left untouched when the request would have to
// wait.
//
// The clock is read with TIME inside the script rather than in Go: the Redis
// server's clock is the one clock every replica shares, and reading it in the
// same script that reads and writes TAT makes a reservation atomic with the
// instant it was scheduled against. The stored TAT keeps a fractional
// millisecond part, so an emission interval that is not a whole number of
// milliseconds is not rounded the same way on every step and does not drift.
//
// The reply is { wait in microseconds, recovered flag }. The flag is set when
// the key held an unreadable value or another Redis type and was reset, which
// the caller logs.
var rateLimiterScript = redis.NewScript(`
redis.replicate_commands()

local key       = KEYS[1]
local interval  = tonumber(ARGV[1])
local tolerance = tonumber(ARGV[2])
local ttl       = tonumber(ARGV[3])
local allowOnly = ARGV[4] == 'allow'

-- A wait this small is a rounding artifact of subtracting two epoch-sized
-- numbers; treating it as zero keeps a burst at exactly limit requests.
local epsilon = 0.01

local typ = redis.call('TYPE', key).ok
local recovered = 0
if typ ~= 'none' and typ ~= 'string' then
    redis.call('DEL', key)
    typ = 'none'
    recovered = 1
end

local timeReply = redis.call('TIME')
local now = tonumber(timeReply[1]) * 1000 + tonumber(timeReply[2]) / 1000

local tat = now
if typ == 'string' then
    local parsed = tonumber(redis.call('GET', key))
    if parsed == nil then
        recovered = 1
    else
        tat = parsed
    end
end

local wait = tat - tolerance - now
if wait < epsilon then
    wait = 0
end
if allowOnly and wait > 0 then
    return { string.format('%.3f', wait * 1000), recovered }
end

local nextTat = math.max(tat, now) + interval
redis.call('SET', key, string.format('%.6f', nextTat), 'PX', ttl)
return { string.format('%.3f', wait * 1000), recovered }
`)

// RateLimiterOptions configures one rate limiter. The zero value is valid: it
// uses the default prefix and discards diagnostics.
type RateLimiterOptions struct {
	// Prefix namespaces every key this rate limiter uses. It defaults to
	// DefaultPrefix. Two deployments that share a Redis instance and prefix
	// share quotas; give each logical cluster its own prefix. A prefix that is
	// not safe in a Redis key is rejected with ErrInvalidOption.
	Prefix string
	// Logger receives a warning when a key holding another Redis type or an
	// unreadable value is reset, and diagnosis after that reset. It defaults to
	// discarding diagnostics.
	Logger Logger
}

// RateLimiter paces one named operation across every process that shares the
// Redis instance.
//
// One Redis key per name holds a theoretical arrival time (TAT) in Unix
// milliseconds. A reservation advances TAT by the emission interval
// per/limit, and a request may run as soon as the current TAT minus one
// emission interval has passed. The first limit reservations in a window are
// therefore admitted at once, and every later reservation is scheduled one
// interval after the previous one: a burst cannot exceed limit, and the
// long-term rate cannot exceed limit per per. The clock is the Redis server's
// TIME, read inside the same script that updates TAT, so every replica
// schedules against one clock and concurrent reservations cannot over-admit.
//
// The state is a single key that expires after twice per; every reservation
// refreshes that expiry. An abandoned name therefore does not leak memory, at
// the cost of forgetting reservation debt that is not touched for twice per.
//
// There is no process-wide registry: two RateLimiter values with the same
// prefix and name operate on the same schedule, which is what makes the limit
// cluster-wide.
type RateLimiter struct {
	client redis.UniversalClient
	log    Logger
	prefix string
}

// NewRateLimiter returns a rate limiter backed by client. Construction performs
// no I/O; Reserve, Wait, and Allow do. A nil client is rejected.
func NewRateLimiter(client redis.UniversalClient, options RateLimiterOptions) (*RateLimiter, error) {
	if client == nil {
		return nil, errors.New("distributed: NewRateLimiter requires a non-nil redis client")
	}
	prefix := strings.TrimSpace(options.Prefix)
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if !validMapName(prefix) {
		return nil, fmt.Errorf("%w: prefix %q must match %s", ErrInvalidOption, prefix, mapNamePattern)
	}
	logger := options.Logger
	if logger == nil {
		logger = nopLogger{}
	}
	return &RateLimiter{client: client, log: logger, prefix: prefix}, nil
}

// key is the Redis key holding one name's theoretical arrival time.
func (r *RateLimiter) key(name string) string {
	return r.prefix + ":rate:" + name
}

// Reserve reserves one slot of name and reports how long the caller must wait
// before running; zero means it may run now. A reservation consumes its slot
// immediately: a later caller observes this reservation's advance of the
// schedule even if this caller has not run yet.
//
// limit must be positive and is the number of slots per window; per must be
// positive and is the window. They are validated per call because they belong
// to the operation, not to the limiter value. Reserve reports ErrInvalidName
// when name is empty or has leading or trailing whitespace and ErrInvalidOption
// when limit or per is not positive.
func (r *RateLimiter) Reserve(ctx context.Context, name string, limit int, per time.Duration) (time.Duration, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := validateRateLimiterCall(name, limit, per); err != nil {
		return 0, err
	}
	wait, recovered, err := r.apply(ctx, name, limit, per, rateLimiterModeReserve)
	if err != nil {
		return 0, fmt.Errorf("distributed: rate limiter %q: reserve: %w", name, err)
	}
	r.logRecovery(name, recovered)
	return wait, nil
}

// Wait reserves one slot of name and blocks until it is this caller's turn.
// The slot is consumed at reservation time, so a context that ends during the
// wait returns ctx.Err() without returning the slot to the pool. Wait returns
// nil as soon as the reserved delay has elapsed; it performs no second Redis
// call.
//
// Errors follow Reserve's contract.
func (r *RateLimiter) Wait(ctx context.Context, name string, limit int, per time.Duration) error {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateRateLimiterCall(name, limit, per); err != nil {
		return err
	}
	wait, recovered, err := r.apply(ctx, name, limit, per, rateLimiterModeReserve)
	if err != nil {
		return fmt.Errorf("distributed: rate limiter %q: wait: %w", name, err)
	}
	r.logRecovery(name, recovered)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Allow consumes one slot of name and reports whether the caller may run now.
// When the next slot is not available yet it reports false and changes
// nothing, so the schedule observed by later callers is the same as if this
// call had not happened. Allow does not report the suggested delay; a caller
// that must retry should apply its own backoff.
//
// Errors follow Reserve's contract.
func (r *RateLimiter) Allow(ctx context.Context, name string, limit int, per time.Duration) (bool, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := validateRateLimiterCall(name, limit, per); err != nil {
		return false, err
	}
	wait, recovered, err := r.apply(ctx, name, limit, per, rateLimiterModeAllow)
	if err != nil {
		return false, fmt.Errorf("distributed: rate limiter %q: allow: %w", name, err)
	}
	r.logRecovery(name, recovered)
	return wait <= 0, nil
}

// validateRateLimiterCall enforces every per-call rule before any Redis
// operation.
func validateRateLimiterCall(name string, limit int, per time.Duration) error {
	if !validRateLimiterName(name) {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if limit <= 0 {
		return fmt.Errorf("%w: rate limiter %q limit must be positive, got %d", ErrInvalidOption, name, limit)
	}
	if per <= 0 {
		return fmt.Errorf("%w: rate limiter %q per must be positive, got %s", ErrInvalidOption, name, per)
	}
	return nil
}

// validRateLimiterName accepts any non-empty name without leading or trailing
// whitespace, the same rule a semaphore name follows.
func validRateLimiterName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name
}

// apply runs one script step and converts its reply. interval, tolerance, and
// the key TTL are derived from limit and per here so the script only carries
// arithmetic about one name.
func (r *RateLimiter) apply(ctx context.Context, name string, limit int, per time.Duration, mode string) (time.Duration, bool, error) {
	perMillis := float64(per) / float64(time.Millisecond)
	interval := perMillis / float64(limit)
	tolerance := perMillis - interval
	ttlMillis := int64(math.Ceil(2 * perMillis))
	if ttlMillis < 1 {
		ttlMillis = 1
	}
	reply, err := rateLimiterScript.Run(ctx, r.client, []string{r.key(name)},
		interval, tolerance, ttlMillis, mode,
	).Result()
	if err != nil {
		return 0, false, err
	}
	return parseRateLimiterReply(reply)
}

// parseRateLimiterReply converts the script's { microseconds, flag } reply.
func parseRateLimiterReply(reply interface{}) (time.Duration, bool, error) {
	parts, ok := reply.([]interface{})
	if !ok || len(parts) != 2 {
		return 0, false, fmt.Errorf("unexpected script reply %T", reply)
	}
	rawWait, ok := parts[0].(string)
	if !ok {
		return 0, false, fmt.Errorf("unexpected wait reply %T", parts[0])
	}
	micros, err := strconv.ParseFloat(rawWait, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse wait %q: %w", rawWait, err)
	}
	recovered := false
	switch flag := parts[1].(type) {
	case int64:
		recovered = flag != 0
	case string:
		recovered = flag != "0"
	default:
		return 0, false, fmt.Errorf("unexpected recovery flag %T", parts[1])
	}
	wait := time.Duration(math.Ceil(micros * float64(time.Microsecond)))
	if wait < 0 {
		wait = 0
	}
	return wait, recovered, nil
}

// logRecovery reports that the stored state was unreadable and had to be
// reset. The operation still succeeded; the warning is the only trace of the
// lost schedule.
func (r *RateLimiter) logRecovery(name string, recovered bool) {
	if recovered {
		r.log.Warn("distributed: rate limiter state was reset", "rate_limiter", name)
	}
}
