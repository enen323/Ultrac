# Build entry points for the Go engine.
#
# The TypeScript tree still builds with bun (`bun run build`); these targets
# only cover cmd/ and internal/. Make is a convenience, not a requirement —
# every target is a plain `go` invocation you can run by hand.

GO      ?= go
GOEXE   := $(shell $(GO) env GOEXE)
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILT   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# -s -w strips the symbol table and DWARF data. Version metadata goes in via
# -X so `go build` with no flags still produces a working binary that reports
# itself honestly as "dev".
LDFLAGS := -s -w \
	-X ultrac/internal/version.Version=$(VERSION) \
	-X ultrac/internal/version.GitCommit=$(COMMIT) \
	-X ultrac/internal/version.BuildTimeUTC=$(BUILT)

.PHONY: build vet test fmt clean check

build:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o bin/ultrac$(GOEXE) ./cmd/ultrac

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin

# The local quality gate. `go vet` alone misses nothing here yet, but the
# target exists so that adding checks later does not change the command
# reviewers and agents are told to run.
check: vet test
