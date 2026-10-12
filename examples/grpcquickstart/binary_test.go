//go:build unix && !aix && !solaris

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	greeterv1 "github.com/xbcio/xbc/examples/grpcquickstart/internal/gen/greeter/v1"
)

// startWait bounds every wait in this file. It is generous on purpose: the
// first assertion a failure should produce is the one naming what did not
// happen, not a timeout on a shared machine.
const startWait = 15 * time.Second

// TestGRPCQuickstartBinaryServesFromTheEnvironmentAlone is the example's
// end-to-end coverage as it ships. The package directory holds no
// application.yml, so the process reads no file at all: the address comes
// from the environment, access logging from the starter's defaults layer, and
// everything else from the plugins' own declared defaults. The standard health
// probe, the unary method, and the server stream are each called over the
// wire, and an interrupt must end the process cleanly.
//
// Signals are not portable to every platform this module builds on, so the
// file carries a unix build constraint rather than skipping at run time.
func TestGRPCQuickstartBinaryServesFromTheEnvironmentAlone(t *testing.T) {
	for _, candidate := range []string{"application.yml", filepath.Join("configs", "application.yml")} {
		if _, err := os.Stat(candidate); err == nil {
			t.Fatalf("%s exists; the example promises to start from the environment alone", candidate)
		}
	}
	binary := buildExample(t)
	address := freeAddress(t)
	process := startExample(t, binary, "XBC_GRPC_ADDR="+address)

	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	waitForServing(t, healthpb.NewHealthClient(conn), process)

	ctx, cancel := context.WithTimeout(context.Background(), startWait)
	defer cancel()
	client := greeterv1.NewGreeterServiceClient(conn)

	hello, err := client.SayHello(ctx, &greeterv1.SayHelloRequest{Name: "XBC"})
	if err != nil {
		t.Fatalf("SayHello: %v\n%s", err, process.output.String())
	}
	if hello.GetMessage() != "hello, XBC" {
		t.Errorf("SayHello = %q, want the greeting the example documents", hello.GetMessage())
	}
	if _, err := client.SayHello(ctx, &greeterv1.SayHelloRequest{Name: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("SayHello with a one-character name = %v, want InvalidArgument", err)
	}

	stream, err := client.StreamGreetings(ctx, &greeterv1.StreamGreetingsRequest{Name: "XBC", Count: 3})
	if err != nil {
		t.Fatalf("StreamGreetings: %v", err)
	}
	var sequences []int32
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("StreamGreetings Recv: %v\n%s", err, process.output.String())
		}
		sequences = append(sequences, message.GetSequence())
	}
	if len(sequences) != 3 || sequences[0] != 1 || sequences[2] != 3 {
		t.Errorf("StreamGreetings sequences = %v, want [1 2 3]", sequences)
	}

	// The starter is the only thing that turns access logging on, so a record
	// for the call above is what proves its defaults layer reached the plan.
	waitForOutput(t, process, "grpc call completed", "method=StreamGreetings", "kind=server_stream")

	if err := process.command.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal the example binary: %v", err)
	}
	select {
	case <-process.exited:
		if err := process.exitErr(); err != nil {
			t.Fatalf("the process exited with %v, want a clean exit\n%s", err, process.output.String())
		}
	case <-time.After(startWait):
		t.Fatalf("timed out waiting for the process to stop\n%s", process.output.String())
	}
}

// TestGRPCQuickstartDoctorNeedsNoFile runs the doctor subcommand with no file
// and no environment: it must validate the plan and name the starter as the
// origin of the access log's activation.
func TestGRPCQuickstartDoctorNeedsNoFile(t *testing.T) {
	binary := buildExample(t)
	output, err := exec.Command(binary, "doctor").CombinedOutput()
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, output)
	}
	for _, want := range []string{"grpc-accesslog", "grpc starter", "greeter", "health-grpc"} {
		if !bytes.Contains(output, []byte(want)) {
			t.Errorf("doctor output does not mention %q:\n%s", want, output)
		}
	}
}

func buildExample(t *testing.T) string {
	t.Helper()

	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go command is unavailable, skip the binary smoke test")
	}
	binary := filepath.Join(t.TempDir(), "grpcquickstart")
	build := exec.Command(goTool, "build", "-o", binary, ".")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build: %v\n%s", buildErr, output)
	}
	return binary
}

type exampleProcess struct {
	command *exec.Cmd
	output  *syncBuffer
	exited  <-chan struct{}
	exitErr func() error
}

func startExample(t *testing.T, binary string, env ...string) *exampleProcess {
	t.Helper()

	command := exec.Command(binary)
	command.Env = append(os.Environ(), env...)
	output := new(syncBuffer)
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatalf("start the example binary: %v", err)
	}
	var exitErr error
	exited := make(chan struct{})
	go func() {
		exitErr = command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
			return
		default:
		}
		_ = command.Process.Kill()
		<-exited
	})
	return &exampleProcess{command: command, output: output, exited: exited, exitErr: func() error { return exitErr }}
}

// waitForServing polls the standard health service until readiness reports
// SERVING, which happens only after the runtime opens the traffic gate.
func waitForServing(t *testing.T, client healthpb.HealthClient, process *exampleProcess) {
	t.Helper()

	deadline := time.Now().Add(startWait)
	for time.Now().Before(deadline) {
		select {
		case <-process.exited:
			t.Fatalf("the process exited before becoming ready: %v\n%s", process.exitErr(), process.output.String())
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		response, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
		cancel()
		if err == nil && response.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for SERVING\n%s", process.output.String())
}

// waitForOutput waits until one logged line contains every fragment.
func waitForOutput(t *testing.T, process *exampleProcess, fragments ...string) {
	t.Helper()

	deadline := time.Now().Add(startWait)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(process.output.String(), "\n") {
			matched := true
			for _, fragment := range fragments {
				if !strings.Contains(line, fragment) {
					matched = false
					break
				}
			}
			if matched {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no logged line contains %q\n%s", fragments, process.output.String())
}

func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return address
}

type syncBuffer struct {
	mutex sync.Mutex
	text  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.text.Write(p)
}

func (b *syncBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.text.String()
}
