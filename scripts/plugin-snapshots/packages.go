package main

import (
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/security/rbac"

	"github.com/xbcio/xbc/integrations/asynq"
	"github.com/xbcio/xbc/integrations/cron"
	"github.com/xbcio/xbc/integrations/elasticsearch"
	"github.com/xbcio/xbc/integrations/gorm"
	"github.com/xbcio/xbc/integrations/kafka"
	"github.com/xbcio/xbc/integrations/objectstorage"
	"github.com/xbcio/xbc/integrations/outbox"
	"github.com/xbcio/xbc/integrations/raft"
	"github.com/xbcio/xbc/integrations/redis"
	"github.com/xbcio/xbc/integrations/webhook"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/accesslog"
	"github.com/xbcio/xbc/transport/web/apikey"
	"github.com/xbcio/xbc/transport/web/auditlog"
	"github.com/xbcio/xbc/transport/web/biz"
	"github.com/xbcio/xbc/transport/web/cors"
	"github.com/xbcio/xbc/transport/web/gracefulshutdown"
	"github.com/xbcio/xbc/transport/web/gzip"
	"github.com/xbcio/xbc/transport/web/health"
	"github.com/xbcio/xbc/transport/web/integrations/casbin"
	casbingorm "github.com/xbcio/xbc/transport/web/integrations/casbin-gorm"
	casbinredis "github.com/xbcio/xbc/transport/web/integrations/casbin-redis"
	"github.com/xbcio/xbc/transport/web/integrations/idempotency"
	"github.com/xbcio/xbc/transport/web/integrations/jwt"
	"github.com/xbcio/xbc/transport/web/integrations/metrics"
	"github.com/xbcio/xbc/transport/web/integrations/session"
	"github.com/xbcio/xbc/transport/web/integrations/swagger"
	"github.com/xbcio/xbc/transport/web/integrations/tracing"
	"github.com/xbcio/xbc/transport/web/pprof"
	"github.com/xbcio/xbc/transport/web/ratelimit"
	"github.com/xbcio/xbc/transport/web/recovery"
	"github.com/xbcio/xbc/transport/web/requestid"
	"github.com/xbcio/xbc/transport/web/securityheaders"
	"github.com/xbcio/xbc/transport/web/tenant"
	"github.com/xbcio/xbc/transport/web/timeout"
)

// definitionProviders is the exhaustive, manually curated list of every
// canonical plugin package's Definition accessor. Membership mirrors
//
//	grep -rl '^func Definition() plugin.Definition' --include='*.go' .
//
// run from the repository root (excluding scripts/plugin-migration-inventory
// test fixtures and examples/quickstart/internal/greeter, which is demo code
// rather than a real integration). Regenerate this list by re-running that
// grep whenever a plugin package is added or removed.
var definitionProviders = []func() plugin.Definition{
	asynq.Definition,
	cron.Definition,
	elasticsearch.Definition,
	gorm.Definition,
	kafka.Definition,
	objectstorage.Definition,
	outbox.Definition,
	raft.Definition,
	redis.Definition,
	webhook.Definition,

	web.Definition,
	accesslog.Definition,
	apikey.Definition,
	auditlog.Definition,
	biz.Definition,
	cors.Definition,
	gracefulshutdown.Definition,
	gzip.Definition,
	health.Definition,
	casbin.Definition,
	casbingorm.Definition,
	casbinredis.Definition,
	idempotency.Definition,
	jwt.Definition,
	metrics.Definition,
	session.Definition,
	swagger.Definition,
	tracing.Definition,
	pprof.Definition,
	ratelimit.Definition,
	recovery.Definition,
	requestid.Definition,
	rbac.Definition,
	securityheaders.Definition,
	tenant.Definition,
	timeout.Definition,
}
