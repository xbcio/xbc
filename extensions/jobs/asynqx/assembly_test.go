package asynqx_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/extensions/jobs/asynqx"
	"github.com/xbcio/xbc/plugin"
)

// capturedContracts is what the consumer Definition receives from the
// assembled application: the three contracts this plugin exports.
type capturedContracts struct {
	enqueuer   asynqx.Enqueuer
	inspector  asynqx.Inspector
	controller asynqx.Controller
}

func contractCaptureDefinition(captured chan<- capturedContracts) plugin.Definition {
	enqueuer := plugin.RefTo[asynqx.Enqueuer](asynqx.Key)
	inspector := plugin.RefTo[asynqx.Inspector](asynqx.Key)
	controller := plugin.RefTo[asynqx.Controller](asynqx.Key)
	return plugin.Define("asynq-contract-consumer", func(ctx plugin.BuildContext) (*struct{}, error) {
		captured <- capturedContracts{
			enqueuer:   enqueuer.Get(ctx).Value,
			inspector:  inspector.Get(ctx).Value,
			controller: controller.Get(ctx).Value,
		}
		return &struct{}{}, nil
	}, plugin.Options[*struct{}]{
		Inputs: plugin.Inputs(enqueuer, inspector, controller),
	})
}

func writeContractConfig(t *testing.T, addr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\n" +
		"plugins:\n  asynq:\n    redis:\n      addr: " + addr + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write application config: %v", err)
	}
	return path
}

// TestContractsAreReachableThroughXBCAssembly pins that an application which
// composes asynq.Bundle can collect Inspector and Controller separately from
// Enqueuer, and that they operate on the plugin's own Redis connection.
func TestContractsAreReachableThroughXBCAssembly(t *testing.T) {
	redisServer := miniredis.RunT(t)
	captured := make(chan capturedContracts, 1)
	app, err := xbc.New(xbc.WithBundles(
		asynqx.Bundle(),
		plugin.BundleOf(contractCaptureDefinition(captured)),
	))
	if err != nil {
		t.Fatalf("xbc.New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	configPath := writeContractConfig(t, redisServer.Addr())
	executed := make(chan error, 1)
	go func() {
		code, execErr := app.Execute(ctx, []string{"--config", configPath})
		if execErr != nil {
			executed <- execErr
			return
		}
		if code != 0 {
			executed <- fmt.Errorf("Execute() exit code = %d", code)
			return
		}
		executed <- nil
	}()

	var contracts capturedContracts
	select {
	case contracts = <-captured:
	case execErr := <-executed:
		t.Fatalf("application exited before exporting its contracts: %v", execErr)
	case <-time.After(5 * time.Second):
		t.Fatal("the consumer plugin did not receive the asynq contracts")
	}
	if contracts.enqueuer == nil || contracts.inspector == nil || contracts.controller == nil {
		t.Fatalf("captured contracts = %+v", contracts)
	}

	if _, err := contracts.enqueuer.Enqueue(context.Background(), asynqx.Task{
		Type:    "assembled",
		Payload: []byte("payload"),
	}, asynqx.Queue("default"), asynqx.TaskID("assembled-1")); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	tasks, err := contracts.inspector.ListTasks("default", asynqx.StatePending, asynqx.Page{})
	if err != nil {
		t.Fatalf("ListTasks() error = %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "assembled-1" || tasks[0].State != asynqx.StatePending {
		t.Fatalf("ListTasks() = %+v", tasks)
	}
	if err := contracts.controller.PauseQueue("default"); err != nil {
		t.Fatalf("PauseQueue() error = %v", err)
	}
	info, err := contracts.inspector.QueueInfo("default")
	if err != nil {
		t.Fatalf("QueueInfo() error = %v", err)
	}
	if !info.Paused || info.Pending != 1 {
		t.Fatalf("QueueInfo() = %+v", info)
	}
	if err := contracts.controller.UnpauseQueue("default"); err != nil {
		t.Fatalf("UnpauseQueue() error = %v", err)
	}
	if err := contracts.controller.DeleteTask("default", "assembled-1"); err != nil {
		t.Fatalf("DeleteTask() error = %v", err)
	}

	cancel()
	select {
	case execErr := <-executed:
		if execErr != nil {
			t.Fatalf("Execute() error = %v", execErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("application did not exit after cancellation")
	}
}
