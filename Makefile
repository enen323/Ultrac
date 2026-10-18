# Build entry points for the Go engine.
#
# Make is a convenience, not a requirement — every target is a plain `go`
# invocation you can run by hand.

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

.PHONY: build vet test fmt clean check live-agent

build:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o bin/ultrac$(GOEXE) ./cmd/ultrac

vet:
	$(GO) vet ./cmd/... ./internal/...

test:
	$(GO) test ./cmd/... ./internal/...

fmt:
	$(GO) fmt ./cmd/... ./internal/...

# The local quality gate.
check: vet test

# live-agent is the real-endpoint acceptance path: the pi-semantics print mode
# against whatever provider ~/.ultrac/settings.json configures. Separate from
# `test` because it spends the user's quota and needs network. Every prompt
# here is read-only. The json line count is the cheap check that the event
# stream is a stream: one JSON object per line, opened by the session header.
#
# The T0-era `live` target (doctor + ask) died with those commands in P7;
# `ultrac probe` covers the endpoint check now.
live-agent: build
	./bin/ultrac$(GOEXE) -p "Reply with exactly: P5-OK"
	./bin/ultrac$(GOEXE) -p "Read go.mod and say the module name in one sentence"
	./bin/ultrac$(GOEXE) --mode json -p "List the Go files under internal/session" | wc -l

clean:
	rm -rf bin
