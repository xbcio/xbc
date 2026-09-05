package kafka

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	AckNone = "none"
	AckOne  = "one"
	AckAll  = "all"

	CompressionNone   = "none"
	CompressionGzip   = "gzip"
	CompressionSnappy = "snappy"
	CompressionLZ4    = "lz4"
	CompressionZstd   = "zstd"

	SASLPlain       = "plain"
	SASLSCRAMSHA256 = "scram-sha-256"
	SASLSCRAMSHA512 = "scram-sha-512"

	BalanceRange      = "range"
	BalanceRoundRobin = "round_robin"

	OffsetEarliest = "earliest"
	OffsetLatest   = "latest"

	ErrorPolicyStop = "stop"
	ErrorPolicySkip = "skip"
)

// Config configures one named Kafka cluster connection. Credentials are kept
// as ordinary configuration values so XBC's secret sources can populate them;
// this package never logs or includes them in errors.
type Config struct {
	Brokers  []string `yaml:"brokers" validate:"required,min=1,dive,required"`
	ClientID string   `yaml:"client_id" default:"xbc"`

	DialTimeout time.Duration             `yaml:"dial_timeout" default:"10s" validate:"gt=0"`
	TLS         TLSConfig                 `yaml:"tls"`
	SASL        SASLConfig                `yaml:"sasl"`
	Producer    ProducerConfig            `yaml:"producer"`
	Consumers   map[string]ConsumerConfig `yaml:"consumers"`
}

// TLSConfig enables TLS 1.2 or newer and optionally loads a private CA and a
// client certificate. CertFile and KeyFile must be configured together.
type TLSConfig struct {
	Enabled    bool   `yaml:"enabled" default:"false"`
	ServerName string `yaml:"server_name"`
	CAFile     string `yaml:"ca_file"`
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
}

// SASLConfig supports PLAIN and SCRAM. SASL is accepted only with TLS enabled
// to prevent credentials from crossing the network without encryption.
type SASLConfig struct {
	Mechanism string `yaml:"mechanism"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
}

// ProducerConfig controls batching and delivery guarantees. Topic may be left
// empty when every Message supplied to Produce carries its own topic.
type ProducerConfig struct {
	Topic        string        `yaml:"topic"`
	BatchSize    int           `yaml:"batch_size" default:"100" validate:"gt=0"`
	BatchBytes   int           `yaml:"batch_bytes" default:"1048576" validate:"gt=0"`
	BatchTimeout time.Duration `yaml:"batch_timeout" default:"1s" validate:"gt=0"`
	RequiredAcks string        `yaml:"required_acks" default:"all" validate:"oneof=none one all"`
	Compression  string        `yaml:"compression" default:"none" validate:"oneof=none gzip snappy lz4 zstd"`
	ReadTimeout  time.Duration `yaml:"read_timeout" default:"10s" validate:"gt=0"`
	WriteTimeout time.Duration `yaml:"write_timeout" default:"10s" validate:"gt=0"`
	MaxAttempts  int           `yaml:"max_attempts" default:"10" validate:"gt=0"`
}

// ConsumerConfig describes one consumer loop. Its map key under consumers is
// also the handler registration name. Sequential dispatch intentionally makes
// QueueCapacity the hard backpressure boundary between Kafka and application
// handlers. Offset commits are synchronous so broker failures remain visible
// to retry and error-policy handling.
type ConsumerConfig struct {
	GroupID string   `yaml:"group_id" validate:"required"`
	Topics  []string `yaml:"topics" validate:"required,min=1,dive,required"`

	QueueCapacity          int           `yaml:"queue_capacity" default:"100" validate:"gt=0"`
	MinBytes               int           `yaml:"min_bytes" default:"1" validate:"gt=0"`
	MaxBytes               int           `yaml:"max_bytes" default:"10485760" validate:"gt=0"`
	MaxWait                time.Duration `yaml:"max_wait" default:"10s" validate:"gt=0"`
	HeartbeatInterval      time.Duration `yaml:"heartbeat_interval" default:"3s" validate:"gt=0"`
	SessionTimeout         time.Duration `yaml:"session_timeout" default:"30s" validate:"gt=0"`
	RebalanceTimeout       time.Duration `yaml:"rebalance_timeout" default:"30s" validate:"gt=0"`
	WatchPartitionChanges  bool          `yaml:"watch_partition_changes" default:"true"`
	PartitionWatchInterval time.Duration `yaml:"partition_watch_interval" default:"5s" validate:"gt=0"`
	BalanceStrategy        string        `yaml:"balance_strategy" default:"range" validate:"oneof=range round_robin"`
	StartOffset            string        `yaml:"start_offset" default:"earliest" validate:"oneof=earliest latest"`

	HandlerMaxAttempts  int           `yaml:"handler_max_attempts" default:"3" validate:"gt=0"`
	HandlerRetryBackoff time.Duration `yaml:"handler_retry_backoff" default:"250ms" validate:"gt=0"`
	FetchErrorBackoff   time.Duration `yaml:"fetch_error_backoff" default:"1s" validate:"gt=0"`
	ErrorPolicy         string        `yaml:"error_policy" default:"stop" validate:"oneof=stop skip"`
}

type normalizedConfig struct {
	Config
	consumers map[string]ConsumerConfig
}

func defaultConfig() Config {
	return Config{
		ClientID:    "xbc",
		DialTimeout: 10 * time.Second,
		Producer:    defaultProducerConfig(),
		Consumers:   make(map[string]ConsumerConfig),
	}
}

func defaultProducerConfig() ProducerConfig {
	return ProducerConfig{
		BatchSize:    100,
		BatchBytes:   1 << 20,
		BatchTimeout: time.Second,
		RequiredAcks: AckAll,
		Compression:  CompressionNone,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		MaxAttempts:  10,
	}
}

func defaultConsumerConfig() ConsumerConfig {
	return ConsumerConfig{
		QueueCapacity:          100,
		MinBytes:               1,
		MaxBytes:               10 << 20,
		MaxWait:                10 * time.Second,
		HeartbeatInterval:      3 * time.Second,
		SessionTimeout:         30 * time.Second,
		RebalanceTimeout:       30 * time.Second,
		WatchPartitionChanges:  true,
		PartitionWatchInterval: 5 * time.Second,
		BalanceStrategy:        BalanceRange,
		StartOffset:            OffsetEarliest,
		HandlerMaxAttempts:     3,
		HandlerRetryBackoff:    250 * time.Millisecond,
		FetchErrorBackoff:      time.Second,
		ErrorPolicy:            ErrorPolicyStop,
	}
}

// Validate checks defaults and semantic relationships that struct tags cannot
// express. It never renders SASL secrets in returned errors.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	d := defaultConfig()
	if strings.TrimSpace(c.ClientID) == "" {
		c.ClientID = d.ClientID
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = d.DialTimeout
	}
	applyProducerDefaults(&c.Producer)

	if len(c.Brokers) == 0 {
		return normalizedConfig{}, errors.New("kafka: at least one broker is required")
	}
	seen := make(map[string]struct{}, len(c.Brokers))
	for i, raw := range c.Brokers {
		broker := strings.TrimSpace(raw)
		if broker == "" {
			return normalizedConfig{}, fmt.Errorf("kafka: broker %d is empty", i)
		}
		if _, _, err := net.SplitHostPort(broker); err != nil {
			return normalizedConfig{}, fmt.Errorf("kafka: broker %q must be host:port: %w", broker, err)
		}
		if _, ok := seen[broker]; ok {
			return normalizedConfig{}, fmt.Errorf("kafka: broker %q is duplicated", broker)
		}
		seen[broker] = struct{}{}
		c.Brokers[i] = broker
	}
	if c.DialTimeout <= 0 {
		return normalizedConfig{}, errors.New("kafka: dial_timeout must be positive")
	}
	if err := validateTLSAndSASL(c.TLS, c.SASL); err != nil {
		return normalizedConfig{}, err
	}
	if err := validateProducer(c.Producer); err != nil {
		return normalizedConfig{}, err
	}

	consumers := make(map[string]ConsumerConfig, len(c.Consumers))
	for rawName, value := range c.Consumers {
		name := strings.TrimSpace(rawName)
		if name == "" {
			return normalizedConfig{}, errors.New("kafka: consumer name cannot be empty")
		}
		if name != rawName {
			return normalizedConfig{}, fmt.Errorf("kafka: consumer name %q cannot have surrounding whitespace", rawName)
		}
		applyConsumerDefaults(&value)
		if err := validateConsumer(name, value); err != nil {
			return normalizedConfig{}, err
		}
		consumers[name] = value
	}
	c.Consumers = consumers
	return normalizedConfig{Config: c, consumers: consumers}, nil
}

func applyProducerDefaults(c *ProducerConfig) {
	d := defaultProducerConfig()
	if c.BatchSize == 0 {
		c.BatchSize = d.BatchSize
	}
	if c.BatchBytes == 0 {
		c.BatchBytes = d.BatchBytes
	}
	if c.BatchTimeout == 0 {
		c.BatchTimeout = d.BatchTimeout
	}
	if strings.TrimSpace(c.RequiredAcks) == "" {
		c.RequiredAcks = d.RequiredAcks
	}
	if strings.TrimSpace(c.Compression) == "" {
		c.Compression = d.Compression
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = d.ReadTimeout
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = d.WriteTimeout
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = d.MaxAttempts
	}
	c.RequiredAcks = strings.ToLower(strings.TrimSpace(c.RequiredAcks))
	c.Compression = strings.ToLower(strings.TrimSpace(c.Compression))
}

func applyConsumerDefaults(c *ConsumerConfig) {
	d := defaultConsumerConfig()
	if c.QueueCapacity == 0 {
		c.QueueCapacity = d.QueueCapacity
	}
	if c.MinBytes == 0 {
		c.MinBytes = d.MinBytes
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = d.MaxBytes
	}
	if c.MaxWait == 0 {
		c.MaxWait = d.MaxWait
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = d.HeartbeatInterval
	}
	if c.SessionTimeout == 0 {
		c.SessionTimeout = d.SessionTimeout
	}
	if c.RebalanceTimeout == 0 {
		c.RebalanceTimeout = d.RebalanceTimeout
	}
	if c.PartitionWatchInterval == 0 {
		c.PartitionWatchInterval = d.PartitionWatchInterval
	}
	if strings.TrimSpace(c.BalanceStrategy) == "" {
		c.BalanceStrategy = d.BalanceStrategy
	}
	if strings.TrimSpace(c.StartOffset) == "" {
		c.StartOffset = d.StartOffset
	}
	if c.HandlerMaxAttempts == 0 {
		c.HandlerMaxAttempts = d.HandlerMaxAttempts
	}
	if c.HandlerRetryBackoff == 0 {
		c.HandlerRetryBackoff = d.HandlerRetryBackoff
	}
	if c.FetchErrorBackoff == 0 {
		c.FetchErrorBackoff = d.FetchErrorBackoff
	}
	if strings.TrimSpace(c.ErrorPolicy) == "" {
		c.ErrorPolicy = d.ErrorPolicy
	}
	c.BalanceStrategy = strings.ToLower(strings.TrimSpace(c.BalanceStrategy))
	c.StartOffset = strings.ToLower(strings.TrimSpace(c.StartOffset))
	c.ErrorPolicy = strings.ToLower(strings.TrimSpace(c.ErrorPolicy))
}

func validateTLSAndSASL(t TLSConfig, s SASLConfig) error {
	if (t.CertFile == "") != (t.KeyFile == "") {
		return errors.New("kafka: tls cert_file and key_file must be configured together")
	}
	mechanism := strings.ToLower(strings.TrimSpace(s.Mechanism))
	credentials := strings.TrimSpace(s.Username) != "" || s.Password != ""
	if mechanism == "" {
		if credentials {
			return errors.New("kafka: sasl mechanism is required when credentials are configured")
		}
		return nil
	}
	if !t.Enabled {
		return errors.New("kafka: sasl requires tls.enabled=true")
	}
	if strings.TrimSpace(s.Username) == "" || s.Password == "" {
		return errors.New("kafka: sasl username and password are required")
	}
	switch mechanism {
	case SASLPlain, SASLSCRAMSHA256, SASLSCRAMSHA512:
		return nil
	default:
		return fmt.Errorf("kafka: unsupported sasl mechanism %q", mechanism)
	}
}

func validateProducer(c ProducerConfig) error {
	if c.BatchSize <= 0 || c.BatchBytes <= 0 || c.BatchTimeout <= 0 {
		return errors.New("kafka: producer batch_size, batch_bytes, and batch_timeout must be positive")
	}
	if c.ReadTimeout <= 0 || c.WriteTimeout <= 0 || c.MaxAttempts <= 0 {
		return errors.New("kafka: producer timeouts and max_attempts must be positive")
	}
	switch c.RequiredAcks {
	case AckNone, AckOne, AckAll:
	default:
		return fmt.Errorf("kafka: unsupported required_acks %q", c.RequiredAcks)
	}
	switch c.Compression {
	case CompressionNone, CompressionGzip, CompressionSnappy, CompressionLZ4, CompressionZstd:
	default:
		return fmt.Errorf("kafka: unsupported compression %q", c.Compression)
	}
	return nil
}

func validateConsumer(name string, c ConsumerConfig) error {
	if strings.TrimSpace(c.GroupID) == "" {
		return fmt.Errorf("kafka: consumer %q group_id is required", name)
	}
	if len(c.Topics) == 0 {
		return fmt.Errorf("kafka: consumer %q requires at least one topic", name)
	}
	for i, topic := range c.Topics {
		if strings.TrimSpace(topic) == "" {
			return fmt.Errorf("kafka: consumer %q topic %d is empty", name, i)
		}
	}
	if c.QueueCapacity <= 0 || c.MinBytes <= 0 || c.MaxBytes <= 0 || c.MaxBytes < c.MinBytes {
		return fmt.Errorf("kafka: consumer %q has invalid queue/min/max byte limits", name)
	}
	if c.MaxWait <= 0 || c.HeartbeatInterval <= 0 || c.SessionTimeout <= 0 || c.RebalanceTimeout <= 0 || c.PartitionWatchInterval <= 0 {
		return fmt.Errorf("kafka: consumer %q has invalid timing configuration", name)
	}
	if c.HandlerMaxAttempts <= 0 || c.HandlerRetryBackoff <= 0 || c.FetchErrorBackoff <= 0 {
		return fmt.Errorf("kafka: consumer %q retry settings must be positive", name)
	}
	switch c.BalanceStrategy {
	case BalanceRange, BalanceRoundRobin:
	default:
		return fmt.Errorf("kafka: consumer %q has unsupported balance_strategy %q", name, c.BalanceStrategy)
	}
	switch c.StartOffset {
	case OffsetEarliest, OffsetLatest:
	default:
		return fmt.Errorf("kafka: consumer %q has unsupported start_offset %q", name, c.StartOffset)
	}
	switch c.ErrorPolicy {
	case ErrorPolicyStop, ErrorPolicySkip:
	default:
		return fmt.Errorf("kafka: consumer %q has unsupported error_policy %q", name, c.ErrorPolicy)
	}
	return nil
}
