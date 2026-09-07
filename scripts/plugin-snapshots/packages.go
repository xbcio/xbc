package main

import (
	"github.com/xbcio/xbc/extensions/authorization/rbac"
	"github.com/xbcio/xbc/plugin"

	"github.com/xbcio/xbc/extensions/coordination/raft"
	"github.com/xbcio/xbc/extensions/jobs/asynq"
	"github.com/xbcio/xbc/extensions/jobs/cron"
	"github.com/xbcio/xbc/extensions/messaging/kafka"
	"github.com/xbcio/xbc/extensions/messaging/outbox"
	"github.com/xbcio/xbc/extensions/messaging/webhook"
	"github.com/xbcio/xbc/extensions/storage/elasticsearch"
	"github.com/xbcio/xbc/extensions/storage/gorm"
	"github.com/xbcio/xbc/extensions/storage/objectstorage"
	"github.com/xbcio/xbc/extensions/storage/redis"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/authentication/apikey"
	"github.com/xbcio/xbc/transport/web/extensions/authentication/jwt"
	"github.com/xbcio/xbc/transport/web/extensions/authentication/session"
	"github.com/xbcio/xbc/transport/web/extensions/authorization/casbin"
	casbingorm "github.com/xbcio/xbc/transport/web/extensions/authorization/casbin-gorm"
	casbinredis "github.com/xbcio/xbc/transport/web/extensions/authorization/casbin-redis"
	"github.com/xbcio/xbc/transport/web/extensions/authorization/tenant"
	"github.com/xbcio/xbc/transport/web/extensions/observability/accesslog"
	"github.com/xbcio/xbc/transport/web/extensions/observability/auditlog"
	"github.com/xbcio/xbc/transport/web/extensions/observability/metrics"
	"github.com/xbcio/xbc/transport/web/extensions/observability/pprof"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
	"github.com/xbcio/xbc/transport/web/extensions/observability/tracing"
	"github.com/xbcio/xbc/transport/web/extensions/openapi/swag"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/gracefulshutdown"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/health"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/idempotency"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/ratelimit"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/recovery"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/timeout"
	"github.com/xbcio/xbc/transport/web/extensions/response/biz"
	"github.com/xbcio/xbc/transport/web/extensions/response/gzip"
	"github.com/xbcio/xbc/transport/web/extensions/security/cors"
	"github.com/xbcio/xbc/transport/web/extensions/security/securityheaders"
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
	swag.Definition,
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
