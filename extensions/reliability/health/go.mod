module github.com/xbcio/xbc/extensions/reliability/health

go 1.25.0

// github.com/xbcio/xbc is deliberately absent from require while the core
// module has no published tag. Repository development resolves it through
// go.work; adding a local replace or placeholder version here would make this
// independently published extension unusable downstream.
//
// Every transport adapter and every dependency plugin that contributes a probe
// imports this module, so its production closure is the standard library alone.
// testify is a test-only requirement and never reaches a consumer's build.
require github.com/stretchr/testify v1.12.1

require go.yaml.in/yaml/v3 v3.0.5 // indirect
