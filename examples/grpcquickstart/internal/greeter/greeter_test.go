package greeter

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	greeterv1 "github.com/xbcio/xbc/examples/grpcquickstart/internal/gen/greeter/v1"
)

func TestSayHelloValidatesWithoutEchoingTheInput(t *testing.T) {
	for _, name := range []string{"", " x ", strings.Repeat("n", 81)} {
		_, err := (&Plugin{}).SayHello(context.Background(), &greeterv1.SayHelloRequest{Name: name})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("SayHello(%q) = %v, want InvalidArgument", name, err)
		}
		if name != "" && strings.Contains(status.Convert(err).Message(), strings.TrimSpace(name)) {
			t.Fatalf("rejection %q echoes the input", status.Convert(err).Message())
		}
	}
	response, err := (&Plugin{}).SayHello(context.Background(), &greeterv1.SayHelloRequest{Name: "  XBC  "})
	if err != nil || response.GetMessage() != "hello, XBC" {
		t.Fatalf("SayHello = %v, %v", response, err)
	}
}

// recordingStream is the smallest server stream StreamGreetings needs.
type recordingStream struct {
	grpc.ServerStreamingServer[greeterv1.StreamGreetingsResponse]
	ctx  context.Context
	sent []*greeterv1.StreamGreetingsResponse
}

func (s *recordingStream) Context() context.Context { return s.ctx }

func (s *recordingStream) Send(message *greeterv1.StreamGreetingsResponse) error {
	s.sent = append(s.sent, message)
	return nil
}

func TestStreamGreetingsBoundsTheCountAndStopsWithTheStream(t *testing.T) {
	for _, count := range []int32{0, -1, maxGreetings + 1} {
		stream := &recordingStream{ctx: context.Background()}
		err := (&Plugin{}).StreamGreetings(&greeterv1.StreamGreetingsRequest{Name: "XBC", Count: count}, stream)
		if status.Code(err) != codes.InvalidArgument || len(stream.sent) != 0 {
			t.Fatalf("count %d: err=%v sent=%d, want InvalidArgument and nothing sent", count, err, len(stream.sent))
		}
	}

	stream := &recordingStream{ctx: context.Background()}
	if err := (&Plugin{}).StreamGreetings(&greeterv1.StreamGreetingsRequest{Name: "XBC", Count: maxGreetings}, stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.sent) != maxGreetings || stream.sent[maxGreetings-1].GetSequence() != maxGreetings {
		t.Fatalf("sent %d messages, want %d in sequence", len(stream.sent), maxGreetings)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	stream = &recordingStream{ctx: cancelled}
	if err := (&Plugin{}).StreamGreetings(&greeterv1.StreamGreetingsRequest{Name: "XBC", Count: 3}, stream); err == nil || len(stream.sent) != 0 {
		t.Fatalf("cancelled stream: err=%v sent=%d, want the context error and nothing sent", err, len(stream.sent))
	}
}
