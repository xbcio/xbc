package kafkax

import (
	"crypto/tls"
	"strings"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl/plain"
)

func mustReader(t *testing.T, cfg ReaderConfig, opts ...ReaderOption) *kafkago.Reader {
	t.Helper()
	reader, err := NewReader(cfg, opts...)
	if err != nil {
		t.Fatalf("NewReader() error = %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return reader
}

func mustWriter(t *testing.T, cfg WriterConfig, opts ...WriterOption) *kafkago.Writer {
	t.Helper()
	writer, err := NewWriter(cfg, opts...)
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	return writer
}

func asReaderOptions(options []ConnectionOption) []ReaderOption {
	out := make([]ReaderOption, len(options))
	for i, option := range options {
		out[i] = option
	}
	return out
}

func asWriterOptions(options []ConnectionOption) []WriterOption {
	out := make([]WriterOption, len(options))
	for i, option := range options {
		out[i] = option
	}
	return out
}

func TestReaderOptionsApply(t *testing.T) {
	t.Run("group", func(t *testing.T) {
		logger := &testLogger{}
		reader := mustReader(t, ReaderConfig{},
			WithBrokers("127.0.0.1:9092", "127.0.0.1:9093"),
			WithClientID("svc"),
			WithDialTimeout(2*time.Second),
			WithTLS(TLSConfig{Enabled: true, ServerName: "broker.internal"}),
			WithSASL(SASLConfig{Mechanism: SASLPlain, Username: "user", Password: "pass"}),
			WithTopic("orders"),
			WithGroup("workers", "events", "audits"),
			WithStartOffset(OffsetLatest),
			WithQueueCapacity(7),
			WithMinBytes(2),
			WithMaxBytes(4096),
			WithMaxWait(2*time.Second),
			WithHeartbeatInterval(time.Second),
			WithSessionTimeout(2*time.Second),
			WithRebalanceTimeout(3*time.Second),
			WithWatchPartitionChanges(true),
			WithPartitionWatchInterval(4*time.Second),
			WithBalanceStrategy(BalanceRoundRobin),
			WithLogger(logger),
			WithErrorLogger(logger),
		)

		cfg := reader.Config()
		if len(cfg.Brokers) != 2 || cfg.Brokers[0] != "127.0.0.1:9092" || cfg.Brokers[1] != "127.0.0.1:9093" {
			t.Fatalf("brokers = %q", cfg.Brokers)
		}
		if cfg.Topic != "orders" || cfg.GroupID != "workers" {
			t.Fatalf("topics = %q, group = %q", cfg.Topic, cfg.GroupID)
		}
		if len(cfg.GroupTopics) != 2 || cfg.GroupTopics[0] != "events" || cfg.GroupTopics[1] != "audits" {
			t.Fatalf("group topics = %q", cfg.GroupTopics)
		}
		if cfg.StartOffset != kafkago.LastOffset {
			t.Fatalf("start offset = %d, want LastOffset", cfg.StartOffset)
		}
		if cfg.QueueCapacity != 7 || cfg.MinBytes != 2 || cfg.MaxBytes != 4096 || cfg.MaxWait != 2*time.Second {
			t.Fatalf("byte limits = %+v", cfg)
		}
		if cfg.HeartbeatInterval != time.Second || cfg.SessionTimeout != 2*time.Second || cfg.RebalanceTimeout != 3*time.Second {
			t.Fatalf("group timing = %+v", cfg)
		}
		if !cfg.WatchPartitionChanges || cfg.PartitionWatchInterval != 4*time.Second {
			t.Fatalf("partition watch = %+v", cfg)
		}
		if len(cfg.GroupBalancers) != 1 {
			t.Fatalf("group balancers = %v", cfg.GroupBalancers)
		}
		if _, ok := cfg.GroupBalancers[0].(kafkago.RoundRobinGroupBalancer); !ok {
			t.Fatalf("group balancer = %T, want RoundRobinGroupBalancer", cfg.GroupBalancers[0])
		}
		if cfg.Logger != logger || cfg.ErrorLogger != logger {
			t.Fatal("loggers were not injected")
		}
		if cfg.Dialer == nil || cfg.Dialer.Timeout != 2*time.Second || cfg.Dialer.ClientID != "svc" {
			t.Fatalf("dialer = %+v", cfg.Dialer)
		}
		if cfg.Dialer.TLS == nil || cfg.Dialer.TLS.MinVersion != tls.VersionTLS12 || cfg.Dialer.TLS.ServerName != "broker.internal" {
			t.Fatalf("dialer TLS = %+v", cfg.Dialer.TLS)
		}
		mechanism, ok := cfg.Dialer.SASLMechanism.(plain.Mechanism)
		if !ok || mechanism.Username != "user" || mechanism.Password != "pass" {
			t.Fatalf("dialer SASL = %T", cfg.Dialer.SASLMechanism)
		}
	})

	t.Run("partition", func(t *testing.T) {
		reader := mustReader(t, ReaderConfig{},
			WithBrokers("127.0.0.1:9092"),
			WithTopic("orders"),
			WithPartition(3),
		)
		if cfg := reader.Config(); cfg.Partition != 3 || cfg.GroupID != "" {
			t.Fatalf("partition reader = %+v", cfg)
		}
	})
}

func TestWriterOptionsApply(t *testing.T) {
	logger := &testLogger{}
	writer := mustWriter(t, WriterConfig{},
		WithBrokers("127.0.0.1:9092"),
		WithClientID("svc"),
		WithDialTimeout(2*time.Second),
		WithTLS(TLSConfig{Enabled: true, ServerName: "broker.internal"}),
		WithSASL(SASLConfig{Mechanism: SASLPlain, Username: "user", Password: "pass"}),
		WithTopic("events"),
		WithBalancer(&kafkago.Hash{}),
		WithAsync(true),
		WithBatchSize(7),
		WithBatchBytes(4096),
		WithBatchTimeout(250*time.Millisecond),
		WithReadTimeout(time.Second),
		WithWriteTimeout(3*time.Second),
		WithRequiredAcks(AckOne),
		WithCompression(CompressionGzip),
		WithMaxAttempts(4),
		WithLogger(logger),
		WithErrorLogger(logger),
	)

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
		t.Fatal("writer loggers were not injected")
	}
	transport, ok := writer.Transport.(*kafkago.Transport)
	if !ok {
		t.Fatalf("writer transport = %T, want *kafkago.Transport", writer.Transport)
	}
	if transport.ClientID != "svc" {
		t.Fatalf("transport client id = %q", transport.ClientID)
	}
	if transport.TLS == nil || transport.TLS.MinVersion != tls.VersionTLS12 || transport.TLS.ServerName != "broker.internal" {
		t.Fatalf("transport TLS = %+v", transport.TLS)
	}
	if _, ok := transport.SASL.(plain.Mechanism); !ok {
		t.Fatalf("transport SASL = %T, want plain.Mechanism", transport.SASL)
	}
}

func TestConnectionOptionsApplyToBothConstructors(t *testing.T) {
	logger := &testLogger{}
	common := []ConnectionOption{
		WithBrokers("127.0.0.1:9092", "127.0.0.1:9093"),
		WithClientID("shared"),
		WithDialTimeout(2 * time.Second),
		WithTopic("shared-topic"),
		WithLogger(logger),
		WithErrorLogger(logger),
	}

	t.Run("tls and sasl", func(t *testing.T) {
		options := append(append([]ConnectionOption(nil), common...),
			WithTLS(TLSConfig{Enabled: true, ServerName: "broker.internal"}),
			WithSASL(SASLConfig{Mechanism: SASLPlain, Username: "user", Password: "pass"}),
			WithAllowInsecurePlain(true),
		)
		reader := mustReader(t, ReaderConfig{}, asReaderOptions(options)...)
		writer := mustWriter(t, WriterConfig{}, asWriterOptions(options)...)

		readerCfg := reader.Config()
		if len(readerCfg.Brokers) != 2 || readerCfg.Topic != "shared-topic" ||
			readerCfg.Logger != logger || readerCfg.ErrorLogger != logger {
			t.Fatalf("reader shared settings = %+v", readerCfg)
		}
		if readerCfg.Dialer.ClientID != "shared" || readerCfg.Dialer.Timeout != 2*time.Second {
			t.Fatalf("reader dialer = %+v", readerCfg.Dialer)
		}
		if readerCfg.Dialer.TLS == nil || readerCfg.Dialer.SASLMechanism == nil {
			t.Fatalf("reader TLS/SASL were not mapped: %+v", readerCfg.Dialer)
		}

		if writer.Addr.String() != "127.0.0.1:9092,127.0.0.1:9093" || writer.Topic != "shared-topic" ||
			writer.Logger != logger || writer.ErrorLogger != logger {
			t.Fatalf("writer shared settings = %+v", writer)
		}
		transport, ok := writer.Transport.(*kafkago.Transport)
		if !ok {
			t.Fatalf("writer transport = %T, want *kafkago.Transport", writer.Transport)
		}
		if transport.ClientID != "shared" || transport.TLS == nil || transport.SASL == nil {
			t.Fatalf("writer transport = %+v", transport)
		}
	})

	t.Run("insecure plain", func(t *testing.T) {
		options := append(append([]ConnectionOption(nil), common...),
			WithSASL(SASLConfig{Mechanism: SASLPlain, Username: "user", Password: "pass"}),
			WithAllowInsecurePlain(true),
		)
		reader := mustReader(t, ReaderConfig{}, asReaderOptions(options)...)
		writer := mustWriter(t, WriterConfig{}, asWriterOptions(options)...)

		if reader.Config().Dialer.TLS != nil {
			t.Fatal("reader built a TLS dialer")
		}
		if _, ok := reader.Config().Dialer.SASLMechanism.(plain.Mechanism); !ok {
			t.Fatalf("reader SASL = %T, want plain.Mechanism", reader.Config().Dialer.SASLMechanism)
		}
		transport := writer.Transport.(*kafkago.Transport)
		if transport.TLS != nil {
			t.Fatal("writer built a TLS transport")
		}
		if _, ok := transport.SASL.(plain.Mechanism); !ok {
			t.Fatalf("writer SASL = %T, want plain.Mechanism", transport.SASL)
		}
	})

	t.Run("custom dialer", func(t *testing.T) {
		dialer := &kafkago.Dialer{Timeout: 3 * time.Second, ClientID: "dialer"}
		options := append(append([]ConnectionOption(nil), common...), WithDialer(dialer))
		reader := mustReader(t, ReaderConfig{}, asReaderOptions(options)...)
		writer := mustWriter(t, WriterConfig{}, asWriterOptions(options)...)

		if reader.Config().Dialer != dialer {
			t.Fatal("reader did not use the supplied dialer")
		}
		transport := writer.Transport.(*kafkago.Transport)
		if transport.ClientID != "dialer" || transport.TLS != nil || transport.SASL != nil {
			t.Fatalf("writer transport = %+v", transport)
		}
	})
}

func TestConstructorOptionsIgnoreNil(t *testing.T) {
	reader := mustReader(t, ReaderConfig{Brokers: []string{"127.0.0.1:9092"}, Topic: "orders"}, nil)
	if reader.Config().Topic != "orders" {
		t.Fatalf("reader topic = %q", reader.Config().Topic)
	}
	writer := mustWriter(t, WriterConfig{Brokers: []string{"127.0.0.1:9092"}}, nil)
	if writer.Topic != "" {
		t.Fatalf("writer topic = %q", writer.Topic)
	}

	reader = mustReader(t, ReaderConfig{Brokers: []string{"127.0.0.1:9092"}, Topic: "orders"}, ReaderFunc(nil))
	if reader.Config().Topic != "orders" {
		t.Fatalf("reader topic with nil ReaderFunc = %q", reader.Config().Topic)
	}
	writer = mustWriter(t, WriterConfig{Brokers: []string{"127.0.0.1:9092"}}, WriterFunc(nil))
	if writer.Topic != "" {
		t.Fatalf("writer topic with nil WriterFunc = %q", writer.Topic)
	}
}

func TestConstructorOptionsOverrideConfig(t *testing.T) {
	readerCfg := ReaderConfig{
		Brokers:       []string{"127.0.0.1:9092"},
		Topic:         "original",
		ClientID:      "original",
		QueueCapacity: 5,
	}
	reader := mustReader(t, readerCfg,
		WithBrokers("127.0.0.1:9093"),
		WithTopic("overridden"),
		WithClientID("first"),
		WithClientID("second"),
		WithQueueCapacity(9),
	)
	cfg := reader.Config()
	if len(cfg.Brokers) != 1 || cfg.Brokers[0] != "127.0.0.1:9093" {
		t.Fatalf("brokers = %q", cfg.Brokers)
	}
	if cfg.Topic != "overridden" || cfg.QueueCapacity != 9 {
		t.Fatalf("overridden reader = %+v", cfg)
	}
	if cfg.Dialer == nil || cfg.Dialer.ClientID != "second" {
		t.Fatalf("reader client id = %+v", cfg.Dialer)
	}
	if readerCfg.Topic != "original" || readerCfg.ClientID != "original" ||
		readerCfg.Brokers[0] != "127.0.0.1:9092" || readerCfg.QueueCapacity != 5 {
		t.Fatalf("NewReader mutated its input: %+v", readerCfg)
	}

	writerCfg := WriterConfig{
		Brokers:      []string{"127.0.0.1:9092"},
		Topic:        "original",
		RequiredAcks: AckNone,
		BatchSize:    5,
	}
	writer := mustWriter(t, writerCfg,
		WithBrokers("127.0.0.1:9093"),
		WithTopic("overridden"),
		WithRequiredAcks(AckAll),
		WithBatchSize(9),
	)
	if writer.Addr.String() != "127.0.0.1:9093" {
		t.Fatalf("writer brokers = %q", writer.Addr.String())
	}
	if writer.Topic != "overridden" || writer.RequiredAcks != kafkago.RequireAll || writer.BatchSize != 9 {
		t.Fatalf("overridden writer = %+v", writer)
	}
	if writerCfg.Topic != "original" || writerCfg.RequiredAcks != AckNone ||
		writerCfg.Brokers[0] != "127.0.0.1:9092" || writerCfg.BatchSize != 5 {
		t.Fatalf("NewWriter mutated its input: %+v", writerCfg)
	}
}

func TestConstructorOptionsCopySlices(t *testing.T) {
	brokers := []string{"127.0.0.1:9092"}
	topics := []string{"orders", "events"}
	reader := mustReader(t, ReaderConfig{}, WithBrokers(brokers...), WithGroup("workers", topics...))
	brokers[0] = "changed:9092"
	topics[0] = "changed"

	cfg := reader.Config()
	if cfg.Brokers[0] != "127.0.0.1:9092" {
		t.Fatalf("reader brokers aliased the option input: %q", cfg.Brokers)
	}
	if cfg.GroupTopics[0] != "orders" {
		t.Fatalf("reader group topics aliased the option input: %q", cfg.GroupTopics)
	}

	writerBrokers := []string{"127.0.0.1:9092"}
	writer := mustWriter(t, WriterConfig{}, WithBrokers(writerBrokers...))
	writerBrokers[0] = "changed:9092"
	if writer.Addr.String() != "127.0.0.1:9092" {
		t.Fatalf("writer brokers aliased the option input: %q", writer.Addr.String())
	}
}

func TestConstructorOptionsRejectSASLPlainWithoutTLS(t *testing.T) {
	const secret = "never-render-this-password"
	insecure := SASLConfig{Mechanism: SASLPlain, Username: "user", Password: secret}

	if _, err := NewReader(ReaderConfig{},
		WithBrokers("127.0.0.1:9092"),
		WithTopic("orders"),
		WithSASL(insecure),
	); err == nil || !strings.Contains(err.Error(), "requires tls") {
		t.Fatalf("NewReader() error = %v, want TLS requirement", err)
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("NewReader leaked the SASL password: %v", err)
	}

	if _, err := NewWriter(WriterConfig{},
		WithBrokers("127.0.0.1:9092"),
		WithSASL(insecure),
	); err == nil || !strings.Contains(err.Error(), "requires tls") {
		t.Fatalf("NewWriter() error = %v, want TLS requirement", err)
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("NewWriter leaked the SASL password: %v", err)
	}
}

func TestConstructorOptionsRejectDialerWithTLSOrSASL(t *testing.T) {
	dialer := &kafkago.Dialer{Timeout: time.Second}
	scram := SASLConfig{Mechanism: SASLSCRAMSHA512, Username: "user", Password: "pass"}

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "reader tls",
			call: func() error {
				_, err := NewReader(ReaderConfig{}, WithBrokers("127.0.0.1:9092"), WithTopic("orders"),
					WithDialer(dialer), WithTLS(TLSConfig{Enabled: true}))
				return err
			},
		},
		{
			name: "reader sasl",
			call: func() error {
				_, err := NewReader(ReaderConfig{}, WithBrokers("127.0.0.1:9092"), WithTopic("orders"),
					WithDialer(dialer), WithSASL(scram))
				return err
			},
		},
		{
			name: "writer tls",
			call: func() error {
				_, err := NewWriter(WriterConfig{}, WithBrokers("127.0.0.1:9092"),
					WithDialer(dialer), WithTLS(TLSConfig{Enabled: true}))
				return err
			},
		},
		{
			name: "writer sasl",
			call: func() error {
				_, err := NewWriter(WriterConfig{}, WithBrokers("127.0.0.1:9092"),
					WithDialer(dialer), WithSASL(scram))
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil || !strings.Contains(err.Error(), "dialer cannot be combined") {
				t.Fatalf("error = %v, want dialer conflict", err)
			}
		})
	}
}
