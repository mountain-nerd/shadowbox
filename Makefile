# Uses a local Go toolchain if present, otherwise runs Go in Docker.
GO_IMAGE ?= golang:1.27-alpine
ifeq ($(shell command -v go 2>/dev/null),)
GO = docker run --rm -v $(CURDIR):/src -v shadowbox-gomod:/go/pkg/mod -v shadowbox-gocache:/root/.cache/go-build -w /src -e CGO_ENABLED=1 $(GO_IMAGE) sh -c 'apk add -q --no-cache gcc musl-dev >/dev/null 2>&1; go "$$@"' go
else
GO = go
endif

.PHONY: test race vet tidy build e2e fmt check
fmt:
	$(GO) fmt ./...
# check = gofmt clean + vet + race-enabled suite (what CI should run).
check: vet race
	@test -z "$$(docker run --rm -v $(CURDIR):/src -w /src $(GO_IMAGE) gofmt -l cmd internal)" || (echo "gofmt needed"; exit 1)
test:
	$(GO) test -timeout 120s ./...
race:
	$(GO) test -race -timeout 180s ./...
vet:
	$(GO) vet ./...
tidy:
	$(GO) mod tidy
build:
	$(GO) build ./...
e2e:
	cd e2e && ./run.sh
# Applies targeted production-code mutations and requires the matching tests to fail.
mutation:
	python3 -I scripts/mutation-check.py $(CURDIR)
