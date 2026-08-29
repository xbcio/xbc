GO ?= go
GOFMT ?= gofmt
MODULE_RUNNER := ./scripts/for-each-module

.PHONY: help fmt fmt-check vet test test-race check

help:
	@printf '%s\n' \
		'make fmt        format all Go source files' \
		'make check      format-check, vet, and test every workspace module' \
		'make test       test every workspace module without cache' \
		'make test-race  race-test every workspace module without cache'

fmt:
	$(GOFMT) -w .

fmt-check:
	@unformatted="$$($(GOFMT) -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "Go files need formatting:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	GO="$(GO)" $(MODULE_RUNNER) vet ./...

test:
	GO="$(GO)" $(MODULE_RUNNER) test ./... -count=1

test-race:
	GO="$(GO)" $(MODULE_RUNNER) test ./... -race -count=1

check: fmt-check vet test
