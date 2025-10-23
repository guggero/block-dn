PKG := github.com/guggero/block-dn
TOOLS_DIR := tools

GOTEST := go test -v

GO_BIN := ${GOPATH}/bin

GOFILES_NOVENDOR = $(shell find . -type f -name '*.go' -not -path "./vendor/*")
GOLIST := go list $(PKG)/... | grep -v '/vendor/'

GOIMPORTS_BIN := $(GO_BIN)/gosimports
GOIMPORTS_PKG := github.com/rinchsan/gosimports/cmd/gosimports

GOBUILD := go build -v
GOINSTALL := go install -v
GOTEST := go test -v
XARGS := xargs -L 1

VERSION_TAG = $(shell git describe --tags)
VERSION_CHECK = @$(call print, "Building master with date version tag")

BUILD_SYSTEM = darwin-amd64 \
linux-386 \
linux-amd64 \
linux-armv6 \
linux-armv7 \
linux-arm64 \
windows-386 \
windows-amd64 \
windows-arm

# By default we will build all systems. But with the 'sys' tag, a specific
# system can be specified. This is useful to release for a subset of
# systems/architectures.
ifneq ($(sys),)
BUILD_SYSTEM = $(sys)
endif

DOCKER_TOOLS = docker run -v $$(pwd):/build block-dn-tools

TEST_TAGS := bitcoind
TEST_FLAGS = -test.timeout=20m -tags="$(TEST_TAGS)"

UNIT := $(GOLIST) | $(XARGS) env $(GOTEST) $(TEST_FLAGS)
LDFLAGS := -X main.Commit=$(shell git describe --tags)
RELEASE_LDFLAGS := -s -w -buildid= $(LDFLAGS)

GREEN := "\\033[0;32m"
NC := "\\033[0m"
define print
	echo $(GREEN)$1$(NC)
endef

default: build

$(GOIMPORTS_BIN):
	@$(call print, "Installing goimports.")
	cd $(TOOLS_DIR); go install -trimpath $(GOIMPORTS_PKG)

unit: 
	@$(call print, "Running unit tests.")
	$(UNIT)

build:
	@$(call print, "Building block-dn.")
	$(GOBUILD) -ldflags "$(LDFLAGS)" ./...

install:
	@$(call print, "Installing block-dn.")
	$(GOINSTALL) -ldflags "$(LDFLAGS)" ./...

docker-tools:
	@$(call print, "Building tools docker image.")
	docker build -q -t block-dn-tools $(TOOLS_DIR)

fmt: $(GOIMPORTS_BIN)
	@$(call print, "Fixing imports.")
	gosimports -w $(GOFILES_NOVENDOR)
	@$(call print, "Formatting source.")
	gofmt -l -w -s $(GOFILES_NOVENDOR)

fmt-check: fmt
	@$(call print, "Checking fmt results.")
	if test -n "$$(git status --porcelain)"; then echo "code not formatted correctly, please run `make fmt` again!"; git status; git diff; exit 1; fi

lint: docker-tools
	@$(call print, "Linting source.")
	$(DOCKER_TOOLS) golangci-lint run -v $(LINT_WORKERS)

docs: install
	@$(call print, "Rendering docs.")
	cfilter-cdn doc
