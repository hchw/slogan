.PHONY: test vet build fmt dev-infra dev-up dev-down migrate run-api run-gateway run-worker \
	web-install web-dev web-build web-test compose-check e2e smoke help

TEST_DATABASE_URL ?= postgres://slogan:slogan@127.0.0.1:5432/slogan?sslmode=disable
TEST_REDIS_ADDR ?= 127.0.0.1:6379

help:
	@printf '%s\n' \
		'go:      build vet fmt test' \
		'web:     web-install web-build web-test' \
		'local:   dev-infra dev-up dev-down migrate run-api run-gateway run-worker' \
		'release: compose-check e2e smoke'

build:
	go build ./cmd/api ./cmd/gateway ./cmd/worker

vet:
	go vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './web/node_modules/*' -not -path './.git/*')

# Unit plus integration suites. Integration tests skip themselves when
# TEST_DATABASE_URL/TEST_REDIS_ADDR are not reachable.
test:
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' TEST_REDIS_ADDR='$(TEST_REDIS_ADDR)' go test ./... -count=1

dev-infra:
	cd deploy && docker compose up -d postgres redis

# Full local stack: Go API/Gateway/Worker, PostgreSQL, Redis and the static web.
dev-up:
	GOPROXY=$${GOPROXY:-https://goproxy.cn,direct} DOCKER_BUILDKIT=1 COMPOSE_DOCKER_CLI_BUILD=1 docker compose -f deploy/docker-compose.yml --profile app up -d --build

dev-down:
	cd deploy && docker compose --profile laya --profile app down

migrate:
	cd deploy && docker compose --profile app run --rm api --migrate-only

run-api:
	go run ./cmd/api

run-gateway:
	go run ./cmd/gateway

run-worker:
	go run ./cmd/worker

web-install:
	npm --prefix web ci

web-dev:
	npm --prefix web run dev

web-build:
	npm --prefix web run build

web-test:
	npm --prefix web test
	npm --prefix web run lint

# Static validation that the delivery compose file is well formed.
compose-check:
	cd deploy && LAYA_IMAGE=slogan-laya:0.3.20 docker compose --profile app --profile laya config -q

# End-to-end flow against a running deployment (see scripts/e2e_smoke.sh).
e2e:
	./scripts/e2e_smoke.sh

# Laya host preflight plus sidecar smoke; requires the laya profile running.
smoke:
	./deploy/laya/preflight.sh
	./deploy/laya/status.sh
	./deploy/laya/smoke.sh
