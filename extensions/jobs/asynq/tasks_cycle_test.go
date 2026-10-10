package asynq_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/extensions/jobs/asynq"
	"github.com/xbcio/xbc/extensions/tasks"
	"github.com/xbcio/xbc/plugin"
)

// cyclicProvider both depends on the asynq Enqueuer explicitly and exports
// tasks.Provider. asynq collects every Provider, so the plugin is upstream of
// asynq through the collection and downstream of it through its own
// dependency: a cycle the composition must reject by name.
type cyclicProvider struct{}

func (*cyclicProvider) Tasks() []tasks.Binding { return nil }

// TestProviderThatAlsoDependsOnAsynqIsACycle pins the rule the tasks package
// documents: a plugin provides tasks or depends on the executor explicitly,
// not both. The cycle is reported while the plan is built, before Redis is
// ever dialled, so the configured address is never reached.
func TestProviderThatAlsoDependsOnAsynqIsACycle(t *testing.T) {
	enqueuer := plugin.RefTo[asynq.Enqueuer](asynq.Key)
	cyclic := plugin.Define("tasks-cyclic-asynq-provider", func(ctx plugin.BuildContext) (*cyclicProvider, error) {
		_ = enqueuer.Get(ctx)
		return &cyclicProvider{}, nil
	}, plugin.Options[*cyclicProvider]{
		Inputs: plugin.Inputs(enqueuer),
		Exports: plugin.Contracts(
			plugin.ExportAs[tasks.Provider](func(value *cyclicProvider) tasks.Provider { return value }),
		),
	})

	path := filepath.Join(t.TempDir(), "application.yml")
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\n" +
		"plugins:\n  asynq:\n    redis:\n      addr: 127.0.0.1:1\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	app, err := xbc.New(xbc.WithBundles(asynq.Bundle(), plugin.BundleOf(cyclic)))
	if err == nil {
		var code int
		code, err = app.Execute(context.Background(), []string{"--config", path})
		if code == 0 {
			t.Fatalf("Execute() exit code = 0, want a composition failure")
		}
	}
	if err == nil {
		t.Fatal("composition error = nil, want a dependency cycle")
	}
	for _, want := range []string{"plugin dependency cycle", "tasks-cyclic-asynq-provider"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("composition error = %q, want it to mention %q", err, want)
		}
	}
}
