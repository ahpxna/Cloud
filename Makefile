SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available commands
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n",$$1,$$2}'

.PHONY: env
env: ## Create .env from the safe example when absent
	@test -f .env || cp .env.example .env

.PHONY: config
config: env ## Validate the Compose model without starting containers
	docker compose config --quiet

.PHONY: db-up
db-up: env ## Start PostgreSQL for local development
	docker compose up -d --wait postgres

.PHONY: protocol-lab-up
protocol-lab-up: env ## Start the loopback-only tus protocol lab
	docker compose --profile protocol-lab up -d --wait postgres tusd-lab

.PHONY: gateway-up
gateway-up: env ## Start the authenticated gateway on loopback (requires token key)
	docker compose --profile gateway up -d --build --wait postgres upload-gateway

.PHONY: edge-up
edge-up: env ## Start gateway plus Cloudflare Tunnel after configuring its token
	@test -n "$$(sed -n 's/^CLOUDFLARE_TUNNEL_TOKEN=//p' .env)" || (echo "set CLOUDFLARE_TUNNEL_TOKEN in .env"; exit 1)
	docker compose --profile gateway --profile edge up -d --build --wait postgres upload-gateway cloudflared

.PHONY: create-user
create-user: ## Create an invite-only family user in the running gateway stack
	@test -n "$(EMAIL)" || (echo "usage: make create-user EMAIL=name@example.com [ROLE=member]"; exit 1)
	docker compose --profile gateway exec upload-gateway admin create-user -email "$(EMAIL)" -role "$(or $(ROLE),member)"

.PHONY: status
status: ## Show local container state
	docker compose --profile protocol-lab ps --all

.PHONY: logs
logs: ## Tail local service logs
	docker compose --profile protocol-lab logs -f --tail=100

.PHONY: down
down: ## Stop local services without deleting data
	docker compose --profile protocol-lab down --remove-orphans

.PHONY: test
test: config ## Run Go tests, vet, and repository contract checks
	GOMODCACHE=$(CURDIR)/.cache/go-mod GOPATH=$(CURDIR)/.cache/go go test ./...
	GOMODCACHE=$(CURDIR)/.cache/go-mod GOPATH=$(CURDIR)/.cache/go go vet ./...
	@for sql in db/migrations/*.sql; do test -s "$$sql" || exit 1; done
	@echo "repository checks passed"

.PHONY: ios-parse
ios-parse: ## Parse the iOS/Share Extension scaffold without full Xcode
	swiftc -parse $$(rg --files ios -g '*.swift')
	ruby -e 'require "yaml"; YAML.load_file("ios/project.yml")'

.PHONY: integration-test
integration-test: ## Run PostgreSQL 18 integration test without Docker
	GOMODCACHE=$(CURDIR)/.cache/go-mod GOPATH=$(CURDIR)/.cache/go GOCACHE=$(CURDIR)/.cache/go-build go test -tags=integration -count=1 ./internal/upload
