GOOS := $(shell go env GOOS)
VERSION := $(shell cat VERSION)
EXT := so
ifeq ($(GOOS),darwin)
EXT := dylib
endif
ifeq ($(GOOS),windows)
EXT := dll
endif

ifeq ($(shell go env GOHOSTOS)/$(shell go env GOHOSTARCH),linux/amd64)
LINUX_AMD64_CC ?= $(CC)
else
LINUX_AMD64_CC ?= zig cc -target x86_64-linux-gnu.2.17
endif

.PHONY: build build-linux-amd64 package-linux-amd64 test check smoke

build:
	mkdir -p build
	go build -trimpath -buildmode=c-shared -ldflags "-X main.version=$(VERSION)" -o build/cpa-plus.$(EXT) ./src

build-linux-amd64:
	mkdir -p build/linux-amd64
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 GOAMD64=v1 CC="$(LINUX_AMD64_CC)" go build -trimpath -buildmode=c-shared -ldflags "-X main.version=$(VERSION)" -o build/linux-amd64/cpa-plus.so ./src

package-linux-amd64: build-linux-amd64
	python3 scripts/package.py build/linux-amd64/cpa-plus.so

test:
	python3 scripts/go-check.py test -race ./...

check: test
	python3 scripts/go-check.py vet ./...
	@test -z "$$(gofmt -l src test)"

smoke: build
	python3 scripts/smoke.py build/cpa-plus.$(EXT)
