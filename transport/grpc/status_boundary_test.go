package grpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

var errNotFound = errors.New("row not found")

// codeMapper claims errors matching target and answers with code.
type codeMapper struct {
	target error
	code   codes.Code
	order  StatusOrder
	nilOut bool
}

func (m codeMapper) MapStatus(_ context.Context, err error) (*status.Status, bool) {
	if !errors.Is(err, m.target) {
		return nil, false
	}
	if m.nilOut {
		return nil, true
	}
	return status.New(m.code, m.code.String()), true
}

func (m codeMapper) StatusOrder() StatusOrder { return m.order }

func mapperEntry(key plugin.Key, m codeMapper) plugin.Entry[StatusMapper] {
	return plugin.Entry[StatusMapper]{Identity: plugin.Identity{Plugin: key}, Value: m}
}

func boundaryFor(t *testing.T, entries ...plugin.Entry[StatusMapper]) *statusBoundary {
	t.Helper()
	ordered, err := orderStatusMappers(entries)
	if err != nil {
		t.Fatal(err)
	}
	return &statusBoundary{logger: log.Nop(), mappers: ordered}
}

func TestStatusBoundaryMapsEveryErrorToADeliberateStatus(t *testing.T) {
	boundary := boundaryFor(t,
		mapperEntry("not-found", codeMapper{target: errNotFound, code: codes.NotFound}),
		mapperEntry("broken", codeMapper{target: errBroken, nilOut: true}),
	)
	cases := map[string]struct {
		err  error
		code codes.Code
		msg  string
	}{
		"existing status passes through": {status.Error(codes.InvalidArgument, "bad name"), codes.InvalidArgument, "bad name"},
		"canceled":                       {fmt.Errorf("query: %w", context.Canceled), codes.Canceled, "canceled"},
		"deadline":                       {fmt.Errorf("query: %w", context.DeadlineExceeded), codes.DeadlineExceeded, "deadline exceeded"},
		"mapped":                         {fmt.Errorf("lookup: %w", errNotFound), codes.NotFound, "NotFound"},
		"mapper returns nil":             {errBroken, codes.Internal, "internal error"},
		"unclaimed": {
			errors.New("pq: relation users at /var/lib/db 10.0.0.7"), codes.Internal, "internal error",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := status.Convert(boundary.toStatus(context.Background(), "/x.Y/Z", tc.err))
			if got.Code() != tc.code || got.Message() != tc.msg {
				t.Fatalf("toStatus = %v %q, want %v %q", got.Code(), got.Message(), tc.code, tc.msg)
			}
		})
	}
}

var errBroken = errors.New("broken mapper target")

type okMapper struct{}

func (okMapper) MapStatus(context.Context, error) (*status.Status, bool) {
	return status.New(codes.OK, ""), true
}
func (okMapper) StatusOrder() StatusOrder { return StatusOrder{} }

func TestStatusMapperClaimingSuccessFailsClosed(t *testing.T) {
	boundary := boundaryFor(t, plugin.Entry[StatusMapper]{Identity: plugin.Identity{Plugin: "ok"}, Value: okMapper{}})
	if got := status.Code(boundary.toStatus(context.Background(), "/x.Y/Z", errors.New("x"))); got != codes.Internal {
		t.Fatalf("code = %v, want Internal", got)
	}
}

func TestStatusMapperOrderIsDeclaredNotCollected(t *testing.T) {
	// Both claim errNotFound. Identity order alone would pick "a".
	a := mapperEntry("a", codeMapper{target: errNotFound, code: codes.NotFound})
	b := mapperEntry("b", codeMapper{target: errNotFound, code: codes.FailedPrecondition, order: StatusOrder{Before: []OrderRef{Require("a")}}})
	for _, entries := range [][]plugin.Entry[StatusMapper]{{a, b}, {b, a}} {
		boundary := boundaryFor(t, entries...)
		if got := status.Code(boundary.toStatus(context.Background(), "/x.Y/Z", errNotFound)); got != codes.FailedPrecondition {
			t.Fatalf("code = %v, want the mapper declared first", got)
		}
	}
	plain := boundaryFor(t, mapperEntry("b", codeMapper{target: errNotFound, code: codes.FailedPrecondition}), a)
	if got := status.Code(plain.toStatus(context.Background(), "/x.Y/Z", errNotFound)); got != codes.NotFound {
		t.Fatalf("code = %v, want identity order as the tie-break", got)
	}
}

func TestStatusMapperOrderingErrors(t *testing.T) {
	cases := map[string][]plugin.Entry[StatusMapper]{
		"missing required": {mapperEntry("a", codeMapper{order: StatusOrder{After: []OrderRef{Require("absent")}}})},
		"cycle": {
			mapperEntry("a", codeMapper{order: StatusOrder{Before: []OrderRef{Require("b")}}}),
			mapperEntry("b", codeMapper{order: StatusOrder{Before: []OrderRef{Require("a")}}}),
		},
		"duplicate identity": {mapperEntry("a", codeMapper{}), mapperEntry("a", codeMapper{})},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := orderStatusMappers(entries); err == nil {
				t.Fatal("orderStatusMappers accepted an invalid set")
			}
		})
	}
	// A Prefer reference to an absent mapper is a soft miss.
	if _, err := orderStatusMappers([]plugin.Entry[StatusMapper]{
		mapperEntry("a", codeMapper{order: StatusOrder{After: []OrderRef{Prefer("absent")}}}),
	}); err != nil {
		t.Fatalf("Prefer of an absent mapper = %v, want nil", err)
	}
}

// failingEcho returns an internal error from both methods.
type failingEcho struct{ err error }

func (f failingEcho) Echo(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	return nil, f.err
}

func (f failingEcho) Repeat(*wrapperspb.StringValue, grpc.ServerStream) error { return f.err }

func TestHandlerErrorsNeverReachTheClientVerbatim(t *testing.T) {
	leaky := errors.New("dial tcp 10.0.0.7:5432: password authentication failed for user app")
	c := contributions{
		registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(failingEcho{err: leaky})},
		mappers:    []plugin.Entry[StatusMapper]{mapperEntry("not-found", codeMapper{target: errNotFound, code: codes.NotFound})},
	}
	_, conn := startServing(t, newServer(DefaultConfig(), c))

	_, err := callEcho(t, conn, "x")
	if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "10.0.0.7") {
		t.Fatalf("unary error = %v, want a generic Internal", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Repeat", ServerStreams: true}, "/"+echoServiceName+"/Repeat")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(wrapperspb.String("x")); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()
	err = stream.RecvMsg(new(wrapperspb.StringValue))
	if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "10.0.0.7") {
		t.Fatalf("stream error = %v, want a generic Internal", err)
	}

	// A mapped error keeps its mapper's status end to end.
	c.registrars = []plugin.Entry[ServiceRegistrar]{echoContributor(failingEcho{err: fmt.Errorf("get: %w", errNotFound)})}
	_, conn = startServing(t, newServer(DefaultConfig(), c))
	if _, err := callEcho(t, conn, "x"); status.Code(err) != codes.NotFound {
		t.Fatalf("mapped error = %v, want NotFound", err)
	}
}
