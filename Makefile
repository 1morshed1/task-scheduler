# Makefile — the project's command menu. Run `make help` to list targets.
#
# Every target is a thin wrapper over a plain command, so you can always see
# exactly what is being run. Nothing here is magic.

# Local dev database connection string. Override on the command line if needed:
#   make migrate DATABASE_URL=postgres://...
DATABASE_URL ?= postgres://scheduler:scheduler@localhost:5432/taskscheduler?sslmode=disable
export DATABASE_URL

.DEFAULT_GOAL := help

## help: show this list of targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F': ' '{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## build: compile the server and worker binaries into ./bin
build:
	@mkdir -p bin
	go build -o bin/server ./cmd/server
	go build -o bin/worker ./cmd/worker
	@echo "built bin/server and bin/worker"

## run-server: run the API + dashboard + reaper (foreground)
run-server:
	go run ./cmd/server

## run-worker: run one worker process (foreground). Run in several terminals.
run-worker:
	go run ./cmd/worker

## test: run the full test suite (needs a reachable Postgres for integration tests)
test:
	go test ./... -count=1

## test-short: run only the fast, DB-free unit tests
test-short:
	go test ./... -short -count=1

## vet: run go vet static analysis
vet:
	go vet ./...

## fmt: format all Go code
fmt:
	go fmt ./...

## migrate: apply database migrations against $DATABASE_URL, then exit
# The server also auto-migrates on boot; this target runs migrations on their
# own (e.g. a CI step or a k8s init container) via the server's -migrate-only flag.
migrate:
	go run ./cmd/server -migrate-only

## db-up: start the local Postgres via docker-compose
db-up:
	docker compose up -d postgres

## db-down: stop the local Postgres
db-down:
	docker compose down

## up: start the whole stack (postgres + server + a few workers) via docker-compose
up:
	docker compose up --build

## down: stop and remove the whole stack
down:
	docker compose down -v

## seed: submit a handful of demo tasks to a running server on :8080
seed:
	@for i in 1 2 3 4 5; do \
	  curl -s -XPOST localhost:8080/api/v1/tasks -d '{"type":"flaky","max_retries":5}' >/dev/null; done
	@curl -s -XPOST localhost:8080/api/v1/tasks -d '{"type":"fail_always","max_retries":2}' >/dev/null
	@curl -s -XPOST localhost:8080/api/v1/tasks -d '{"type":"sleep","payload":{"ms":3000}}' >/dev/null
	@echo "seeded demo tasks — watch them at http://localhost:8080"

.PHONY: help build run-server run-worker test test-short vet fmt migrate db-up db-down up down seed
