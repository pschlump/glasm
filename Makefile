

.PHONY: build test glua fuzz-soak fuzz-status

# Version = latest git tag, "v" stripped; when the tree has no tags the
# fallback literal in package.go stands (no -X is injected).
VERSION := $(shell git tag --sort=v:refname 2>/dev/null | tail -1 | sed 's/^v//')

# Build information (git commit, tag, build date) is injected at link time via
# -ldflags; version.go and package.go are static and never regenerated.
LDFLAGS := -X 'main.GitCommit=$(shell git rev-list -1 HEAD)' \
           -X 'main.GitTag=$(shell git tag --sort=v:refname | tail -1)' \
           -X 'main.BuildDate=$(shell date)'
ifneq ($(strip $(VERSION)),)
LDFLAGS += -X 'github.com/pschlump/glasm.PackageVersion=$(VERSION)'
endif

build:
	./_tools/go-inline *.go && go fmt . && go build -ldflags "$(LDFLAGS)" -o cmd/glua/glua ./cmd/glua

glua: build

test:
	./_tools/go-inline *.go && go fmt . &&  go test





fuzz-soak: build
	go run ./cmd/luafuzz -soak

fuzz-status:
	go run ./cmd/luafuzz -status
