GO ?= go
.PHONY: build run migrate generate check-generated test lint dev security architecture-check
build:
	$(GO) build -o bin/harness-kit-server ./cmd
run:
	$(GO) run ./cmd
migrate:
	$(GO) run ./cmd migrate
generate:
	$(GO) run github.com/a-h/templ/cmd/templ@v0.3.1100 generate
	$(GO) run ./cmd/contractgen
check-generated:
	$(GO) run ./cmd/contractgen -check
test:
	$(GO) test -race ./...
lint:
	$(GO) vet ./...
dev:
	docker compose -f dev/compose.yml up --build

security:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

architecture-check:
	$(GO) test ./internal/architecture

.PHONY: cluster-up cluster-down load-test
cluster-up:
	docker compose -f deploy/compose.yml up --build -d
cluster-down:
	docker compose -f deploy/compose.yml down
load-test:
	docker compose -f deploy/compose.yml run --rm k6

.PHONY: cleanup
cleanup:
	$(GO) run ./cmd cleanup

.PHONY: restore-test
restore-test:
	python3 deploy/restore-smoke.py
