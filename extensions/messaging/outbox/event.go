package outbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
)

var (
	// ErrClosed means the service no longer accepts events.
	ErrClosed = errors.New("outbox: service is closed")
	// ErrTransactionRequired prevents accidentally writing outside the caller's
	// business transaction.
	ErrTransactionRequired = errors.New("outbox: a caller-owned GORM transaction is required")
)

// Event is a domain-neutral message. Enqueue and delivery take defensive
// copies of Payload and Headers.
type Event struct {
	ID         string
	Topic      string
	Key        string
	Payload    []byte
	Headers    map[string]string
	OccurredAt time.Time
}

// Clone returns a deep copy.
func (e Event) Clone() Event {
	e.Payload = append([]byte(nil), e.Payload...)
	if e.Headers != nil {
		source := e.Headers
		e.Headers = make(map[string]string, len(source))
		for key, value := range source {
			e.Headers[key] = value
		}
	}
	return e
}

// Publisher sends one event to an external broker or service. Implementations
// should use Event.ID as an idempotency key because every transactional outbox
// provides at-least-once, not exactly-once, delivery.
type Publisher interface {
	Publish(context.Context, Event) error
}

// PublisherFunc adapts a function to Publisher.
type PublisherFunc func(context.Context, Event) error

func (f PublisherFunc) Publish(ctx context.Context, event Event) error { return f(ctx, event) }

// Dispatcher is the transaction-facing business API.
type Dispatcher interface {
	Enqueue(context.Context, *gorm.DB, Event) (Event, error)
}

// Service inserts events through a Store while coordinating shutdown
// admission. It never starts or commits the supplied transaction.
type Service struct {
	store           Store
	maxPayloadBytes int
	config          Config
	publisher       Publisher

	closed      atomic.Bool
	admissionMu sync.Mutex
	inflight    sync.WaitGroup
	closeOnce   sync.Once
	closeDone   chan struct{}

	lifecycleMu sync.Mutex
	starting    bool
	started     bool
	opened      bool
	stopping    bool
	stopped     bool
	worker      *worker
	stopDone    chan struct{}
	stopErr     error
}

func newService(store Store, maxPayloadBytes int) *Service {
	config := defaultConfig()
	config.MaxPayloadBytes = maxPayloadBytes
	return newConfiguredService(config, store, nil)
}

func newConfiguredService(config Config, store Store, publisher Publisher) *Service {
	return &Service{
		store:           store,
		maxPayloadBytes: config.MaxPayloadBytes,
		config:          config,
		publisher:       publisher,
		closeDone:       make(chan struct{}),
	}
}

// Enqueue inserts event through tx. tx must be a caller-owned transaction;
// Service rejects nil and non-transactional GORM handles so a business write
// and its event cannot silently become separate commits.
func (s *Service) Enqueue(ctx context.Context, tx *gorm.DB, event Event) (Event, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if tx == nil {
		return Event{}, ErrTransactionRequired
	}
	if err := requireTransaction(tx); err != nil {
		return Event{}, err
	}
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	if !s.begin() {
		return Event{}, ErrClosed
	}
	defer s.inflight.Done()

	event = event.Clone()
	event.Topic = strings.TrimSpace(event.Topic)
	if event.Topic == "" {
		return Event{}, errors.New("outbox: event topic is required")
	}
	if len(event.Payload) > s.maxPayloadBytes {
		return Event{}, fmt.Errorf("outbox: event payload exceeds %d bytes", s.maxPayloadBytes)
	}
	if event.ID == "" {
		id, err := randomToken(16)
		if err != nil {
			return Event{}, fmt.Errorf("outbox: generate event id: %w", err)
		}
		event.ID = id
	} else if strings.TrimSpace(event.ID) != event.ID {
		return Event{}, errors.New("outbox: event id must not have surrounding whitespace")
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	} else {
		event.OccurredAt = event.OccurredAt.UTC()
	}
	if err := s.store.Insert(ctx, tx, event); err != nil {
		return Event{}, fmt.Errorf("outbox: enqueue event: %w", err)
	}
	return event.Clone(), nil
}

// Insert is an alias for Enqueue for code that prefers persistence wording.
func (s *Service) Insert(ctx context.Context, tx *gorm.DB, event Event) (Event, error) {
	return s.Enqueue(ctx, tx, event)
}

func (s *Service) begin() bool {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.closed.Load() {
		return false
	}
	s.inflight.Add(1)
	return true
}

// stopAdmission permanently rejects new events and returns a shared channel
// that closes after every event accepted before the transition has finished.
// Waiting is deliberately detached from any Stop caller's context: a caller
// deadline limits only that caller, not the one shared cleanup operation.
func (s *Service) stopAdmission() <-chan struct{} {
	s.closeOnce.Do(func() {
		s.admissionMu.Lock()
		s.closed.Store(true)
		s.admissionMu.Unlock()
		go func() {
			s.inflight.Wait()
			close(s.closeDone)
		}()
	})
	return s.closeDone
}

func randomToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
