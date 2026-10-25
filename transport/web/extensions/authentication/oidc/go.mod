module github.com/xbcio/xbc/transport/web/extensions/authentication/oidc

go 1.25.0

// XBC core, the authentication contract, the neutral OIDC client, the neutral
// session module, the Web transport, and the shared federation Web base
// resolve through their published release tags; repository development
// resolves the same source through go.work.
require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/go-jose/go-jose/v4 v4.1.6
	github.com/redis/go-redis/v9 v9.21.0
	github.com/xbcio/xbc v0.1.0
	github.com/xbcio/xbc/extensions/authentication v0.3.0
	github.com/xbcio/xbc/extensions/authentication/oidc v0.2.0
	github.com/xbcio/xbc/transport/web v0.2.0
	github.com/xbcio/xbc/transport/web/extensions/authentication/federation v0.1.0
)

require github.com/xbcio/xbc/extensions/authentication/session v0.2.0

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/gabriel-vasile/mimetype v1.4.13 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.30.3 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/knadh/koanf/maps v0.1.2 // indirect
	github.com/knadh/koanf/parsers/yaml v1.1.1 // indirect
	github.com/knadh/koanf/providers/confmap v1.0.1 // indirect
	github.com/knadh/koanf/providers/file v1.2.1 // indirect
	github.com/knadh/koanf/v2 v2.3.6 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/xbcio/xbc/transport/web/extensions/authentication/session v0.1.0 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)
