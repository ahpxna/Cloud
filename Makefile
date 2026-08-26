SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help
ALL_PROFILES := --profile gateway --profile edge --profile protocol-lab --profile integrity --profile observability --profile audit

.PHONY: help
help: ## Show available commands
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n",$$1,$$2}'

.PHONY: env
env: ## Create .env from the safe example when absent
	@test -f .env || cp .env.example .env

.PHONY: config
config: env ## Validate the Compose model without starting containers
	docker compose $(ALL_PROFILES) config --quiet

.PHONY: resolve-image-digests
resolve-image-digests: ## Resolve external Compose tags to immutable digest refs
	bash scripts/resolve-image-digests.sh

.PHONY: verify-image-digests
verify-image-digests: ## Fail unless external Compose images are pinned by sha256 digest
	bash scripts/verify-image-digests.sh

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

.PHONY: observability-up
observability-up: env ## Start private metrics, Prometheus, Alertmanager, and Grafana
	docker compose --profile observability up -d --build --wait postgres metrics-exporter prometheus alertmanager grafana

.PHONY: create-user
create-user: ## Create an invite-only family user in the running stack
	@test -n "$(EMAIL)" || (echo "usage: make create-user EMAIL=name@example.com [ROLE=member]"; exit 1)
	docker compose --profile admin run --rm admin create-user -email "$(EMAIL)" -role "$(or $(ROLE),member)"

.PHONY: scrub
scrub: env ## Re-read and SHA-256 every committed original
	docker compose --profile integrity run --rm scrub $(SCRUB_ARGS)

.PHONY: integrity-cycle
integrity-cycle: env ## Full-byte scrub followed by a new signed manifest
	bash scripts/integrity-cycle.sh

.PHONY: manifest-verify
manifest-verify: env ## Verify one signed manifest with the public trust key
	@test -n "$(MANIFEST_FILE)" || (echo "usage: make manifest-verify MANIFEST_FILE=manifest-...json [MANIFEST_ARGS=...]"; exit 1)
	docker compose --profile integrity run --rm manifest-verify -mode verify -input "/manifests/$(notdir $(MANIFEST_FILE))" -object-key "manifests/$(notdir $(MANIFEST_FILE))" $(MANIFEST_ARGS)

.PHONY: manifest-reconcile
manifest-reconcile: env ## Repair a verified manifest file -> DB linkage crash window
	@test -n "$(MANIFEST_FILE)" -a -n "$(OBJECT_KEY)" || (echo "usage: make manifest-reconcile MANIFEST_FILE=manifest-...json OBJECT_KEY=manifests/manifest-...json"; exit 1)
	docker compose --profile integrity run --rm manifest-verify -mode reconcile -input "/manifests/$(notdir $(MANIFEST_FILE))" -object-key "$(OBJECT_KEY)"

.PHONY: audit-export
audit-export: env ## Export append-only upload events to JSONL + SHA-256
	bash scripts/export-audit.sh

.PHONY: backup
backup: env ## Create a quiescent encrypted restic backup
	bash scripts/backup-restic.sh

.PHONY: restore-drill
restore-drill: env ## Restore a snapshot into isolation and re-hash all originals
	bash scripts/restore-drill.sh $(RESTORE_ARGS)

.PHONY: synthetic-probe
synthetic-probe: ## Exercise login -> resumable upload -> verify -> download SHA-256
	@test -n "$(BASE_URL)" -a -n "$(EMAIL)" -a -n "$(PASSWORD_FILE)" || (echo "usage: make synthetic-probe BASE_URL=https://... EMAIL=probe@example.com PASSWORD_FILE=/path/to/password [PROBE_ARGS=...]"; exit 1)
	go run ./cmd/synthetic-probe -base-url "$(BASE_URL)" -email "$(EMAIL)" -password-file "$(PASSWORD_FILE)" $(PROBE_ARGS)

.PHONY: synthetic-probe-docker
synthetic-probe-docker: env ## Run the synthetic probe inside the private Docker ingress network
	@test -n "$(EMAIL)" -a -n "$(PASSWORD_FILE)" || (echo "usage: make synthetic-probe-docker EMAIL=probe@example.com PASSWORD_FILE=/absolute/path/to/password [PROBE_ARGS=...]"; exit 1)
	@test -f "$(PASSWORD_FILE)" || (echo "probe password file not found: $(PASSWORD_FILE)"; exit 1)
	docker compose --profile gateway run --rm \
		-v "$(abspath $(PASSWORD_FILE)):/run/secrets/probe-password:ro" \
		synthetic-probe \
		-base-url http://upload-gateway:8080 \
		-allow-http-host upload-gateway \
		-email "$(EMAIL)" \
		-password-file /run/secrets/probe-password \
		$(PROBE_ARGS)

.PHONY: chaos-resume
chaos-resume: env ## Restart the local gateway during a slow resumable upload
	bash scripts/chaos-resume.sh

.PHONY: install-systemd
install-systemd: ## Install backup/integrity timers on a Linux host
	@test "$$(id -u)" -eq 0 || (echo "run with sudo: sudo make install-systemd"; exit 1)
	install -m 0644 deploy/systemd/family-photo-cloud-integrity.service /etc/systemd/system/
	install -m 0644 deploy/systemd/family-photo-cloud-integrity.timer /etc/systemd/system/
	install -m 0644 deploy/systemd/family-photo-cloud-backup.service /etc/systemd/system/
	install -m 0644 deploy/systemd/family-photo-cloud-backup.timer /etc/systemd/system/
	systemctl daemon-reload
	@echo "Review unit paths/environment, then enable explicitly: systemctl enable --now family-photo-cloud-{integrity,backup}.timer"

.PHONY: status
status: ## Show local container state across all profiles
	docker compose $(ALL_PROFILES) ps --all

.PHONY: logs
logs: ## Tail local service logs across all profiles
	docker compose $(ALL_PROFILES) logs -f --tail=100

.PHONY: down
down: ## Stop local services without deleting data
	docker compose $(ALL_PROFILES) down --remove-orphans

.PHONY: test
test: config ## Run Go tests, vet, formatting, shell, and repository contract checks
	@test -z "$$(gofmt -l cmd internal)" || (echo "gofmt required:"; gofmt -l cmd internal; exit 1)
	find scripts -type f -name '*.sh' -print0 | xargs -0 -n1 bash -n
	GOMODCACHE=$(CURDIR)/.cache/go-mod GOPATH=$(CURDIR)/.cache/go go test ./...
	GOMODCACHE=$(CURDIR)/.cache/go-mod GOPATH=$(CURDIR)/.cache/go go vet ./...
	@for sql in db/migrations/*.sql; do test -s "$$sql" || exit 1; done
	@echo "repository checks passed"

.PHONY: ios-parse
ios-parse: ## Parse the iOS/Share Extension scaffold without full Xcode
	swiftc -parse $$(rg --files ios -g '*.swift')
	ruby -e 'require "yaml"; YAML.load_file("ios/project.yml")'

.PHONY: host-preflight
host-preflight: ## Read-only Linux/media-disk readiness check for the future host
	bash scripts/host-preflight.sh

.PHONY: ios-test
ios-test: ## Generate the iOS project and run simulator XCTest (requires full Xcode + XcodeGen)
	bash scripts/ios-test.sh

.PHONY: integration-test
integration-test: ## Run PostgreSQL 18 upload + account/MFA integration tests without Docker
	GOMODCACHE=$(CURDIR)/.cache/go-mod GOPATH=$(CURDIR)/.cache/go GOCACHE=$(CURDIR)/.cache/go-build go test -tags=integration -count=1 ./internal/upload ./internal/account
