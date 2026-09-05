package kafka

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/segmentio/kafka-go/sasl/scram"
)

type messageWriter interface {
	Write(context.Context, []Message) error
	Close() error
}

type fetchedMessage struct {
	message Message
	raw     any
}

type messageReader interface {
	Fetch(context.Context) (fetchedMessage, error)
	Commit(context.Context, fetchedMessage) error
	Close() error
}

type backendFactory interface {
	NewWriter(normalizedConfig) (messageWriter, error)
	NewReader(normalizedConfig, string, ConsumerConfig) (messageReader, error)
}

type kafkaFactory struct{}

func (kafkaFactory) NewWriter(cfg normalizedConfig) (messageWriter, error) {
	dialer, err := buildDialer(cfg.Config)
	if err != nil {
		return nil, err
	}
	codec, err := compressionCodec(cfg.Producer.Compression)
	if err != nil {
		return nil, err
	}
	return &writerAdapter{writer: kafkago.NewWriter(kafkago.WriterConfig{
		Brokers:          cfg.Brokers,
		Topic:            cfg.Producer.Topic,
		Dialer:           dialer,
		BatchSize:        cfg.Producer.BatchSize,
		BatchBytes:       cfg.Producer.BatchBytes,
		BatchTimeout:     cfg.Producer.BatchTimeout,
		ReadTimeout:      cfg.Producer.ReadTimeout,
		WriteTimeout:     cfg.Producer.WriteTimeout,
		RequiredAcks:     requiredAcks(cfg.Producer.RequiredAcks),
		MaxAttempts:      cfg.Producer.MaxAttempts,
		CompressionCodec: codec,
		Async:            false,
	})}, nil
}

func (kafkaFactory) NewReader(cfg normalizedConfig, _ string, consumer ConsumerConfig) (messageReader, error) {
	dialer, err := buildDialer(cfg.Config)
	if err != nil {
		return nil, err
	}
	reader := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:       cfg.Brokers,
		GroupID:       consumer.GroupID,
		GroupTopics:   append([]string(nil), consumer.Topics...),
		Dialer:        dialer,
		QueueCapacity: consumer.QueueCapacity,
		MinBytes:      consumer.MinBytes,
		MaxBytes:      consumer.MaxBytes,
		MaxWait:       consumer.MaxWait,
		// Keep commits synchronous so Commit reports the broker result to the
		// retry and error-policy handling path.
		CommitInterval:         0,
		HeartbeatInterval:      consumer.HeartbeatInterval,
		SessionTimeout:         consumer.SessionTimeout,
		RebalanceTimeout:       consumer.RebalanceTimeout,
		WatchPartitionChanges:  consumer.WatchPartitionChanges,
		PartitionWatchInterval: consumer.PartitionWatchInterval,
		GroupBalancers:         []kafkago.GroupBalancer{groupBalancer(consumer.BalanceStrategy)},
		StartOffset:            startOffset(consumer.StartOffset),
	})
	return &readerAdapter{reader: reader}, nil
}

func buildDialer(cfg Config) (*kafkago.Dialer, error) {
	tlsConfig, err := buildTLSConfig(cfg.TLS)
	if err != nil {
		return nil, err
	}
	mechanism, err := buildSASL(cfg.SASL)
	if err != nil {
		return nil, err
	}
	return &kafkago.Dialer{Timeout: cfg.DialTimeout, ClientID: cfg.ClientID, TLS: tlsConfig, SASLMechanism: mechanism}, nil
}

func buildTLSConfig(cfg TLSConfig) (*tls.Config, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	out := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: strings.TrimSpace(cfg.ServerName)}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("kafka: read tls CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("kafka: tls ca_file contains no certificates")
		}
		out.RootCAs = pool
	}
	if cfg.CertFile != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("kafka: load tls client certificate: %w", err)
		}
		out.Certificates = []tls.Certificate{certificate}
	}
	return out, nil
}

func buildSASL(cfg SASLConfig) (sasl.Mechanism, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Mechanism)) {
	case "":
		return nil, nil
	case SASLPlain:
		return plain.Mechanism{Username: cfg.Username, Password: cfg.Password}, nil
	case SASLSCRAMSHA256:
		return scram.Mechanism(scram.SHA256, cfg.Username, cfg.Password)
	case SASLSCRAMSHA512:
		return scram.Mechanism(scram.SHA512, cfg.Username, cfg.Password)
	default:
		return nil, fmt.Errorf("kafka: unsupported sasl mechanism %q", cfg.Mechanism)
	}
}

func requiredAcks(value string) int {
	switch value {
	case AckNone:
		return 0
	case AckOne:
		return 1
	default:
		return -1
	}
}

func compressionCodec(value string) (kafkago.CompressionCodec, error) {
	switch value {
	case CompressionNone:
		return nil, nil
	case CompressionGzip:
		return kafkago.Gzip.Codec(), nil
	case CompressionSnappy:
		return kafkago.Snappy.Codec(), nil
	case CompressionLZ4:
		return kafkago.Lz4.Codec(), nil
	case CompressionZstd:
		return kafkago.Zstd.Codec(), nil
	default:
		return nil, fmt.Errorf("kafka: unsupported compression %q", value)
	}
}

func groupBalancer(value string) kafkago.GroupBalancer {
	switch value {
	case BalanceRange:
		return kafkago.RangeGroupBalancer{}
	case BalanceRoundRobin:
		return kafkago.RoundRobinGroupBalancer{}
	default:
		return kafkago.RangeGroupBalancer{}
	}
}

func startOffset(value string) int64 {
	if value == OffsetLatest {
		return kafkago.LastOffset
	}
	return kafkago.FirstOffset
}

type writerAdapter struct{ writer *kafkago.Writer }

func (w *writerAdapter) Write(ctx context.Context, messages []Message) error {
	wire := make([]kafkago.Message, len(messages))
	for i, message := range messages {
		headers := make([]kafkago.Header, len(message.Headers))
		for j, header := range message.Headers {
			headers[j] = kafkago.Header{Key: header.Key, Value: header.Value}
		}
		wire[i] = kafkago.Message{Topic: message.Topic, Key: message.Key, Value: message.Value, Headers: headers, Time: message.Time}
	}
	return w.writer.WriteMessages(ctx, wire...)
}
func (w *writerAdapter) Close() error { return w.writer.Close() }

type readerAdapter struct{ reader *kafkago.Reader }

func (r *readerAdapter) Fetch(ctx context.Context) (fetchedMessage, error) {
	message, err := r.reader.FetchMessage(ctx)
	if err != nil {
		return fetchedMessage{}, err
	}
	headers := make([]Header, len(message.Headers))
	for i, header := range message.Headers {
		headers[i] = Header{Key: header.Key, Value: append([]byte(nil), header.Value...)}
	}
	return fetchedMessage{message: Message{
		Topic: message.Topic, Key: append([]byte(nil), message.Key...), Value: append([]byte(nil), message.Value...),
		Headers: headers, Time: message.Time, Partition: message.Partition, Offset: message.Offset,
	}, raw: message}, nil
}
func (r *readerAdapter) Commit(ctx context.Context, message fetchedMessage) error {
	wire, ok := message.raw.(kafkago.Message)
	if !ok {
		return errors.New("kafka: invalid internal commit token")
	}
	return r.reader.CommitMessages(ctx, wire)
}
func (r *readerAdapter) Close() error { return r.reader.Close() }
