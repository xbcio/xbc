package metrics

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

// Order implements grpctransport.UnaryInterceptor and
// grpctransport.StreamInterceptor. Metrics observe the call and constrain
// nothing; within the phase the identity tie-break places them, and the
// tracing interceptor owns its own edge ahead of metrics.
func (*Plugin) Order() grpctransport.Order {
	return grpctransport.Order{Phase: grpctransport.PhaseObserve}
}

// UnaryInterceptor implements grpctransport.UnaryInterceptor.
func (p *Plugin) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		if p == nil || p.core == nil {
			return handler(ctx, req)
		}
		service, method := splitFullMethod(info.FullMethod)
		inflight := p.inflight.WithLabelValues(service, method)
		inflight.Inc()
		started := time.Now()
		panicked := true
		defer func() {
			inflight.Dec()
			p.record(service, method, codeLabel(err, panicked), started)
		}()
		resp, err = handler(ctx, req)
		panicked = false
		return resp, err
	}
}

// StreamInterceptor implements grpctransport.StreamInterceptor. A stream is
// one call: it is counted once, from the moment the stream is accepted until
// its handler returns.
func (p *Plugin) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		if p == nil || p.core == nil {
			return handler(srv, ss)
		}
		service, method := splitFullMethod(info.FullMethod)
		inflight := p.inflight.WithLabelValues(service, method)
		inflight.Inc()
		started := time.Now()
		panicked := true
		defer func() {
			inflight.Dec()
			p.record(service, method, codeLabel(err, panicked), started)
		}()
		err = handler(srv, ss)
		panicked = false
		return err
	}
}

// record counts one finished call. duration is computed from the caller's
// start time, after the in-flight decrement, so a scrape never sees a call
// that is both in flight and finished.
func (p *Plugin) record(service, method, code string, started time.Time) {
	p.requests.WithLabelValues(service, method, code).Inc()
	p.duration.WithLabelValues(service, method, code).Observe(time.Since(started).Seconds())
}

// codeLabel names the status an unwinding panic will end as. The Server's
// panic boundary answers a re-panicked call with codes.Internal "internal
// error", so recording Internal here keeps the metric and the wire status in
// agreement for the same call.
func codeLabel(err error, panicked bool) string {
	code := status.Code(err)
	if panicked {
		code = codes.Internal
	}
	return code.String()
}

// splitFullMethod splits a full gRPC method "/service/method" into the two
// label values. Interceptors only run for registered methods, so both halves
// are bounded; an unparsable form degrades to bounded placeholders rather than
// creating a series per input.
func splitFullMethod(fullMethod string) (service, method string) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(fullMethod), "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		service, method = trimmed[:index], trimmed[index+1:]
	} else if trimmed != "" {
		service, method = trimmed, "unknown"
	}
	if service == "" || method == "" {
		return "unknown", "unknown"
	}
	return service, method
}
