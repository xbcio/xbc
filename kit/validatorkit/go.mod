module github.com/xbcio/xbc/kit/validatorkit

go 1.26.0

// This module is framework-free by construction: it depends on the
// go-playground validator libraries only, never on any github.com/xbcio/xbc
// package. See tests/architecture/kit_modules_test.go.
require (
	github.com/go-playground/locales v0.14.1
	github.com/go-playground/universal-translator v0.18.1
	github.com/go-playground/validator/v10 v10.30.3
	github.com/stretchr/testify v1.12.1
)

require (
	github.com/gabriel-vasile/mimetype v1.4.13 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)
