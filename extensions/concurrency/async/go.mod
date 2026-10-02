module github.com/xbcio/xbc/extensions/concurrency/async

go 1.25.0

// github.com/xbcio/xbc is deliberately absent from require while the core
// module has no published tag. Repository development resolves it through
// go.work; adding a local replace or placeholder version here would make this
// independently published extension unusable downstream.
//
// This module names no transport and depends on nothing beyond core and the
// standard library: the goroutine executor used in this step needs nothing
// else, and the later ants-backed executor stays an optional addition rather
// than a required one. testify is a test-only requirement and never reaches a
// consumer's build.
require github.com/stretchr/testify v1.12.1

require go.yaml.in/yaml/v3 v3.0.5 // indirect
