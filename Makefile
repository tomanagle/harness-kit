# Harness Kit developer tasks.
GO      ?= go
BIN     := bin/harness-kit
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build install run test test-go test-ts test-providers test-live lint wasm-check cross client-ts clean

# Where `make install` puts the binary (must be on your PATH).
INSTALL_DIR ?= $(HOME)/.local/bin

all: lint test build

build:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/harness-kit

# Copy the CLI onto your PATH: then run `harness-kit runtimes`, `harness-kit prompt ...`.
install: build
	install -d $(INSTALL_DIR)
	install -m 0755 $(BIN) $(INSTALL_DIR)/harness-kit
	@echo "installed $(INSTALL_DIR)/harness-kit"

# Run the CLI without installing it: make run ARGS="runtimes discover"
run: build
	@./$(BIN) $(ARGS)

test: test-go test-ts test-providers

test-go:
	$(GO) test -race ./...

# Separate SDK modules; these tests use local HTTP fixtures, never paid APIs.
test-providers:
	cd providers/openai && $(GO) vet ./... && $(GO) test -race ./... && $(GO) build ./...
	cd providers/anthropic && $(GO) vet ./... && $(GO) test -race ./... && $(GO) build ./...
	cd examples/model && $(GO) vet ./... && $(GO) test -race ./... && $(GO) build -o ../../bin/model-example .

test-ts:
	cd packages/client-ts && npm install --no-audit --no-fund && npm run typecheck && npm test

# Runs against the real Codex and Claude Code CLIs. Uses a little of your quota.
test-live:
	HARNESS_KIT_LIVE=1 $(GO) test -count=1 -run TestLive -v ./runtimes/...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run gofmt -w ." && exit 1)
	$(GO) vet ./...

# The transport-neutral core must build for WebAssembly and must never
# import process-spawning code.
CORE := ./config ./settings ./event ./permission ./mcp ./tools ./model ./contextwindow ./agent ./session ./runtimes/fake ./rpc/protocol ./rpc/server
wasm-check:
	GOOS=wasip1 GOARCH=wasm $(GO) build $(CORE)
	GOOS=js GOARCH=wasm $(GO) build $(CORE)
	@for p in $(CORE); do \
	  if $(GO) list -deps $$p | grep -qE '^(os/exec|github.com/tomanagle/harness-kit/process)$$'; then \
	    echo "$$p must not depend on os/exec or process"; exit 1; fi; done
	@echo "core is WASM-portable"

cross:
	for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
	  GOOS=$${t%/*} GOARCH=$${t#*/} $(GO) build -o /dev/null ./cmd/harness-kit || exit 1; done

# Build the TypeScript client into packages/client-ts/dist (used by the
# Electron and Wails examples).
client-ts:
	cd packages/client-ts && npm install --no-audit --no-fund --silent && npm run build --silent

clean:
	rm -rf bin packages/client-ts/dist
	find examples/wails/frontend/dist -mindepth 1 ! -name .gitkeep -exec rm -rf {} +

# ---- examples -----------------------------------------------------------------
#
#   make example                 list the examples
#   make example <name>          run one (or: make example-<name>)
#
#   RUNTIME=fake|codex|claude    runtime to use (default: fake, offline)
#   PROMPT="..."                 prompt for cli and node
#
# make can't take flags like --electron (it reads dashed words as its own
# options), hence `make example electron`.

RUNTIME ?= fake
PROMPT  ?= Hello from the example!
EXAMPLES := overview cli node tools electron wails

.PHONY: example examples $(addprefix example-,$(EXAMPLES)) example-electron-smoke

# `make example electron`: treat the word after "example" as its argument.
ifeq (example,$(firstword $(MAKECMDGOALS)))
  EXAMPLE_NAME := $(word 2,$(MAKECMDGOALS))
  ifneq ($(EXAMPLE_NAME),)
    $(eval $(EXAMPLE_NAME):;@:)
  endif
endif

example:
	@if [ -z "$(EXAMPLE_NAME)" ]; then $(MAKE) --no-print-directory examples; \
	elif echo " $(EXAMPLES) " | grep -q " $(EXAMPLE_NAME) "; then $(MAKE) --no-print-directory example-$(EXAMPLE_NAME); \
	else echo "unknown example '$(EXAMPLE_NAME)'"; $(MAKE) --no-print-directory examples; exit 2; fi

examples:
	@echo "Examples (make example <name>, or make example-<name>):"
	@echo "  overview  runtimes, your sessions, host tools and MCP servers (JSON=1 for JSON)"
	@echo "  cli       Go harness linking the library directly; interactive chat"
	@echo "  node      Node.js using the sidecar (the Electron main-process code path)"
	@echo "  tools     host tools registered from Node, with an approval"
	@echo "  electron  Electron dashboard: runtimes, sessions, tools, MCP, chat (downloads Electron once)"
	@echo "  wails     the same dashboard as a Wails app (HARNESS_KIT_MODE=direct for in-process)"
	@echo ""
	@echo "Options: RUNTIME=fake|codex|claude (default fake), PROMPT=\"...\""
	@echo "  e.g.  make example tools RUNTIME=codex"

example-overview: build
	@node examples/node/overview.ts $(if $(JSON),--json)

# A prompt on the command line runs one turn; otherwise it's a chat.
example-cli:
	$(GO) run ./examples/cli -runtime $(RUNTIME) $(if $(filter command line,$(origin PROMPT)),"$(PROMPT)")

example-node: build
	node examples/node/index.ts $(RUNTIME) "$(PROMPT)"

example-tools: build
	node examples/node/tools.ts $(RUNTIME)

# ELECTRON_RUN_AS_NODE leaks from Electron-based terminals and editors and
# would run main.js as plain Node, so clear it.
example-electron: build client-ts
	cd examples/electron && npm install --no-audit --no-fund && env -u ELECTRON_RUN_AS_NODE npx electron .

# Runs the Electron app with a hidden window and drives its UI bridge
# (preload -> IPC -> sidecar -> fake runtime, including an approval).
example-electron-smoke: build client-ts
	cd examples/electron && npm install --no-audit --no-fund --silent && env -u ELECTRON_RUN_AS_NODE HARNESS_KIT_SMOKE=1 npx electron .

# Builds the Wails app with plain `go build` (no wails CLI needed), with the
# shared dashboard from examples/ui, and runs it in sidecar mode;
# HARNESS_KIT_MODE=direct links Harness Kit in-process instead.
# On Linux, Wails also needs the webkit2gtk development packages.
WAILS_LDFLAGS := $(if $(filter Darwin,$(shell uname -s)),-framework UniformTypeIdentifiers)
WAILS_DIST := examples/wails/frontend/dist
example-wails: build client-ts
	mkdir -p $(WAILS_DIST)/vendor
	cp packages/client-ts/dist/*.js $(WAILS_DIST)/vendor/
	cp examples/ui/app.js examples/ui/style.css examples/ui/api.js examples/ui/demo-tools.js examples/ui/wails-bridge.js $(WAILS_DIST)/
	cp examples/ui/wails.html $(WAILS_DIST)/index.html
	cd examples/wails && CGO_LDFLAGS="$(WAILS_LDFLAGS)" $(GO) build -tags desktop,production -o ../../bin/wails-example .
	HARNESS_KIT_BIN=$(CURDIR)/$(BIN) ./bin/wails-example
