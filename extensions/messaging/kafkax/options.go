package kafkax

import (
	"time"

	kafkago "github.com/segmentio/kafka-go"
)

// ReaderOption configures NewReader. The With functions are the closed set of
// options; ReaderFunc adapts a caller-written function. A nil option is
// ignored. Every ConnectionOption is also a ReaderOption.
type ReaderOption interface {
	applyReader(*ReaderConfig)
}

// WriterOption configures NewWriter. The With functions are the closed set of
// options; WriterFunc adapts a caller-written function. A nil option is
// ignored. Every ConnectionOption is also a WriterOption.
type WriterOption interface {
	applyWriter(*WriterConfig)
}

// ConnectionOption configures settings ReaderConfig and WriterConfig share:
// brokers, client id, dial timeout, TLS, SASL, the dialer, the loggers, and the
// default topic. The same value can be passed to NewReader and NewWriter.
type ConnectionOption interface {
	ReaderOption
	WriterOption
}

// ReaderFunc adapts a function to a ReaderOption. A nil function is a no-op.
type ReaderFunc func(*ReaderConfig)

func (f ReaderFunc) applyReader(cfg *ReaderConfig) {
	if f != nil {
		f(cfg)
	}
}

// WriterFunc adapts a function to a WriterOption. A nil function is a no-op.
type WriterFunc func(*WriterConfig)

func (f WriterFunc) applyWriter(cfg *WriterConfig) {
	if f != nil {
		f(cfg)
	}
}

// connectionOption applies one setter to a reader and a writer.
type connectionOption struct {
	reader func(*ReaderConfig)
	writer func(*WriterConfig)
}

func (option connectionOption) applyReader(cfg *ReaderConfig) { option.reader(cfg) }

func (option connectionOption) applyWriter(cfg *WriterConfig) { option.writer(cfg) }

// WithBrokers sets the broker addresses. The slice is copied when the option is
// applied, and it applies to both constructors.
func WithBrokers(brokers ...string) ConnectionOption {
	return connectionOption{
		reader: func(cfg *ReaderConfig) { cfg.Brokers = append([]string(nil), brokers...) },
		writer: func(cfg *WriterConfig) { cfg.Brokers = append([]string(nil), brokers...) },
	}
}

// WithClientID sets the Kafka client id reported to the cluster.
func WithClientID(clientID string) ConnectionOption {
	return connectionOption{
		reader: func(cfg *ReaderConfig) { cfg.ClientID = clientID },
		writer: func(cfg *WriterConfig) { cfg.ClientID = clientID },
	}
}

// WithDialTimeout sets how long a connection attempt may take.
func WithDialTimeout(timeout time.Duration) ConnectionOption {
	return connectionOption{
		reader: func(cfg *ReaderConfig) { cfg.DialTimeout = timeout },
		writer: func(cfg *WriterConfig) { cfg.DialTimeout = timeout },
	}
}

// WithTLS enables TLS 1.2 or newer with the supplied CA and client certificate
// settings.
func WithTLS(config TLSConfig) ConnectionOption {
	return connectionOption{
		reader: func(cfg *ReaderConfig) { cfg.TLS = config },
		writer: func(cfg *WriterConfig) { cfg.TLS = config },
	}
}

// WithSASL sets the SASL credentials. PLAIN requires TLS unless
// WithAllowInsecurePlain(true) is also supplied.
func WithSASL(config SASLConfig) ConnectionOption {
	return connectionOption{
		reader: func(cfg *ReaderConfig) { cfg.SASL = config },
		writer: func(cfg *WriterConfig) { cfg.SASL = config },
	}
}

// WithAllowInsecurePlain permits SASL PLAIN without TLS.
func WithAllowInsecurePlain(allow bool) ConnectionOption {
	return connectionOption{
		reader: func(cfg *ReaderConfig) { cfg.AllowInsecurePlain = allow },
		writer: func(cfg *WriterConfig) { cfg.AllowInsecurePlain = allow },
	}
}

// WithDialer overrides the built dialer. A custom dialer owns its transport
// security, so it cannot be combined with TLS or SASL settings.
func WithDialer(dialer *kafkago.Dialer) ConnectionOption {
	return connectionOption{
		reader: func(cfg *ReaderConfig) { cfg.Dialer = dialer },
		writer: func(cfg *WriterConfig) { cfg.Dialer = dialer },
	}
}

// WithLogger sets kafka-go's diagnostic logger.
func WithLogger(logger kafkago.Logger) ConnectionOption {
	return connectionOption{
		reader: func(cfg *ReaderConfig) { cfg.Logger = logger },
		writer: func(cfg *WriterConfig) { cfg.Logger = logger },
	}
}

// WithErrorLogger sets kafka-go's error logger.
func WithErrorLogger(logger kafkago.Logger) ConnectionOption {
	return connectionOption{
		reader: func(cfg *ReaderConfig) { cfg.ErrorLogger = logger },
		writer: func(cfg *WriterConfig) { cfg.ErrorLogger = logger },
	}
}

// WithTopic sets the default topic. A reader may instead name group topics with
// WithGroup, and a writer may leave the topic empty when every message carries
// its own.
func WithTopic(topic string) ConnectionOption {
	return connectionOption{
		reader: func(cfg *ReaderConfig) { cfg.Topic = topic },
		writer: func(cfg *WriterConfig) { cfg.Topic = topic },
	}
}

// WithGroup sets the consumer group. Supplied topics replace GroupTopics; with
// no topics the configured Topic or GroupTopics stays in effect.
func WithGroup(groupID string, topics ...string) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) {
		cfg.GroupID = groupID
		if len(topics) > 0 {
			cfg.GroupTopics = append([]string(nil), topics...)
		}
	})
}

// WithPartition selects direct partition consumption, which cannot be combined
// with WithGroup.
func WithPartition(partition int) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) { cfg.Partition = partition })
}

// WithStartOffset sets where a group without a committed offset starts:
// OffsetEarliest or OffsetLatest.
func WithStartOffset(offset string) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) { cfg.StartOffset = offset })
}

// WithQueueCapacity sets how many messages the reader buffers.
func WithQueueCapacity(capacity int) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) { cfg.QueueCapacity = capacity })
}

// WithMinBytes sets the smallest fetch the reader accepts.
func WithMinBytes(size int) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) { cfg.MinBytes = size })
}

// WithMaxBytes sets the largest fetch the reader accepts.
func WithMaxBytes(size int) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) { cfg.MaxBytes = size })
}

// WithMaxWait sets how long a fetch waits for MinBytes.
func WithMaxWait(wait time.Duration) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) { cfg.MaxWait = wait })
}

// WithHeartbeatInterval sets the consumer group heartbeat interval.
func WithHeartbeatInterval(interval time.Duration) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) { cfg.HeartbeatInterval = interval })
}

// WithSessionTimeout sets how long a silent member stays in its group.
func WithSessionTimeout(timeout time.Duration) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) { cfg.SessionTimeout = timeout })
}

// WithRebalanceTimeout sets how long a rebalance may take.
func WithRebalanceTimeout(timeout time.Duration) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) { cfg.RebalanceTimeout = timeout })
}

// WithWatchPartitionChanges enables periodic partition discovery.
func WithWatchPartitionChanges(watch bool) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) { cfg.WatchPartitionChanges = watch })
}

// WithPartitionWatchInterval sets how often partitions are rediscovered.
func WithPartitionWatchInterval(interval time.Duration) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) { cfg.PartitionWatchInterval = interval })
}

// WithBalanceStrategy restricts the group balancing strategies to BalanceRange
// or BalanceRoundRobin.
func WithBalanceStrategy(strategy string) ReaderOption {
	return ReaderFunc(func(cfg *ReaderConfig) { cfg.BalanceStrategy = strategy })
}

// WithBalancer sets the writer's partition balancer.
func WithBalancer(balancer kafkago.Balancer) WriterOption {
	return WriterFunc(func(cfg *WriterConfig) { cfg.Balancer = balancer })
}

// WithAsync makes WriteMessages return without waiting for delivery.
func WithAsync(async bool) WriterOption {
	return WriterFunc(func(cfg *WriterConfig) { cfg.Async = async })
}

// WithBatchSize sets the maximum number of messages per batch.
func WithBatchSize(size int) WriterOption {
	return WriterFunc(func(cfg *WriterConfig) { cfg.BatchSize = size })
}

// WithBatchBytes sets the maximum batch size in bytes.
func WithBatchBytes(size int) WriterOption {
	return WriterFunc(func(cfg *WriterConfig) { cfg.BatchBytes = size })
}

// WithBatchTimeout sets how often an incomplete batch is flushed.
func WithBatchTimeout(timeout time.Duration) WriterOption {
	return WriterFunc(func(cfg *WriterConfig) { cfg.BatchTimeout = timeout })
}

// WithReadTimeout sets the writer's read timeout.
func WithReadTimeout(timeout time.Duration) WriterOption {
	return WriterFunc(func(cfg *WriterConfig) { cfg.ReadTimeout = timeout })
}

// WithWriteTimeout sets the writer's write timeout.
func WithWriteTimeout(timeout time.Duration) WriterOption {
	return WriterFunc(func(cfg *WriterConfig) { cfg.WriteTimeout = timeout })
}

// WithRequiredAcks sets the delivery guarantee: AckNone, AckOne, or AckAll.
func WithRequiredAcks(acks string) WriterOption {
	return WriterFunc(func(cfg *WriterConfig) { cfg.RequiredAcks = acks })
}

// WithCompression sets the batch compression codec.
func WithCompression(compression string) WriterOption {
	return WriterFunc(func(cfg *WriterConfig) { cfg.Compression = compression })
}

// WithMaxAttempts sets how many times a message delivery is attempted.
func WithMaxAttempts(attempts int) WriterOption {
	return WriterFunc(func(cfg *WriterConfig) { cfg.MaxAttempts = attempts })
}
