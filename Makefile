GO   ?= go
PKGS := ./...

# Fuzz targets, as "package:Target" so each runs against the right package.
FUZZERS := \
	./internal/link:FuzzParser \
	./internal/link:FuzzParserChunked \
	./internal/link:FuzzDecode \
	./internal/link:FuzzSecondary \
	./internal/transport:FuzzReassembler \
	./internal/transport:FuzzRoundTrip \
	./internal/transport:FuzzHeader \
	./internal/app:FuzzParseFragment \
	./internal/app:FuzzFragmentRoundTrip \
	./internal/app:FuzzObjectHeader \
	./internal/app:FuzzBuilder \
	./objects:FuzzCodecs \
	./objects:FuzzPacked \
	./objects:FuzzCommands

.DEFAULT_GOAL := check

.PHONY: check
check: fmt-check vet test ## fmt, vet and test — the pre-commit gate

.PHONY: test
test: ## run unit tests with the race detector
	$(GO) test -race $(PKGS)

.PHONY: test-short
test-short: ## run unit tests without the race detector
	$(GO) test $(PKGS)

.PHONY: cover
cover: ## produce coverage.html
	$(GO) test -coverprofile=coverage.out -covermode=atomic $(PKGS)
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "coverage.html written"

.PHONY: vet
vet:
	$(GO) vet $(PKGS)

.PHONY: fmt
fmt:
	gofmt -s -w .

.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: lint
lint: ## golangci-lint, if installed
	@command -v golangci-lint >/dev/null 2>&1 \
		|| { echo "golangci-lint not installed; skipping"; exit 0; }
	golangci-lint run

# Parsers face bytes from devices we do not control over links that corrupt
# them, so fuzzing is part of the normal gate rather than an occasional extra.
.PHONY: fuzz-short
fuzz-short: ## 20s per fuzz target
	@$(MAKE) --no-print-directory fuzz FUZZTIME=20s

.PHONY: fuzz-long
fuzz-long: ## 10m per fuzz target, for nightly runs
	@$(MAKE) --no-print-directory fuzz FUZZTIME=10m

FUZZTIME ?= 20s

.PHONY: fuzz
fuzz:
	@for spec in $(FUZZERS); do \
		pkg=$${spec%%:*}; target=$${spec##*:}; \
		printf '==> %-24s %s\n' "$$target" "$$pkg"; \
		$(GO) test "$$pkg" -run=XXX -fuzz="^$$target$$" -fuzztime=$(FUZZTIME) || exit 1; \
	done

.PHONY: bench
bench:
	$(GO) test -bench=. -benchmem -run=XXX $(PKGS)

.PHONY: generate
generate: ## regenerate the object codecs
	$(GO) generate $(PKGS)

.PHONY: generate-check
generate-check: generate ## fail if generated code is stale
	@git diff --exit-code || { echo "generated code is stale; run 'make generate'"; exit 1; }

# ---------- Interoperability ----------
#
# Agreeing with yourself is not interoperability. These build the two
# implementations that matter and run this stack against them.

.PHONY: interop-build
interop-build: ## build the opendnp3 container
	docker build -f interop/Dockerfile.opendnp3 -t go-dnp3-interop-opendnp3 interop/

.PHONY: interop
interop: ## run our master against their outstations
	$(GO) test -tags interop -v -timeout 10m ./interop/

.PHONY: interop-reverse
interop-reverse: ## drive their masters against our outstation
	./interop/reverse.sh

.PHONY: interop-clean
interop-clean:
	-docker rm -f interop-opendnp3 2>/dev/null
	-docker rmi go-dnp3-interop-opendnp3 2>/dev/null

.PHONY: clean
clean:
	rm -f coverage.out coverage.html
	$(GO) clean -testcache

.PHONY: help
help:
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
