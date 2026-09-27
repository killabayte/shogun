# Get the latest commit branch, hash, and date
TAG=$(shell git describe --tags --abbrev=0 --exact-match 2>/dev/null)
BRANCH=$(if $(TAG),$(TAG),$(shell git rev-parse --abbrev-ref HEAD 2>/dev/null))
HASH=$(shell git rev-parse --short=7 HEAD 2>/dev/null)
# git formats the date itself, so the value is the same on macOS and Linux
TIMESTAMP=$(shell TZ=UTC0 git log -1 --date=format-local:%Y%m%dT%H%M%S --format=%cd HEAD 2>/dev/null)
GIT_REV=$(shell printf "%s-%s-%s" "$(BRANCH)" "$(HASH)" "$(TIMESTAMP)")
REV=$(if $(filter --,$(GIT_REV)),latest,$(GIT_REV))
# an exact release tag (v0.1.0) becomes the version; other builds keep the source default
VERSION_FLAG=$(if $(filter v%,$(TAG)),-X main.version=$(TAG:v%=%))

# where `make install` links the binary; override for a prefix that needs no privileges
BINDIR ?= /usr/local/bin

GOFILES=$(shell find . -type f -name "*.go" -not -path "./vendor/*")

all: test build

# cp then mv: a rename leaves a running shogun on its old inode, so rebuilding while a plan is
# running does not rewrite the pages of a live binary (macOS kills such a process).
build:
	@mkdir -p .bin
	go build -ldflags "-X main.revision=$(REV) $(VERSION_FLAG) -s -w" -o .bin/shogun.$(BRANCH) ./cmd/shogun
	cp .bin/shogun.$(BRANCH) .bin/shogun.tmp && mv -f .bin/shogun.tmp .bin/shogun

# symlink rather than copy, so every later `make build` is picked up without reinstalling; rm before
# ln instead of `ln -sf`, which BSD ln resolves through an existing link to a directory.
install: build
	rm -f "$(BINDIR)/shogun"
	ln -s "$(CURDIR)/.bin/shogun" "$(BINDIR)/shogun"
	@echo "$(BINDIR)/shogun -> $(CURDIR)/.bin/shogun"

uninstall:
	rm -f "$(BINDIR)/shogun"

test:
	go clean -testcache
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	rm coverage.out

# the provider tests re-exec the race-instrumented test binary as a fake CLI once per case, so this
# costs ~40s; the timeout is headroom for slower machines, not a budget
race:
	go test -race -timeout=300s ./...

# go vet and gofmt always; golangci-lint only where it is installed
lint:
	go vet ./...
	@test -z "$$(gofmt -s -l $(GOFILES))" || (echo "gofmt needed:"; gofmt -s -l $(GOFILES); exit 1)
	@if command -v golangci-lint >/dev/null; then golangci-lint run --max-issues-per-linter=0 --max-same-issues=0; else echo "golangci-lint not installed, skipped"; fi

fmt:
	gofmt -s -w $(GOFILES)
	@if command -v goimports >/dev/null; then goimports -w $(GOFILES); fi

version:
	@echo "branch: $(BRANCH), hash: $(HASH), timestamp: $(TIMESTAMP)"
	@echo "revision: $(REV)"

.PHONY: all build install uninstall test race lint fmt version
