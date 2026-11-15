package mail

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	gomail "github.com/wneessen/go-mail"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

func TestBuildClientVerifiesConnectivityAndClosesTheProbe(t *testing.T) {
	server := newFakeSMTPServer(t)
	values := serverValues(t, server)
	values["verify"] = true
	config := bindConfig(t, values)

	client, err := buildClient(config, "default")
	if err != nil {
		t.Fatalf("buildClient() error = %v", err)
	}
	server.waitForAccepted(t, 1)
	if messages := server.received(); len(messages) != 0 {
		t.Fatalf("the verification probe sent %d messages, want none", len(messages))
	}

	if err := stopClient(client, context.Background()); err != nil {
		t.Fatalf("stopClient() error = %v", err)
	}
	// The library close is a no-op once the probe connection is gone, so a
	// repeated stop must stay successful.
	if err := stopClient(client, context.Background()); err != nil {
		t.Fatalf("second stopClient() error = %v", err)
	}
	if err := client.DialAndSend(gomail.NewMsg()); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after stop error = %v, want ErrClosed", err)
	}
}

func TestBuildClientWithVerificationDisabledDoesNotDial(t *testing.T) {
	server := newFakeSMTPServer(t)
	config := bindConfig(t, serverValues(t, server))

	client, err := buildClient(config, "default")
	if err != nil {
		t.Fatalf("buildClient() error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(client, context.Background()) })

	time.Sleep(100 * time.Millisecond)
	if accepted := server.acceptedCount(); accepted != 0 {
		t.Fatalf("verify=false accepted %d connections, want none", accepted)
	}
}

// TestBuildClientFailureNamesOnlyInstanceAndAddress pins the error contract:
// a refused connection names the instance and the address, never credentials.
func TestBuildClientFailureNamesOnlyInstanceAndAddress(t *testing.T) {
	const secret = "another-secret-password-value"
	host, port := closedAddress(t)
	config := bindConfig(t, map[string]any{
		"host":      host,
		"port":      port,
		"username":  "service",
		"password":  secret,
		"auth_type": "none",
		"tls":       map[string]any{"policy": "none"},
	})

	_, err := buildClient(config, "alerts")
	if err == nil {
		t.Fatal("buildClient() error = nil, want a refused connection")
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))
	for _, want := range []string{"mail: instance alerts", address} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error rendered the password: %q", err)
	}
}

func TestDialAndSendDeliversAMessage(t *testing.T) {
	server := newFakeSMTPServer(t)
	config := bindConfig(t, serverValues(t, server))
	client, err := buildClient(config, "default")
	if err != nil {
		t.Fatalf("buildClient() error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(client, context.Background()) })

	message := gomail.NewMsg()
	if err := message.From("sender@example.com"); err != nil {
		t.Fatalf("From: %v", err)
	}
	if err := message.To("recipient@example.com"); err != nil {
		t.Fatalf("To: %v", err)
	}
	message.Subject("xbc mail plugin")
	message.SetBodyString(gomail.TypeTextPlain, "hello from xbc")

	if err := client.DialAndSend(message); err != nil {
		t.Fatalf("DialAndSend() error = %v", err)
	}

	received := server.received()
	if len(received) != 1 {
		t.Fatalf("server received %d messages, want 1", len(received))
	}
	got := received[0]
	if !strings.Contains(got.mailFrom, "sender@example.com") {
		t.Fatalf("MAIL FROM = %q, want the sender", got.mailFrom)
	}
	if len(got.recipients) != 1 || !strings.Contains(got.recipients[0], "recipient@example.com") {
		t.Fatalf("RCPT TO = %v, want the recipient", got.recipients)
	}
	for _, want := range []string{"Subject: xbc mail plugin", "hello from xbc"} {
		if !strings.Contains(got.data, want) {
			t.Fatalf("message data does not contain %q:\n%s", want, got.data)
		}
	}
}

// TestAssembledInstancesAreIndependent composes two configured instances and
// proves that each sends through its own server.
func TestAssembledInstancesAreIndependent(t *testing.T) {
	firstServer := newFakeSMTPServer(t)
	secondServer := newFakeSMTPServer(t)
	environment := newEnvironment(t, map[string]any{
		"first":  serverValues(t, firstServer),
		"second": serverValues(t, secondServer),
	})

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("assembly.BuildPlan: %v", err)
	}
	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("assembly.Construct: %v", err)
	}
	first := clientFor(t, constructed, "first")
	second := clientFor(t, constructed, "second")
	t.Cleanup(func() {
		for _, identity := range []plugin.Identity{
			{Plugin: Key, Instance: "first"},
			{Plugin: Key, Instance: "second"},
		} {
			if instance, ok := constructed.Instance(identity); ok {
				_ = instance.StopBounded(context.Background(), time.Second)
			}
		}
	})

	if first == second {
		t.Fatal("both instances resolved to the same client")
	}
	sendMessage(t, first, "first@example.com", "first instance")
	sendMessage(t, second, "second@example.com", "second instance")

	assertSingleMessage(t, firstServer, "first instance")
	assertSingleMessage(t, secondServer, "second instance")
}

func TestAssembledPluginExportsTheSenderContract(t *testing.T) {
	server := newFakeSMTPServer(t)
	environment := newEnvironment(t, map[string]any{"default": serverValues(t, server)})

	senderInput := plugin.RefToInstance[Sender](Key, plugin.DefaultInstance)
	consumer := plugin.Define(
		"sender-consumer",
		func(ctx plugin.BuildContext) (*senderConsumer, error) {
			return &senderConsumer{sender: senderInput.Get(ctx).Value}, nil
		},
		plugin.Options[*senderConsumer]{Inputs: plugin.Inputs(senderInput)},
	)
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(consumer), Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("assembly.BuildPlan: %v", err)
	}
	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("assembly.Construct: %v", err)
	}
	t.Cleanup(func() {
		if instance, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}); ok {
			_ = instance.StopBounded(context.Background(), time.Second)
		}
	})

	primary := clientFor(t, constructed, plugin.DefaultInstance)
	consumerInstance, ok := constructed.Instance(plugin.Identity{Plugin: "sender-consumer", Instance: plugin.DefaultInstance})
	if !ok {
		t.Fatal("the consumer must be constructed")
	}
	probe, ok := consumerInstance.Primary().(*senderConsumer)
	if !ok {
		t.Fatalf("consumer primary = %T, want *senderConsumer", consumerInstance.Primary())
	}
	if probe.sender != Sender(primary) {
		t.Fatal("the consumer resolved a different sender than the instance's primary")
	}
	sendMessage(t, probe.sender, "contract@example.com", "through the contract")
	assertSingleMessage(t, server, "through the contract")
}

func TestClientAndConfigFormattingNeverRenderThePassword(t *testing.T) {
	const secret = "format-secret-password-value"
	values := serverValues(t, newFakeSMTPServer(t))
	values["username"] = "service"
	values["password"] = secret
	config := bindConfig(t, values)
	client, err := buildClient(config, "alerts")
	if err != nil {
		t.Fatalf("buildClient() error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(client, context.Background()) })

	for _, format := range []string{"%v", "%+v", "%#v"} {
		if rendered := fmt.Sprintf(format, client); strings.Contains(rendered, secret) {
			t.Fatalf("%s rendered the password: %q", format, rendered)
		}
	}
	if rendered := client.String(); strings.Contains(rendered, secret) {
		t.Fatalf("String() rendered the password: %q", rendered)
	}
	if rendered := client.GoString(); strings.Contains(rendered, secret) {
		t.Fatalf("GoString() rendered the password: %q", rendered)
	}
}

type senderConsumer struct {
	sender Sender
}

// TestPrepareFailureNamesTheConfiguredInstance proves that an invalid value
// rejected during preparation still identifies the configured section: XBC's
// planning error carries the plugin identity, which spells the instance name,
// alongside the field-level message this package produces.
func TestPrepareFailureNamesTheConfiguredInstance(t *testing.T) {
	const secret = "prepare-secret-password-value"
	environment := newEnvironment(t, map[string]any{
		"alerts": map[string]any{
			"host":      "smtp.example.com",
			"password":  secret,
			"auth_type": "carrier-pigeon",
		},
	})
	_, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	if err == nil {
		t.Fatal("assembly.BuildPlan() error = nil, want the invalid auth type rejected")
	}
	for _, want := range []string{"mail[alerts]", "auth_type"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error rendered the password: %q", err)
	}
}

func clientFor(t *testing.T, constructed *assembly.Constructed, instance string) *Client {
	t.Helper()
	value, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: instance})
	if !ok {
		t.Fatalf("instance %q was not constructed", instance)
	}
	client, ok := value.Primary().(*Client)
	if !ok {
		t.Fatalf("primary = %T, want *Client", value.Primary())
	}
	return client
}

func sendMessage(t *testing.T, sender Sender, recipient, body string) {
	t.Helper()
	message := gomail.NewMsg()
	if err := message.From("noreply@example.com"); err != nil {
		t.Fatalf("From: %v", err)
	}
	if err := message.To(recipient); err != nil {
		t.Fatalf("To: %v", err)
	}
	message.Subject("xbc mail plugin")
	message.SetBodyString(gomail.TypeTextPlain, body)
	if err := sender.DialAndSend(message); err != nil {
		t.Fatalf("DialAndSend(%q) error = %v", body, err)
	}
}

func assertSingleMessage(t *testing.T, server *fakeSMTPServer, body string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		received := server.received()
		if len(received) == 1 {
			if !strings.Contains(received[0].data, body) {
				t.Fatalf("received data does not contain %q:\n%s", body, received[0].data)
			}
			return
		}
		if len(received) > 1 {
			t.Fatalf("server received %d messages, want 1", len(received))
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("server received %d messages, want 1", len(server.received()))
}
