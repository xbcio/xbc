package asynq

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"
)

func TestClientDefaultRetentionAppliesAndExplicitOptionWins(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := validTestConfig(server.Addr())
	cfg.DefaultRetention = 5 * time.Minute
	redisClient := goredis.NewClient(cfg.Redis.options(nil))
	t.Cleanup(func() { _ = redisClient.Close() })
	client := newClient(hibiken.NewClientFromRedisClient(redisClient), cfg)
	inspector := hibiken.NewInspectorFromRedisClient(redisClient)
	ctx := context.Background()

	if client.defaults.retention != 5*time.Minute {
		t.Fatalf("client default retention = %s, want 5m", client.defaults.retention)
	}
	retentionOf := func(id string, options ...TaskOption) time.Duration {
		t.Helper()
		info, err := client.Enqueue(ctx, Task{Type: "kept"}, append([]TaskOption{TaskID(id)}, options...)...)
		if err != nil {
			t.Fatalf("Enqueue(%q) error = %v", id, err)
		}
		stored, err := inspector.GetTaskInfo(cfg.DefaultQueue, info.ID)
		if err != nil {
			t.Fatalf("GetTaskInfo(%q) error = %v", id, err)
		}
		return stored.Retention
	}

	if got := retentionOf("defaulted"); got != 5*time.Minute {
		t.Fatalf("retention without an option = %s, want the configured 5m", got)
	}
	if got := retentionOf("overridden", Retention(time.Minute)); got != time.Minute {
		t.Fatalf("retention with an option = %s, want the explicit 1m", got)
	}
	if got := retentionOf("cleared", Retention(0)); got != 0 {
		t.Fatalf("retention with an explicit zero = %s, want none", got)
	}
}

func TestConfigDefaultRetentionDefaultsBindingAndValidation(t *testing.T) {
	cfg := bindAsynqConfig(t, map[string]any{})
	if cfg.DefaultRetention != 0 {
		t.Fatalf("default_retention default = %s, want zero", cfg.DefaultRetention)
	}

	bound := bindAsynqConfig(t, map[string]any{"default_retention": "5m"})
	if bound.DefaultRetention != 5*time.Minute {
		t.Fatalf("bound default_retention = %s, want 5m", bound.DefaultRetention)
	}

	invalid := defaultConfig()
	invalid.DefaultRetention = -time.Second
	if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "default_retention") {
		t.Fatalf("Validate() error = %v, want default_retention named", err)
	}
}
