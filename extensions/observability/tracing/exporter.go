package tracing

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/credentials"
)

type exporterFactory func(context.Context, normalizedConfig) (sdktrace.SpanExporter, error)

func newOTLPExporter(ctx context.Context, cfg normalizedConfig) (sdktrace.SpanExporter, error) {
	tlsCfg, err := loadTLSConfig(cfg.exporter)
	if err != nil {
		return nil, err
	}
	if cfg.exporter.protocol == ProtocolHTTP {
		options := []otlptracehttp.Option{
			otlptracehttp.WithHeaders(cloneStrings(cfg.exporter.headers)),
			otlptracehttp.WithTimeout(cfg.exporter.timeout),
		}
		if cfg.exporter.endpointURL {
			options = append(options, otlptracehttp.WithEndpointURL(cfg.exporter.endpoint))
		} else {
			options = append(options, otlptracehttp.WithEndpoint(cfg.exporter.endpoint))
		}
		if cfg.exporter.insecure {
			options = append(options, otlptracehttp.WithInsecure())
		} else {
			options = append(options, otlptracehttp.WithTLSClientConfig(tlsCfg))
		}
		exporter, newErr := otlptracehttp.New(ctx, options...)
		if newErr != nil {
			return nil, fmt.Errorf("tracing: start OTLP HTTP exporter: %w", newErr)
		}
		return exporter, nil
	}

	options := []otlptracegrpc.Option{
		otlptracegrpc.WithHeaders(cloneStrings(cfg.exporter.headers)),
		otlptracegrpc.WithTimeout(cfg.exporter.timeout),
	}
	if cfg.exporter.endpointURL {
		options = append(options, otlptracegrpc.WithEndpointURL(cfg.exporter.endpoint))
	} else {
		options = append(options, otlptracegrpc.WithEndpoint(cfg.exporter.endpoint))
	}
	if cfg.exporter.insecure {
		options = append(options, otlptracegrpc.WithInsecure())
	} else {
		options = append(options, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(tlsCfg)))
	}
	exporter, newErr := otlptracegrpc.New(ctx, options...)
	if newErr != nil {
		return nil, fmt.Errorf("tracing: start OTLP gRPC exporter: %w", newErr)
	}
	return exporter, nil
}

func loadTLSConfig(cfg normalizedExporterConfig) (*tls.Config, error) {
	if cfg.insecure {
		return nil, nil
	}
	tlsCfg := &tls.Config{ // #nosec G402 -- TLS 1.2 is the explicit minimum.
		MinVersion: tls.VersionTLS12,
		ServerName: cfg.tls.ServerName,
	}
	if cfg.tls.CAFile != "" {
		pem, err := os.ReadFile(cfg.tls.CAFile)
		if err != nil {
			return nil, fmt.Errorf("tracing: read exporter CA file: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("tracing: load system CA pool: %w", err)
		}
		if roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tracing: exporter CA file contains no valid certificates")
		}
		tlsCfg.RootCAs = roots
	}
	if cfg.tls.CertFile != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.tls.CertFile, cfg.tls.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tracing: load exporter client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{certificate}
	}
	return tlsCfg, nil
}

func cloneStrings(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
