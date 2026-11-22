package kafkax

import (
	"errors"
	"fmt"
	"strings"
	"time"

	kafkago "github.com/segmentio/kafka-go"
)

// connectionConfig is the connection-level subset shared by ReaderConfig and
// WriterConfig. It exists so both direct constructors normalize and validate a
// connection through one path instead of repeating the broker, TLS, and SASL
// rules.
type connectionConfig struct {
	Brokers            []string
	ClientID           string
	DialTimeout        time.Duration
	TLS                TLSConfig
	SASL               SASLConfig
	AllowInsecurePlain bool
	Dialer             *kafkago.Dialer
}

// ReaderConfig configures a reader built by NewReader. It exists for hosts that
// own a kafka-go *Reader without an XBC application; a plugin-managed consumer
// is described by ConsumerConfig instead.
//
// NormalizeReader fills defaults and validates this value. The defaults are
// kafka-go's documented ones, so a direct reader that leaves a field unset
// behaves like a bare kafka-go reader. Three useful plugin defaults differ:
// MaxBytes is 1,000,000 rather than the plugin's 10 MiB, partition watching is
// off rather than on, and an empty BalanceStrategy leaves kafka-go's
// [range, round_robin] offer list instead of restricting it to range.
type ReaderConfig struct {
	Brokers     []string
	ClientID    string
	DialTimeout time.Duration

	// TLS enables TLS 1.2 or newer, optionally with a private CA and a client
	// certificate. CertFile and KeyFile must be configured together.
	TLS TLSConfig

	// SASL supports PLAIN and SCRAM. PLAIN sends the password itself, so it is
	// accepted only with TLS enabled unless AllowInsecurePlain is set.
	SASL SASLConfig

	// AllowInsecurePlain permits SASL PLAIN without TLS. It is off by default
	// and the plugin path never permits this combination; enable it only to
	// preserve a legacy broker that accepts cleartext credentials. SCRAM never
	// needs it because it does not send the password.
	AllowInsecurePlain bool

	// Dialer overrides the connection dialer this package would build from
	// Brokers, ClientID, DialTimeout, TLS, and SASL. When it is set those
	// connection fields are ignored, and configuring both a Dialer and a TLS or
	// SASL block is rejected rather than silently preferring one. A custom
	// dialer owns its own transport security.
	Dialer *kafkago.Dialer

	// Topic is the single topic to read. GroupTopics takes precedence when both
	// are set with a GroupID.
	Topic       string
	GroupID     string
	GroupTopics []string

	// Partition selects direct partition consumption and is mutually exclusive
	// with GroupID. Zero selects partition 0.
	Partition int

	// StartOffset selects where a group without a committed offset begins.
	// OffsetEarliest (the default) maps to kafka-go's FirstOffset and
	// OffsetLatest maps to LastOffset. It is only used when GroupID is set.
	StartOffset string

	QueueCapacity          int
	MinBytes               int
	MaxBytes               int
	MaxWait                time.Duration
	HeartbeatInterval      time.Duration
	SessionTimeout         time.Duration
	RebalanceTimeout       time.Duration
	WatchPartitionChanges  bool
	PartitionWatchInterval time.Duration

	// BalanceStrategy restricts the group balancing strategies offered to the
	// coordinator: BalanceRange or BalanceRoundRobin. Empty, the default,
	// leaves kafka-go's range-then-round-robin offer list. It is only used when
	// GroupID is set.
	BalanceStrategy string

	// Logger and ErrorLogger receive kafka-go's internal diagnostics. A nil
	// logger keeps kafka-go's silent default.
	Logger      kafkago.Logger
	ErrorLogger kafkago.Logger
}

// WriterConfig configures a writer built by NewWriter. It exists for hosts that
// own a kafka-go *Writer without an XBC application; a plugin-managed producer
// is described by ProducerConfig instead.
//
// NormalizeWriter fills defaults and validates this value. The defaults match
// both kafka-go's documented writer defaults and the plugin's producer section.
type WriterConfig struct {
	Brokers     []string
	ClientID    string
	DialTimeout time.Duration

	// TLS enables TLS 1.2 or newer, optionally with a private CA and a client
	// certificate. CertFile and KeyFile must be configured together.
	TLS TLSConfig

	// SASL supports PLAIN and SCRAM. PLAIN sends the password itself, so it is
	// accepted only with TLS enabled unless AllowInsecurePlain is set.
	SASL SASLConfig

	// AllowInsecurePlain permits SASL PLAIN without TLS. It is off by default
	// and the plugin path never permits this combination; enable it only to
	// preserve a legacy broker that accepts cleartext credentials. SCRAM never
	// needs it because it does not send the password.
	AllowInsecurePlain bool

	// Dialer overrides the connection dialer this package would build from
	// Brokers, ClientID, DialTimeout, TLS, and SASL. When it is set those
	// connection fields are ignored, and configuring both a Dialer and a TLS or
	// SASL block is rejected rather than silently preferring one. A custom
	// dialer owns its own transport security.
	Dialer *kafkago.Dialer

	// Topic may be left empty when every written message carries its own topic.
	Topic string

	// Balancer distributes messages across partitions. Nil selects kafka-go's
	// round-robin balancer.
	Balancer kafkago.Balancer

	// Async makes WriteMessages return without waiting for delivery. Errors
	// from asynchronous writes never reach the caller.
	Async bool

	BatchSize    int
	BatchBytes   int
	BatchTimeout time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	RequiredAcks string
	Compression  string
	MaxAttempts  int

	// Logger and ErrorLogger receive kafka-go's internal diagnostics. A nil
	// logger keeps kafka-go's silent default.
	Logger      kafkago.Logger
	ErrorLogger kafkago.Logger
}

// NormalizeReader returns a copy of cfg with the defaults NewReader applies,
// without constructing a reader. A caller mapping an existing configuration
// can inspect the effective values first. The input is never modified.
func NormalizeReader(cfg ReaderConfig) (ReaderConfig, error) {
	out := cfg
	out.Brokers = append([]string(nil), cfg.Brokers...)
	out.GroupTopics = append([]string(nil), cfg.GroupTopics...)

	connection, err := normalizeConnection(connectionOfReader(cfg))
	if err != nil {
		return ReaderConfig{}, err
	}
	applyReaderConnection(&out, connection)

	if out.QueueCapacity == 0 {
		out.QueueCapacity = 100
	}
	if out.MinBytes == 0 {
		out.MinBytes = 1
	}
	if out.MaxBytes == 0 {
		out.MaxBytes = 1_000_000
	}
	if out.MaxWait == 0 {
		out.MaxWait = 10 * time.Second
	}
	if out.HeartbeatInterval == 0 {
		out.HeartbeatInterval = 3 * time.Second
	}
	if out.SessionTimeout == 0 {
		out.SessionTimeout = 30 * time.Second
	}
	if out.RebalanceTimeout == 0 {
		out.RebalanceTimeout = 30 * time.Second
	}
	if out.PartitionWatchInterval == 0 {
		out.PartitionWatchInterval = 5 * time.Second
	}
	if strings.TrimSpace(out.StartOffset) == "" {
		out.StartOffset = OffsetEarliest
	}
	out.StartOffset = strings.ToLower(strings.TrimSpace(out.StartOffset))
	out.BalanceStrategy = strings.ToLower(strings.TrimSpace(out.BalanceStrategy))
	out.Topic = strings.TrimSpace(out.Topic)
	for i, topic := range out.GroupTopics {
		out.GroupTopics[i] = strings.TrimSpace(topic)
	}

	if err := validateReader(out); err != nil {
		return ReaderConfig{}, err
	}
	return out, nil
}

// NormalizeWriter returns a copy of cfg with the defaults NewWriter applies,
// without constructing a writer. A caller mapping an existing configuration
// can inspect the effective values first. The input is never modified.
func NormalizeWriter(cfg WriterConfig) (WriterConfig, error) {
	out := cfg
	out.Brokers = append([]string(nil), cfg.Brokers...)

	connection, err := normalizeConnection(connectionOfWriter(cfg))
	if err != nil {
		return WriterConfig{}, err
	}
	applyWriterConnection(&out, connection)

	producer := defaultProducerConfig()
	if out.BatchSize == 0 {
		out.BatchSize = producer.BatchSize
	}
	if out.BatchBytes == 0 {
		out.BatchBytes = producer.BatchBytes
	}
	if out.BatchTimeout == 0 {
		out.BatchTimeout = producer.BatchTimeout
	}
	if out.ReadTimeout == 0 {
		out.ReadTimeout = producer.ReadTimeout
	}
	if out.WriteTimeout == 0 {
		out.WriteTimeout = producer.WriteTimeout
	}
	if out.MaxAttempts == 0 {
		out.MaxAttempts = producer.MaxAttempts
	}
	if strings.TrimSpace(out.RequiredAcks) == "" {
		out.RequiredAcks = producer.RequiredAcks
	}
	if strings.TrimSpace(out.Compression) == "" {
		out.Compression = producer.Compression
	}
	out.RequiredAcks = strings.ToLower(strings.TrimSpace(out.RequiredAcks))
	out.Compression = strings.ToLower(strings.TrimSpace(out.Compression))
	out.Topic = strings.TrimSpace(out.Topic)

	if err := validateWriter(out); err != nil {
		return WriterConfig{}, err
	}
	return out, nil
}

// NewReader constructs a kafka-go reader from cfg with the same connection
// security, validation, and defaults NormalizeReader reports. It exists for
// hosts that own a reader without an XBC application: the caller owns the
// returned value and releases it with Close, which kafka-go makes idempotent.
//
// The returned reader performs no commit by itself -- CommitInterval is 0, so
// ReadMessage commits synchronously and FetchMessage callers own the commit
// point through CommitMessages, matching the plugin's manual-commit model.
func NewReader(cfg ReaderConfig) (*kafkago.Reader, error) {
	prepared, err := NormalizeReader(cfg)
	if err != nil {
		return nil, err
	}
	dialer, err := connectionOfReader(prepared).dialer()
	if err != nil {
		return nil, err
	}
	readerConfig := kafkago.ReaderConfig{
		Brokers:                prepared.Brokers,
		Topic:                  prepared.Topic,
		GroupID:                prepared.GroupID,
		GroupTopics:            prepared.GroupTopics,
		Partition:              prepared.Partition,
		Dialer:                 dialer,
		QueueCapacity:          prepared.QueueCapacity,
		MinBytes:               prepared.MinBytes,
		MaxBytes:               prepared.MaxBytes,
		MaxWait:                prepared.MaxWait,
		CommitInterval:         0,
		HeartbeatInterval:      prepared.HeartbeatInterval,
		SessionTimeout:         prepared.SessionTimeout,
		RebalanceTimeout:       prepared.RebalanceTimeout,
		WatchPartitionChanges:  prepared.WatchPartitionChanges,
		PartitionWatchInterval: prepared.PartitionWatchInterval,
		GroupBalancers:         groupBalancers(prepared.BalanceStrategy),
		StartOffset:            startOffset(prepared.StartOffset),
		Logger:                 prepared.Logger,
		ErrorLogger:            prepared.ErrorLogger,
	}
	if err := (&readerConfig).Validate(); err != nil {
		return nil, fmt.Errorf("kafka: invalid reader configuration: %w", err)
	}
	return kafkago.NewReader(readerConfig), nil
}

// NewWriter constructs a kafka-go writer from cfg with the same connection
// security, validation, and defaults NormalizeWriter reports. It exists for
// hosts that own a writer without an XBC application: the caller owns the
// returned value and releases it with Close, which kafka-go makes idempotent.
func NewWriter(cfg WriterConfig) (*kafkago.Writer, error) {
	prepared, err := NormalizeWriter(cfg)
	if err != nil {
		return nil, err
	}
	dialer, err := connectionOfWriter(prepared).dialer()
	if err != nil {
		return nil, err
	}
	codec, err := compressionCodec(prepared.Compression)
	if err != nil {
		return nil, err
	}
	writerConfig := kafkago.WriterConfig{
		Brokers:          prepared.Brokers,
		Topic:            prepared.Topic,
		Dialer:           dialer,
		Balancer:         prepared.Balancer,
		BatchSize:        prepared.BatchSize,
		BatchBytes:       prepared.BatchBytes,
		BatchTimeout:     prepared.BatchTimeout,
		ReadTimeout:      prepared.ReadTimeout,
		WriteTimeout:     prepared.WriteTimeout,
		RequiredAcks:     requiredAcks(prepared.RequiredAcks),
		MaxAttempts:      prepared.MaxAttempts,
		CompressionCodec: codec,
		Async:            prepared.Async,
		Logger:           prepared.Logger,
		ErrorLogger:      prepared.ErrorLogger,
	}
	if err := (&writerConfig).Validate(); err != nil {
		return nil, fmt.Errorf("kafka: invalid writer configuration: %w", err)
	}
	return kafkago.NewWriter(writerConfig), nil
}

func normalizeConnection(cfg connectionConfig) (connectionConfig, error) {
	defaults := defaultConfig()
	cfg.Brokers = append([]string(nil), cfg.Brokers...)
	if strings.TrimSpace(cfg.ClientID) == "" {
		cfg.ClientID = defaults.ClientID
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaults.DialTimeout
	}
	brokers, err := normalizeBrokers(cfg.Brokers, false)
	if err != nil {
		return connectionConfig{}, err
	}
	cfg.Brokers = brokers
	if cfg.DialTimeout <= 0 {
		return connectionConfig{}, errors.New("kafka: dial_timeout must be positive")
	}
	if cfg.Dialer != nil {
		if cfg.TLS != (TLSConfig{}) || cfg.SASL != (SASLConfig{}) {
			return connectionConfig{}, errors.New("kafka: dialer cannot be combined with tls or sasl configuration")
		}
		return cfg, nil
	}
	if err := validateTLSAndSASLWith(cfg.TLS, cfg.SASL, cfg.AllowInsecurePlain); err != nil {
		return connectionConfig{}, err
	}
	return cfg, nil
}

func (cfg connectionConfig) dialer() (*kafkago.Dialer, error) {
	if cfg.Dialer != nil {
		return cfg.Dialer, nil
	}
	return buildDialer(Config{
		ClientID:    cfg.ClientID,
		DialTimeout: cfg.DialTimeout,
		TLS:         cfg.TLS,
		SASL:        cfg.SASL,
	})
}

func connectionOfReader(cfg ReaderConfig) connectionConfig {
	return connectionConfig{
		Brokers:            cfg.Brokers,
		ClientID:           cfg.ClientID,
		DialTimeout:        cfg.DialTimeout,
		TLS:                cfg.TLS,
		SASL:               cfg.SASL,
		AllowInsecurePlain: cfg.AllowInsecurePlain,
		Dialer:             cfg.Dialer,
	}
}

func connectionOfWriter(cfg WriterConfig) connectionConfig {
	return connectionConfig{
		Brokers:            cfg.Brokers,
		ClientID:           cfg.ClientID,
		DialTimeout:        cfg.DialTimeout,
		TLS:                cfg.TLS,
		SASL:               cfg.SASL,
		AllowInsecurePlain: cfg.AllowInsecurePlain,
		Dialer:             cfg.Dialer,
	}
}

func applyReaderConnection(cfg *ReaderConfig, connection connectionConfig) {
	cfg.Brokers = connection.Brokers
	cfg.ClientID = connection.ClientID
	cfg.DialTimeout = connection.DialTimeout
}

func applyWriterConnection(cfg *WriterConfig, connection connectionConfig) {
	cfg.Brokers = connection.Brokers
	cfg.ClientID = connection.ClientID
	cfg.DialTimeout = connection.DialTimeout
}

func validateReader(cfg ReaderConfig) error {
	if cfg.Partition < 0 {
		return errors.New("kafka: reader partition cannot be negative")
	}
	if cfg.GroupID != "" {
		if cfg.Partition != 0 {
			return errors.New("kafka: reader partition cannot be used with group_id")
		}
		if cfg.Topic == "" && len(cfg.GroupTopics) == 0 {
			return errors.New("kafka: reader group_id requires topic or group_topics")
		}
	} else {
		if len(cfg.GroupTopics) > 0 {
			return errors.New("kafka: reader group_topics requires group_id")
		}
		if cfg.Topic == "" {
			return errors.New("kafka: reader topic is required")
		}
	}
	for i, topic := range cfg.GroupTopics {
		if topic == "" {
			return fmt.Errorf("kafka: reader group_topics entry %d is empty", i)
		}
	}
	if cfg.QueueCapacity <= 0 || cfg.MinBytes <= 0 || cfg.MaxBytes <= 0 || cfg.MaxBytes < cfg.MinBytes {
		return errors.New("kafka: reader has invalid queue/min/max byte limits")
	}
	if cfg.MaxWait <= 0 || cfg.HeartbeatInterval <= 0 || cfg.SessionTimeout <= 0 || cfg.RebalanceTimeout <= 0 || cfg.PartitionWatchInterval <= 0 {
		return errors.New("kafka: reader has invalid timing configuration")
	}
	switch cfg.StartOffset {
	case OffsetEarliest, OffsetLatest:
	default:
		return fmt.Errorf("kafka: reader has unsupported start_offset %q", cfg.StartOffset)
	}
	switch cfg.BalanceStrategy {
	case "", BalanceRange, BalanceRoundRobin:
	default:
		return fmt.Errorf("kafka: reader has unsupported balance_strategy %q", cfg.BalanceStrategy)
	}
	return nil
}

func validateWriter(cfg WriterConfig) error {
	if cfg.BatchSize <= 0 || cfg.BatchBytes <= 0 || cfg.BatchTimeout <= 0 {
		return errors.New("kafka: writer batch_size, batch_bytes, and batch_timeout must be positive")
	}
	if cfg.ReadTimeout <= 0 || cfg.WriteTimeout <= 0 || cfg.MaxAttempts <= 0 {
		return errors.New("kafka: writer timeouts and max_attempts must be positive")
	}
	switch cfg.RequiredAcks {
	case AckNone, AckOne, AckAll:
	default:
		return fmt.Errorf("kafka: writer has unsupported required_acks %q", cfg.RequiredAcks)
	}
	switch cfg.Compression {
	case CompressionNone, CompressionGzip, CompressionSnappy, CompressionLZ4, CompressionZstd:
	default:
		return fmt.Errorf("kafka: writer has unsupported compression %q", cfg.Compression)
	}
	return nil
}

// groupBalancers maps the reader's strategy name to kafka-go's offer list. An
// empty strategy returns nil so kafka-go supplies its [range, round_robin]
// default instead of this package restricting the negotiation.
func groupBalancers(value string) []kafkago.GroupBalancer {
	switch value {
	case BalanceRange:
		return []kafkago.GroupBalancer{kafkago.RangeGroupBalancer{}}
	case BalanceRoundRobin:
		return []kafkago.GroupBalancer{kafkago.RoundRobinGroupBalancer{}}
	default:
		return nil
	}
}
