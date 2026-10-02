package async_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/extensions/concurrency/async"
	"github.com/xbcio/xbc/plugin"
)

// fakeDB is a plugin that only holds a connection: it needs Stop alone, per
// AGENTS.md's "Plugin Shutdown: Drain and Stop" guidance. Close reports
// whether anything observed it while still open.
type fakeDB struct {
	closed chan struct{}
}

func newFakeDB() *fakeDB { return &fakeDB{closed: make(chan struct{})} }

func (db *fakeDB) open() bool {
	select {
	case <-db.closed:
		return false
	default:
		return true
	}
}

func (db *fakeDB) stop(context.Context) error {
	close(db.closed)
	return nil
}

func fakeDBDefinition() (plugin.Definition, *fakeDB) {
	db := newFakeDB()
	definition := plugin.Define("async-e2e-db", func(plugin.BuildContext) (*fakeDB, error) {
		return db, nil
	}, plugin.Options[*fakeDB]{
		Lifecycle: plugin.Lifecycle[*fakeDB]{Stop: (*fakeDB).stop},
	})
	return definition, db
}

// fakeIngress is a minimal TrafficOpener standing in for a real listener: it
// accepts one HTTP request, which spawns a background task through the global
// async.Spawner, then stops serving in its own Stop -- exactly the ingress
// phase AGENTS.md describes as finishing before Drain begins.
type fakeIngress struct {
	server    *http.Server
	db        *fakeDB
	taskRan   chan error
	requested chan struct{}
	ready     chan string
}

func (ingress *fakeIngress) openTraffic(*plugin.Context) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) {
		close(ingress.requested)
		dbOpenAtSpawn := ingress.db.open()
		_ = async.Spawn(context.Background(), "e2e-task", func(context.Context) {
			if !dbOpenAtSpawn {
				ingress.taskRan <- errors.New("db was already closed when the task was spawned")
				return
			}
			time.Sleep(150 * time.Millisecond) // outlast the HTTP response and let drain begin
			if !ingress.db.open() {
				ingress.taskRan <- errors.New("db was closed before the spawned task finished")
				return
			}
			ingress.taskRan <- nil
		})
	})
	ingress.server = &http.Server{Handler: mux}
	go ingress.server.Serve(listener)
	ingress.ready <- listener.Addr().String()
	return nil
}

func (ingress *fakeIngress) stop(ctx context.Context) error {
	return ingress.server.Shutdown(ctx)
}

func fakeIngressDefinition(db *fakeDB, taskRan chan error, requested chan struct{}) (plugin.Definition, *fakeIngress) {
	ingress := &fakeIngress{db: db, taskRan: taskRan, requested: requested, ready: make(chan string, 1)}
	definition := plugin.Define("async-e2e-ingress", func(plugin.BuildContext) (*fakeIngress, error) {
		return ingress, nil
	}, plugin.Options[*fakeIngress]{
		Lifecycle: plugin.Lifecycle[*fakeIngress]{
			OpenTraffic: (*fakeIngress).openTraffic,
			Stop:        (*fakeIngress).stop,
		},
	})
	return definition, ingress
}

func writeAppConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\n" +
		"xbc:\n  shutdown_timeout: 5s\n  drain_timeout: 3s\n  pre_stop_timeout: 0s\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

// TestSpawnedTaskSeesDBOpenDuringDrainAndFinishesBeforeDBStop runs the real
// shutdown sequence through the public composition path: ingress stops first,
// then async's own Drain waits for the task ingress spawned, and only then
// does the db plugin's Stop run -- so the task observes the db open throughout
// and finishes strictly before it closes.
func TestSpawnedTaskSeesDBOpenDuringDrainAndFinishesBeforeDBStop(t *testing.T) {
	dbDefinition, db := fakeDBDefinition()
	taskRan := make(chan error, 1)
	requested := make(chan struct{})
	ingressDefinition, ingress := fakeIngressDefinition(db, taskRan, requested)
	_ = ingress

	bundle := plugin.BundleOf(dbDefinition, ingressDefinition)
	app, err := xbc.New(xbc.WithBundles(async.Bundle(), bundle))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, execErr := app.Execute(ctx, []string{"--config", writeAppConfig(t)})
		done <- struct {
			code int
			err  error
		}{code, execErr}
	}()
	t.Cleanup(cancel)

	// Wait for ingress to actually be serving before issuing the request.
	var addr string
	select {
	case addr = <-ingress.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("ingress did not open its listener")
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach the handler")
	}

	// Begin shutdown immediately: the spawned task should still be running
	// (it sleeps 150ms) when drain begins, proving drain waits for it rather
	// than the db being stopped first.
	cancel()

	select {
	case taskErr := <-taskRan:
		assert.NoError(t, taskErr)
	case <-time.After(5 * time.Second):
		t.Fatal("spawned task did not finish during shutdown")
	}

	select {
	case result := <-done:
		assert.NoError(t, result.err)
		assert.Equal(t, 0, result.code)
	case <-time.After(5 * time.Second):
		t.Fatal("application did not exit")
	}

	assert.False(t, db.open(), "db must be stopped once the application has exited")
}
