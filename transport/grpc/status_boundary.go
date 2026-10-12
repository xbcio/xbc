package grpc

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/ordering"
)

// StatusBoundaryKey is the canonical producer identity of the outermost
// PhaseError interceptor, which the Server assembles on both chains from the
// collected StatusMapper plugins.
const StatusBoundaryKey plugin.Key = "grpc-status-boundary"

// StatusMapper is the consumer-owned contract for translating an application
// error into the status a client receives. The first mapper that recognizes an
// error wins; StatusOrder declares precedence among mappers on a graph of its
// own, independent of interceptor order.
//
// A mapper decides what a caller may learn about a failure, so it must not copy
// an error's text into the status unless that text is known to be safe.
type StatusMapper interface {
	MapStatus(context.Context, error) (*status.Status, bool)
	StatusOrder() StatusOrder
}

// StatusOrder declares precedence among StatusMapper plugins. References use
// interceptor-order strictness semantics but form an independent graph.
type StatusOrder struct {
	After  []OrderRef
	Before []OrderRef
}

var statusBoundaryIdentity = plugin.Identity{Plugin: StatusBoundaryKey}

// statusBoundary makes every error that leaves a handler a deliberate status.
//
// grpc-go itself turns a non-status error into codes.Unknown carrying the
// error's text, which hands a client whatever the error happened to say --
// SQL, file paths, upstream addresses. The boundary passes an existing status
// through untouched (a handler that chose one meant it), maps context
// cancellation and expiry to their own codes, offers everything else to the
// mappers, and answers what none of them recognize with a fixed
// codes.Internal, logging the cause server-side.
type statusBoundary struct {
	logger  log.Logger
	mappers []plugin.Entry[StatusMapper]
}

func (*statusBoundary) Order() Order { return Order{Phase: PhaseError} }

func (b *statusBoundary) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			return nil, b.toStatus(ctx, info.FullMethod, err)
		}
		return resp, nil
	}
}

func (b *statusBoundary) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := handler(srv, ss); err != nil {
			return b.toStatus(ss.Context(), info.FullMethod, err)
		}
		return nil
	}
}

func (b *statusBoundary) toStatus(ctx context.Context, fullMethod string, err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	}
	for _, entry := range b.mappers {
		mapped, ok := entry.Value.MapStatus(ctx, err)
		if !ok {
			continue
		}
		if mapped == nil || mapped.Code() == codes.OK {
			// A mapper claiming an error as success would turn a failure into
			// an empty reply; that is a mapper defect and fails closed.
			b.logger.Error("grpc status mapper returned no failure status", "method", fullMethod, "mapper", entry.Identity.String(), "error", err.Error())
			return status.Error(codes.Internal, "internal error")
		}
		return mapped.Err()
	}
	b.logger.Error("grpc request failed", "method", fullMethod, "error", err.Error())
	return status.Error(codes.Internal, "internal error")
}

// orderStatusMappers sorts mappers by their independent StatusOrder graph,
// with canonical Identity as the tie-break.
func orderStatusMappers(entries []plugin.Entry[StatusMapper]) ([]plugin.Entry[StatusMapper], error) {
	entries = slices.Clone(entries)
	slices.SortFunc(entries, func(a, b plugin.Entry[StatusMapper]) int {
		return plugin.CompareIdentity(a.Identity, b.Identity)
	})
	graph := ordering.New()
	byName := make(map[string]plugin.Entry[StatusMapper], len(entries))
	for i, entry := range entries {
		entry.Identity = entry.Identity.Normalized()
		entries[i] = entry
		name := entry.Identity.String()
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("xbc: status mapper identity %s is present more than once", entry.Identity)
		}
		graph.AddNode(name)
		byName[name] = entry
	}
	for _, entry := range entries {
		declaration := entry.Value.StatusOrder()
		for _, relation := range []struct {
			refs      []OrderRef
			direction ordering.Direction
		}{
			{declaration.After, ordering.After},
			{declaration.Before, ordering.Before},
		} {
			for _, ref := range relation.refs {
				matched := false
				for _, target := range entries {
					if target.Identity == entry.Identity || !ref.matches(target.Identity) {
						continue
					}
					matched = true
					if relation.direction == ordering.After {
						graph.AddHardEdge(target.Identity.String(), entry.Identity.String())
					} else {
						graph.AddHardEdge(entry.Identity.String(), target.Identity.String())
					}
				}
				if !matched && ref.Required() {
					return nil, fmt.Errorf("xbc: status mapper %s requires %s=%q, but no matching mapper is present",
						entry.Identity, relation.direction, ref)
				}
			}
		}
	}
	names, _, err := graph.Sort()
	if err != nil {
		return nil, fmt.Errorf("xbc: status mapper ordering failed: %w", err)
	}
	ordered := make([]plugin.Entry[StatusMapper], len(names))
	for i, name := range names {
		ordered[i] = byName[name]
	}
	return ordered, nil
}
