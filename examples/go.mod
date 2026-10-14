module github.com/xbcio/xbc/examples

go 1.25.0

// XBC modules imported below resolve through their published release tags;
// repository development resolves the same source through the root go.work.
require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/casbin/casbin/v2 v2.135.0
	github.com/casbin/gorm-adapter/v3 v3.39.0
	github.com/swaggo/swag v1.16.6
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
	gorm.io/gorm v1.31.1
)

require (
	github.com/stretchr/testify v1.12.1
	github.com/xbcio/xbc v0.1.0
	github.com/xbcio/xbc/extensions/authentication/apikey v0.1.0
	github.com/xbcio/xbc/extensions/authentication/jwt v0.1.0
	github.com/xbcio/xbc/extensions/authorization/casbin v0.1.0
	github.com/xbcio/xbc/extensions/authorization/casbin-gorm v0.1.0
	github.com/xbcio/xbc/extensions/authorization/casbin-redis v0.1.0
	github.com/xbcio/xbc/extensions/authorization/rbac v0.1.0
	github.com/xbcio/xbc/extensions/concurrency/async v0.1.0
	github.com/xbcio/xbc/extensions/jobs/asynq v0.1.0
	github.com/xbcio/xbc/extensions/jobs/cron v0.1.0
	github.com/xbcio/xbc/extensions/messaging/outbox v0.1.0
	github.com/xbcio/xbc/extensions/reliability/health v0.1.0
	github.com/xbcio/xbc/extensions/storage/gorm v0.1.0
	github.com/xbcio/xbc/extensions/tasks v0.1.0
	github.com/xbcio/xbc/transport/grpc v0.1.0
	github.com/xbcio/xbc/transport/grpc/extensions/observability/metrics v0.1.0
	github.com/xbcio/xbc/transport/web v0.1.0
	github.com/xbcio/xbc/transport/web/engines/gin v0.1.0
	github.com/xbcio/xbc/transport/web/extensions/authentication/jwt v0.1.0
	github.com/xbcio/xbc/transport/web/extensions/authorization/casbin v0.1.0
	github.com/xbcio/xbc/transport/web/extensions/observability/metrics v0.1.0
	github.com/xbcio/xbc/transport/web/extensions/observability/tracing v0.1.0
	github.com/xbcio/xbc/transport/web/extensions/openapi/swag v0.1.0
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/KyleBanks/depth v1.2.1 // indirect
	github.com/PuerkitoBio/purell v1.1.1 // indirect
	github.com/PuerkitoBio/urlesc v0.0.0-20170810143723-de5bf2ad4578 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bmatcuk/doublestar/v4 v4.6.1 // indirect
	github.com/bytedance/gopkg v0.1.3 // indirect
	github.com/bytedance/sonic v1.15.0 // indirect
	github.com/bytedance/sonic/loader v0.5.0 // indirect
	github.com/casbin/govaluate v1.3.0 // indirect
	github.com/casbin/redis-watcher/v2 v2.5.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/gabriel-vasile/mimetype v1.4.13 // indirect
	github.com/gin-contrib/sse v1.1.0 // indirect
	github.com/gin-gonic/gin v1.12.0 // indirect
	github.com/glebarez/go-sqlite v1.20.3 // indirect
	github.com/glebarez/sqlite v1.7.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/jsonpointer v0.19.5 // indirect
	github.com/go-openapi/jsonreference v0.19.6 // indirect
	github.com/go-openapi/spec v0.20.4 // indirect
	github.com/go-openapi/swag v0.19.15 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.30.3 // indirect
	github.com/go-sql-driver/mysql v1.8.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/goccy/go-json v0.10.5 // indirect
	github.com/goccy/go-yaml v1.19.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	github.com/hibiken/asynq v0.26.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.6.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/knadh/koanf/maps v0.1.2 // indirect
	github.com/knadh/koanf/parsers/yaml v1.1.1 // indirect
	github.com/knadh/koanf/providers/confmap v1.0.1 // indirect
	github.com/knadh/koanf/providers/file v1.2.1 // indirect
	github.com/knadh/koanf/v2 v2.3.6 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/mailru/easyjson v0.7.6 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-sqlite3 v1.14.22 // indirect
	github.com/microsoft/go-mssqldb v1.6.0 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/panjf2000/ants/v2 v2.12.1 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/prometheus/client_golang v1.23.2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/quic-go/quic-go v0.59.0 // indirect
	github.com/redis/go-redis/v9 v9.21.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230126093431-47fa9a501578 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/swaggest/swgui v1.8.9 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/ugorji/go/codec v1.3.1 // indirect
	github.com/vearutop/statigz v1.4.0 // indirect
	github.com/xbcio/xbc/extensions/authentication v0.1.0 // indirect
	github.com/xbcio/xbc/extensions/coordination/lease v0.1.0 // indirect
	github.com/xbcio/xbc/extensions/observability/metrics v0.1.0 // indirect
	github.com/xbcio/xbc/extensions/observability/tracing v0.1.0 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.mongodb.org/mongo-driver/v2 v2.5.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/arch v0.22.0 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/mod v0.35.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	golang.org/x/tools v0.44.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gorm.io/driver/mysql v1.6.0 // indirect
	gorm.io/driver/postgres v1.6.0 // indirect
	gorm.io/driver/sqlite v1.6.0 // indirect
	gorm.io/driver/sqlserver v1.6.0 // indirect
	gorm.io/plugin/dbresolver v1.6.0 // indirect
	modernc.org/libc v1.22.2 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
	modernc.org/sqlite v1.20.3 // indirect
)
