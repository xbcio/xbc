module github.com/xbcio/xbc/kit/gormx

go 1.26.0

// This module is framework-free by construction: it depends on GORM and the
// GORM Gen field package, never on any github.com/xbcio/xbc package. See
// tests/architecture/kit_modules_test.go.
require (
	github.com/stretchr/testify v1.12.1
	gorm.io/gen v0.3.27
	gorm.io/gorm v1.31.1
)

require (
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/text v0.40.0 // indirect
)
