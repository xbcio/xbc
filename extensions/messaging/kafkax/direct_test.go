package kafkax

import (
	"crypto/tls"
	"strings"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl/plain"
)

// testLogger is a comparable kafka-go Logger used to assert injection by
// identity; a LoggerFunc would panic when compared as an interface value.
type testLogger struct{}

func (*testLogger) Printf(string, ...any) {}

func TestNormalizeReaderAppliesDefaultsAndReturnsACopy(t *testing.T) {
	cfg := ReaderConfig{
		Brokers:     []string{" 127.0.0.1:9092 "},
		Topic:       " orders ",
		GroupID:     "workers",
		GroupTopics: []string{" events "},
	}

	normalized, err := NormalizeReader(cfg)
	if err != nil {
		t.Fatalf("NormalizeReader() error = %v", err)
	}
	if len(normalized.Brokers) != 1 || normalized.Brokers[0] != "127.0.0.1:9092" {
		t.Fatalf("normalized brokers = %q", normalized.Brokers)
	}
	if normalized.Topic != "orders" || len(normalized.GroupTopics) != 1 || normalized.GroupTopics[0] != "events" {
		t.Fatalf("normalized topics = %q, %q", normalized.Topic, normalized.GroupTopics)
	}
	if normalized.ClientID != "xbc" || normalized.DialTimeout != 10*time.Second {
		t.Fatalf("connection defaults = %q, %s", normalized.ClientID, normalized.DialTimeout)
	}
	if normalized.QueueCapacity != 100 || normalized.MinBytes != 1 || normalized.MaxBytes != 1_000_000 {
		t.Fatalf("byte defaults = %+v", normalized)
	}
	if normalized.MaxWait != 10*time.Second || normalized.HeartbeatInterval != 3*time.Second {
		t.Fatalf("timing defaults = %+v", normalized)
	}
	if normalized.SessionTimeout != 30*time.Second || normalized.RebalanceTimeout != 30*time.Second {
		t.Fatalf("group timing defaults = %+v", normalized)
	}
	if normalized.PartitionWatchInterval != 5*time.Second || normalized.WatchPartitionChanges {
		t.Fatalf("partition watch defaults = %+v", normalized)
	}
	if normalized.StartOffset != OffsetEarliest || normalized.BalanceStrategy != "" {
		t.Fatalf("delivery defaults = %q, %q", normalized.StartOffset, normalized.BalanceStrategy)
	}

	if cfg.Brokers[0] != " 127.0.0.1:9092 " || cfg.Topic != " orders " || cfg.GroupTopics[0] != " events " {
		t.Fatalf("NormalizeReader mutated its input: %+v", cfg)
	}
}

func TestNormalizeWriterAppliesDefaultsAndReturnsACopy(t *testing.T) {
	cfg := WriterConfig{Brokers: []string{" 127.0.0.1:9092 "}}

	normalized, err := NormalizeWriter(cfg)
	if err != nil {
		t.Fatalf("NormalizeWriter() error = %v", err)
	}
	if len(normalized.Brokers) != 1 || normalized.Brokers[0] != "127.0.0.1:9092" {
		t.Fatalf("normalized brokers = %q", normalized.Brokers)
	}
	if normalized.ClientID != "xbc" || normalized.DialTimeout != 10*time.Second {
		t.Fatalf("connection defaults = %q, %s", normalized.ClientID, normalized.DialTimeout)
	}
	if normalized.BatchSize != 100 || normalized.BatchBytes != 1<<20 || normalized.BatchTimeout != time.Second {
		t.Fatalf("batch defaults = %+v", normalized)
	}
	if normalized.ReadTimeout != 10*time.Second || normalized.WriteTimeout != 10*time.Second {
		t.Fatalf("timeout defaults = %+v", normalized)
	}
	if normalized.RequiredAcks != AckAll || normalized.Compression != CompressionNone || normalized.MaxAttempts != 10 {
		t.Fatalf("delivery defaults = %+v", normalized)
	}
	if cfg.Brokers[0] != " 127.0.0.1:9092 " {
		t.Fatalf("NormalizeWriter mutated its input: %q", cfg.Brokers[0])
	}
}

func TestReaderConfigValidation(t *testing.T) {
	const secret = "never-render-this-password"
	base := func() ReaderConfig {
		return ReaderConfig{Brokers: []string{"127.0.0.1:9092"}, Topic: "orders"}
	}
	tests := []struct {
		name string
		edit func(*ReaderConfig)
		want string
	}{
		{name: "brokers required", edit: func(c *ReaderConfig) { c.Brokers = nil }, want: "at least one broker"},
		{name: "broker host port", edit: func(c *ReaderConfig) { c.Brokers = []string{"localhost"} }, want: "host:port"},
		{name: "duplicate broker", edit: func(c *ReaderConfig) { c.Brokers = []string{"a:1", "a:1"} }, want: "duplicated"},
		{name: "topic required", edit: func(c *ReaderConfig) { c.Topic = " " }, want: "topic is required"},
		{name: "group topics require group", edit: func(c *ReaderConfig) { c.GroupTopics = []string{"orders"} }, want: "group_topics requires group_id"},
		{name: "group needs topics", edit: func(c *ReaderConfig) { c.Topic, c.GroupID = "", "workers" }, want: "requires topic or group_topics"},
		{name: "empty group topic", edit: func(c *ReaderConfig) {
			c.Topic, c.GroupID, c.GroupTopics = "", "workers", []string{" "}
		}, want: "entry 0 is empty"},
		{name: "partition with group", edit: func(c *ReaderConfig) { c.GroupID, c.Partition = "workers", 1 }, want: "partition cannot be used with group_id"},
		{name: "negative partition", edit: func(c *ReaderConfig) { c.Partition = -1 }, want: "partition cannot be negative"},
		{name: "start offset", edit: func(c *ReaderConfig) { c.StartOffset = "middle" }, want: "start_offset"},
		{name: "byte limits", edit: func(c *ReaderConfig) { c.MinBytes, c.MaxBytes = 20, 10 }, want: "byte limits"},
		{name: "queue capacity", edit: func(c *ReaderConfig) { c.QueueCapacity = -1 }, want: "byte limits"},
		{name: "timing", edit: func(c *ReaderConfig) { c.HeartbeatInterval = -1 }, want: "timing"},
		{name: "balance strategy", edit: func(c *ReaderConfig) { c.BalanceStrategy = "sticky" }, want: "balance_strategy"},
		{name: "plain requires tls", edit: func(c *ReaderConfig) {
			c.SASL = SASLConfig{Mechanism: SASLPlain, Username: "u", Password: secret}
		}, want: "requires tls"},
		{name: "unknown mechanism", edit: func(c *ReaderConfig) {
			c.SASL = SASLConfig{Mechanism: "bad", Username: "u", Password: secret}
		}, want: "unsupported sasl"},
		{name: "mechanism required", edit: func(c *ReaderConfig) { c.SASL = SASLConfig{Username: "u", Password: secret} }, want: "mechanism is required"},
		{name: "tls key pair", edit: func(c *ReaderConfig) { c.TLS = TLSConfig{Enabled: true, CertFile: "client.pem"} }, want: "configured together"},
		{name: "dialer conflict", edit: func(c *ReaderConfig) {
			c.Dialer = kafkago.DefaultDialer
			c.SASL = SASLConfig{Mechanism: SASLPlain, Username: "u", Password: secret}
		}, want: "dialer cannot be combined"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base()
			test.edit(&cfg)
			_, err := NormalizeReader(cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NormalizeReader() error = %v, want substring %q", err, test.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("NormalizeReader leaked the SASL password: %v", err)
			}
		})
	}
}

func TestWriterConfigValidation(t *testing.T) {
	const secret = "never-render-this-password"
	base := func() WriterConfig {
		return WriterConfig{Brokers: []string{"127.0.0.1:9092"}}
	}
	tests := []struct {
		name string
		edit func(*WriterConfig)
		want string
	}{
		{name: "brokers required", edit: func(c *WriterConfig) { c.Brokers = nil }, want: "at least one broker"},
		{name: "broker host port", edit: func(c *WriterConfig) { c.Brokers = []string{"localhost"} }, want: "host:port"},
		{name: "plain requires tls", edit: func(c *WriterConfig) {
			c.SASL = SASLConfig{Mechanism: SASLPlain, Username: "u", Password: secret}
		}, want: "requires tls"},
		{name: "unknown mechanism", edit: func(c *WriterConfig) {
			c.SASL = SASLConfig{Mechanism: "bad", Username: "u", Password: secret}
		}, want: "unsupported sasl"},
		{name: "required acks", edit: func(c *WriterConfig) { c.RequiredAcks = "sometimes" }, want: "required_acks"},
		{name: "compression", edit: func(c *WriterConfig) { c.Compression = "zip" }, want: "compression"},
		{name: "negative batch", edit: func(c *WriterConfig) { c.BatchSize = -1 }, want: "batch_size"},
		{name: "negative batch bytes", edit: func(c *WriterConfig) { c.BatchBytes = -1 }, want: "batch_size"},
		{name: "negative timeouts", edit: func(c *WriterConfig) { c.WriteTimeout = -1 }, want: "timeouts"},
		{name: "negative attempts", edit: func(c *WriterConfig) { c.MaxAttempts = -1 }, want: "timeouts"},
		{name: "dialer conflict", edit: func(c *WriterConfig) {
			c.Dialer = kafkago.DefaultDialer
			c.SASL = SASLConfig{Mechanism: SASLPlain, Username: "u", Password: secret}
		}, want: "dialer cannot be combined"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base()
			test.edit(&cfg)
			_, err := NormalizeWriter(cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NormalizeWriter() error = %v, want substring %q", err, test.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("NormalizeWriter leaked the SASL password: %v", err)
			}
		})
	}
}

func TestNewWriterMapsConfiguration(t *testing.T) {
	logger := &testLogger{}
	writer, err := NewWriter(WriterConfig{
		Brokers:      []string{"127.0.0.1:9092"},
		Topic:        "events",
		ClientID:     "svc",
		DialTimeout:  2 * time.Second,
		TLS:          TLSConfig{Enabled: true, ServerName: "broker.internal"},
		SASL:         SASLConfig{Mechanism: SASLPlain, Username: "user", Password: "pass"},
		Balancer:     &kafkago.Hash{},
		Async:        true,
		BatchSize:    7,
		BatchBytes:   4096,
		BatchTimeout: 250 * time.Millisecond,
		ReadTimeout:  time.Second,
		WriteTimeout: 3 * time.Second,
		RequiredAcks: AckOne,
		Compression:  CompressionGzip,
		MaxAttempts:  4,
		Logger:       logger,
		ErrorLogger:  logger,
	})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	if writer.Topic != "events" || !writer.Async || writer.MaxAttempts != 4 {
		t.Fatalf("writer = %+v", writer)
	}
	if writer.BatchSize != 7 || writer.BatchBytes != 4096 || writer.BatchTimeout != 250*time.Millisecond {
		t.Fatalf("writer batch = %+v", writer)
	}
	if writer.ReadTimeout != time.Second || writer.WriteTimeout != 3*time.Second {
		t.Fatalf("writer timeouts = %+v", writer)
	}
	if writer.RequiredAcks != kafkago.RequireOne || writer.Compression != kafkago.Gzip {
		t.Fatalf("writer delivery = %+v", writer)
	}
	if _, ok := writer.Balancer.(*kafkago.Hash); !ok {
		t.Fatalf("writer balancer = %T, want *kafkago.Hash", writer.Balancer)
	}
	if writer.Logger != logger || writer.ErrorLogger != logger {
		t.Fatalf("writer loggers were not injected")
	}

	transport, ok := writer.Transport.(*kafkago.Transport)
	if !ok {
		t.Fatalf("writer transport = %T, want *kafkago.Transport", writer.Transport)
	}
	if transport.ClientID != "svc" {
		t.Fatalf("transport client id = %q", transport.ClientID)
	}
	if transport.TLS == nil || transport.TLS.MinVersion != tls.VersionTLS12 || transport.TLS.ServerName != "broker.internal" {
		t.Fatalf("transport tls = %+v", transport.TLS)
	}
	mechanism, ok := transport.SASL.(plain.Mechanism)
	if !ok {
		t.Fatalf("transport SASL = %T, want plain.Mechanism", transport.SASL)
	}
	if mechanism.Username != "user" || mechanism.Password != "pass" {
		t.Fatalf("transport SASL credentials were not mapped")
	}
}

func TestNewReaderMapsConfiguration(t *testing.T) {
	logger := &testLogger{}
	reader, err := NewReader(ReaderConfig{
		Brokers:                []string{"127.0.0.1:9092"},
		ClientID:               "svc",
		DialTimeout:            3 * time.Second,
		TLS:                    TLSConfig{Enabled: true, ServerName: "broker.internal"},
		SASL:                   SASLConfig{Mechanism: SASLPlain, Username: "user", Password: "pass"},
		Topic:                  "orders",
		GroupID:                "workers",
		StartOffset:            OffsetLatest,
		QueueCapacity:          7,
		MinBytes:               2,
		MaxBytes:               4096,
		MaxWait:                2 * time.Second,
		HeartbeatInterval:      time.Second,
		SessionTimeout:         2 * time.Second,
		RebalanceTimeout:       3 * time.Second,
		WatchPartitionChanges:  true,
		PartitionWatchInterval: time.Second,
		BalanceStrategy:        BalanceRoundRobin,
		Logger:                 logger,
		ErrorLogger:            logger,
	})
	if err != nil {
		t.Fatalf("NewReader() error = %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	cfg := reader.Config()
	if len(cfg.Brokers) != 1 || cfg.Brokers[0] != "127.0.0.1:9092" || cfg.Topic != "orders" || cfg.GroupID != "workers" {
		t.Fatalf("reader connection = %+v", cfg)
	}
	if cfg.StartOffset != kafkago.LastOffset || cfg.CommitInterval != 0 {
		t.Fatalf("reader offsets = %+v", cfg)
	}
	if cfg.QueueCapacity != 7 || cfg.MinBytes != 2 || cfg.MaxBytes != 4096 || cfg.MaxWait != 2*time.Second {
		t.Fatalf("reader byte limits = %+v", cfg)
	}
	if cfg.HeartbeatInterval != time.Second || cfg.SessionTimeout != 2*time.Second || cfg.RebalanceTimeout != 3*time.Second {
		t.Fatalf("reader timing = %+v", cfg)
	}
	if !cfg.WatchPartitionChanges || cfg.PartitionWatchInterval != time.Second {
		t.Fatalf("reader partition watch = %+v", cfg)
	}
	if len(cfg.GroupBalancers) != 1 {
		t.Fatalf("reader group balancers = %v", cfg.GroupBalancers)
	}
	if _, ok := cfg.GroupBalancers[0].(kafkago.RoundRobinGroupBalancer); !ok {
		t.Fatalf("reader group balancer = %T, want RoundRobinGroupBalancer", cfg.GroupBalancers[0])
	}
	if cfg.Logger != logger || cfg.ErrorLogger != logger {
		t.Fatalf("reader loggers were not injected")
	}

	if cfg.Dialer == nil || cfg.Dialer.Timeout != 3*time.Second || cfg.Dialer.ClientID != "svc" {
		t.Fatalf("reader dialer = %+v", cfg.Dialer)
	}
	if cfg.Dialer.TLS == nil || cfg.Dialer.TLS.MinVersion != tls.VersionTLS12 || cfg.Dialer.TLS.ServerName != "broker.internal" {
		t.Fatalf("reader dialer tls = %+v", cfg.Dialer.TLS)
	}
	mechanism, ok := cfg.Dialer.SASLMechanism.(plain.Mechanism)
	if !ok {
		t.Fatalf("reader SASL = %T, want plain.Mechanism", cfg.Dialer.SASLMechanism)
	}
	if mechanism.Username != "user" || mechanism.Password != "pass" {
		t.Fatalf("reader SASL credentials were not mapped")
	}
}

func TestNewReaderStartOffsetAndTopicSelection(t *testing.T) {
	defaulted, err := NewReader(ReaderConfig{
		Brokers: []string{"127.0.0.1:9092"},
		Topic:   "orders",
		GroupID: "workers",
	})
	if err != nil {
		t.Fatalf("NewReader(default) error = %v", err)
	}
	t.Cleanup(func() { _ = defaulted.Close() })
	if defaulted.Config().StartOffset != kafkago.FirstOffset {
		t.Fatalf("default start offset = %d, want FirstOffset", defaulted.Config().StartOffset)
	}

	groupTopics, err := NewReader(ReaderConfig{
		Brokers:     []string{"127.0.0.1:9092"},
		GroupID:     "workers",
		GroupTopics: []string{"orders", "events"},
		StartOffset: OffsetLatest,
	})
	if err != nil {
		t.Fatalf("NewReader(group topics) error = %v", err)
	}
	t.Cleanup(func() { _ = groupTopics.Close() })
	cfg := groupTopics.Config()
	if len(cfg.GroupTopics) != 2 || cfg.GroupTopics[0] != "orders" || cfg.GroupTopics[1] != "events" {
		t.Fatalf("group topics = %q", cfg.GroupTopics)
	}
	if cfg.StartOffset != kafkago.LastOffset {
		t.Fatalf("latest start offset = %d, want LastOffset", cfg.StartOffset)
	}

	partitioned, err := NewReader(ReaderConfig{
		Brokers:   []string{"127.0.0.1:9092"},
		Topic:     "orders",
		Partition: 3,
	})
	if err != nil {
		t.Fatalf("NewReader(partition) error = %v", err)
	}
	t.Cleanup(func() { _ = partitioned.Close() })
	if partitioned.Config().Partition != 3 || partitioned.Config().GroupID != "" {
		t.Fatalf("partition reader = %+v", partitioned.Config())
	}
}

func TestDirectConstructorsAllowInsecurePlainOnlyWhenEnabled(t *testing.T) {
	// The uppercase spelling is the legacy configuration value SAS uses; the
	// constructors must canonicalize it exactly like the plugin does.
	insecure := SASLConfig{Mechanism: "PLAIN", Username: "user", Password: "pass"}

	reader, err := NewReader(ReaderConfig{
		Brokers:            []string{"127.0.0.1:9092"},
		Topic:              "orders",
		GroupID:            "workers",
		SASL:               insecure,
		AllowInsecurePlain: true,
	})
	if err != nil {
		t.Fatalf("NewReader(allow_insecure_plain) error = %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if reader.Config().Dialer.TLS != nil {
		t.Fatal("allow_insecure_plain built a TLS dialer")
	}
	if _, ok := reader.Config().Dialer.SASLMechanism.(plain.Mechanism); !ok {
		t.Fatalf("reader SASL = %T, want plain.Mechanism", reader.Config().Dialer.SASLMechanism)
	}

	writer, err := NewWriter(WriterConfig{
		Brokers:            []string{"127.0.0.1:9092"},
		Topic:              "events",
		SASL:               insecure,
		AllowInsecurePlain: true,
	})
	if err != nil {
		t.Fatalf("NewWriter(allow_insecure_plain) error = %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	transport := writer.Transport.(*kafkago.Transport)
	if transport.TLS != nil {
		t.Fatal("allow_insecure_plain built a TLS transport")
	}
	if _, ok := transport.SASL.(plain.Mechanism); !ok {
		t.Fatalf("writer SASL = %T, want plain.Mechanism", transport.SASL)
	}
}

func TestSCRAMIsAcceptedWithoutTLSByDirectConstructors(t *testing.T) {
	reader, err := NewReader(ReaderConfig{
		Brokers: []string{"127.0.0.1:9092"},
		Topic:   "orders",
		GroupID: "workers",
		SASL:    SASLConfig{Mechanism: "SCRAM-SHA-512", Username: "user", Password: "pass"},
	})
	if err != nil {
		t.Fatalf("NewReader(scram) error = %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if reader.Config().Dialer.TLS != nil {
		t.Fatal("SCRAM without TLS unexpectedly built a TLS dialer")
	}
	if reader.Config().Dialer.SASLMechanism == nil {
		t.Fatal("SCRAM mechanism was not built")
	}
}

func TestDirectConstructorsUseAnExplicitDialer(t *testing.T) {
	dialer := &kafkago.Dialer{Timeout: 42 * time.Second, ClientID: "custom"}

	reader, err := NewReader(ReaderConfig{
		Brokers: []string{"127.0.0.1:9092"},
		Topic:   "orders",
		GroupID: "workers",
		Dialer:  dialer,
	})
	if err != nil {
		t.Fatalf("NewReader(dialer) error = %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if reader.Config().Dialer != dialer {
		t.Fatal("NewReader did not use the supplied dialer")
	}

	writer, err := NewWriter(WriterConfig{
		Brokers: []string{"127.0.0.1:9092"},
		Topic:   "events",
		Dialer:  dialer,
	})
	if err != nil {
		t.Fatalf("NewWriter(dialer) error = %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	transport := writer.Transport.(*kafkago.Transport)
	if transport.ClientID != "custom" {
		t.Fatalf("writer transport client id = %q, want the dialer's", transport.ClientID)
	}
}

func TestDirectClientsCloseIsIdempotent(t *testing.T) {
	writer, err := NewWriter(WriterConfig{
		Brokers: []string{"127.0.0.1:9092"},
		Topic:   "events",
	})
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("first writer Close() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("second writer Close() error = %v", err)
	}

	reader, err := NewReader(ReaderConfig{
		Brokers: []string{"127.0.0.1:9092"},
		Topic:   "orders",
		GroupID: "workers",
	})
	if err != nil {
		t.Fatalf("NewReader() error = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("first reader Close() error = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("second reader Close() error = %v", err)
	}
}
