module github.com/xbcio/xbc/tests/composition

go 1.25.0

// XBC modules imported below have no published tags yet, so they
// intentionally do not appear in require. Repository development resolves
// them through the root go.work; local replace directives, v0.0.0, and
// synthetic pseudo-versions would only hide a broken publication boundary.
require github.com/stretchr/testify v1.12.1

require go.yaml.in/yaml/v3 v3.0.5 // indirect
