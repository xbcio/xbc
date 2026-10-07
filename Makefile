GO ?= go
GOFMT ?= gofmt
MODULE_RUNNER := ./scripts/for-each-module
ORDER_RUNNER := ./scripts/test-order-modules

.PHONY: help fmt fmt-check vet test test-race test-order check

help:
	@printf '%s\n' \
		'make fmt        format all Go source files' \
		'make check      format-check, vet, and test every workspace module' \
		'make test       test every workspace module without cache' \
		'make test-race  race-test every workspace module without cache' \
		'make test-order race-test shutdown ordering/drain budgets at -count=20'

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

# test-order is the shutdown-ordering and drain-budget gate: every test whose
# name matches "Shutdown|Unwind|Order|Drain", run at -race -count=20 so a
# scheduling-dependent ordering regression (reverse graph order, the ingress
# closure, the drain phase, a shared shutdown budget) has twenty chances to
# show up instead of one. It covers every workspace module that actually has
# a matching test -- see scripts/test-order-modules for the module list and
# why it is explicit rather than every entry in go.work.
test-order:
	GO="$(GO)" $(ORDER_RUNNER)

check: fmt-check vet test
