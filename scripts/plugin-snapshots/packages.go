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
	corehealth "github.com/xbcio/xbc/extensions/reliability/health"
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

// bundleProviders is the exhaustive, manually curated list of every reusable
// plugin implementation package's Bundle accessor. Membership mirrors
//
//	grep -rl '^func Bundle() plugin.Bundle' --include='*.go' .
//
// run from the repository root after excluding aggregate Bundles (such as
// transport/web/prelude, whose entries belong to the implementation packages
// below), scripts/plugin-migration-inventory test fixtures, and
// examples/quickstart/internal/greeter demo code. Regenerate this list by
// re-running that grep whenever a plugin package is added or removed.
var bundleProviders = []func() plugin.Bundle{
	asynq.Bundle,
	corehealth.Bundle,
	cron.Bundle,
	elasticsearch.Bundle,
	gorm.Bundle,
	kafka.Bundle,
	objectstorage.Bundle,
	outbox.Bundle,
	raft.Bundle,
	redis.Bundle,
	webhook.Bundle,

	web.Bundle,
	accesslog.Bundle,
	apikey.Bundle,
	auditlog.Bundle,
	biz.Bundle,
	cors.Bundle,
	gracefulshutdown.Bundle,
	gzip.Bundle,
	health.Bundle,
	casbin.Bundle,
	casbingorm.Bundle,
	casbinredis.Bundle,
	idempotency.Bundle,
	jwt.Bundle,
	metrics.Bundle,
	session.Bundle,
	swag.Bundle,
	tracing.Bundle,
	pprof.Bundle,
	ratelimit.Bundle,
	recovery.Bundle,
	requestid.Bundle,
	rbac.Bundle,
	securityheaders.Bundle,
	tenant.Bundle,
	timeout.Bundle,
}
