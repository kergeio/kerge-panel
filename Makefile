SHELL := bash
GO ?= go
BIN := bin
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.8.0

.PHONY: all build test lint fmt-check vet langcheck cfips commitcheck dcocheck vuln css css-check tzlist check clean

all: check

## build: build the panel binary into bin/
build:
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN)/ ./cmd/panel

## test: run all tests with the race detector
test:
	$(GO) test -race ./...

## lint: formatting, vet, English-only check, Cloudflare examples in sync
lint: fmt-check vet langcheck cfips

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

langcheck:
	set -o pipefail; git ls-files -z | $(GO) run ./tools/langcheck files

cfips:
	./scripts/check-cloudflare-ips.sh

## commitcheck: check that all commit messages are English
commitcheck:
	set -o pipefail; git log -z --format='%H%n%B' | $(GO) run ./tools/langcheck commits

## dcocheck: check that every commit carries a DCO sign-off by its author
dcocheck:
	set -o pipefail; git log -z --no-merges --format='%H%n%an <%ae>%n%B' | $(GO) run ./tools/dcocheck

## vuln: scan dependencies for known vulnerabilities
vuln:
	$(GO) run $(GOVULNCHECK) ./...

## css: compile web/static/app.css with the pinned Tailwind CLI
css:
	./scripts/tailwind.sh -i web/input.css -o web/static/app.css --minify

## css-check: fail if the committed app.css is not what css would produce
css-check:
	@tmp=$$(mktemp); trap 'rm -f "$$tmp"' EXIT; \
	./scripts/tailwind.sh -i web/input.css -o "$$tmp" --minify; \
	if ! cmp -s "$$tmp" web/static/app.css; then \
		echo "web/static/app.css is out of date; run 'make css' and commit the result"; exit 1; \
	fi

## tzlist: regenerate the time zone list from the Go toolchain's tz data
tzlist:
	$(GO) run ./tools/tzgen > internal/i18n/zones.go

## check: everything CI runs
check: lint css-check test vuln commitcheck dcocheck build

clean:
	rm -rf $(BIN)
