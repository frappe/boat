# Boat's build. Deliberately short: one host binary, its tests, and the codegen
# step that keeps internal/wire in sync with the IDL.

BINARY := boat
BUILD_DIRECTORY := bin
VERSION_PACKAGE := github.com/frappe/boat/internal/version

# git describe when there is anything to describe, "dev" otherwise. An unstamped
# build says so plainly rather than claiming a release number: this string is
# what a host reports as its running version, and Atlas treats version drift as
# observed state.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(VERSION_PACKAGE).Version=$(VERSION)

# Pinned: internal/wire is checked in, so the generator version is part of what
# makes that file reproducible.
CODEGEN := github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1

.PHONY: all build test vet fmt fmt-check generate check clean

all: build

# CGO_ENABLED=0 and -trimpath: one static binary that runs on a bare Ubuntu host
# with no toolchain, no libc version to match, and no build paths baked in.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIRECTORY)/$(BINARY) ./cmd/boat

# No CGO_ENABLED=0 here, unlike build: the race detector needs cgo. Boat runs one
# actor per VM against a shared store, so the detector earns its runtime cost.
test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi

# api/openapi.yaml is the contract; internal/wire/wire.gen.go is generated from
# it and checked in, so a build needs neither the network nor the generator. Run
# this after every edit to the IDL, and never hand-edit the generated file.
generate:
	cd api && go run $(CODEGEN) --config codegen.yaml openapi.yaml

check: fmt-check vet test

clean:
	rm -rf $(BUILD_DIRECTORY) coverage.out
