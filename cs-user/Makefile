.PHONY: help test test-race test-coverage vet build build-migrate migrate run clean fmt check swagger swagger-check

SWAG_VERSION := v1.16.6
SWAG := go run github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION)

## help: show available targets (default)
help:
	@awk 'BEGIN {FS = ":.*##"; printf "cs-user Makefile targets:\n\n"} \
	  /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

## test: run unit tests
test:
	go test ./...

## test-race: run unit tests with race detector
test-race:
	go test -race ./...

## test-coverage: run unit tests with coverage report (coverage.out)
test-coverage:
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	@echo "Coverage report: coverage.out"
	@go tool cover -func=coverage.out | tail -5

## vet: run go vet
vet:
	go vet ./...

## fmt: format Go source
fmt:
	gofmt -s -w .

## build: build the cs-user API binary into ./bin/
build:
	mkdir -p bin
	go build -trimpath -ldflags="-s -w" -o bin/cs-user ./cmd/api

## build-migrate: build the standalone migrate binary into ./bin/
build-migrate:
	mkdir -p bin
	go build -trimpath -ldflags="-s -w" -o bin/cs-user-migrate ./cmd/migrate

## migrate: run pending migrations against the configured DB (alias for bin/cs-user-migrate up)
migrate: build-migrate
	./bin/cs-user-migrate up

## run: run cs-user locally (requires env vars — see README)
run:
	go run ./cmd/api

## clean: remove build artifacts
clean:
	rm -rf bin coverage.out

## check: run fmt + vet + test-race (the local pre-push gate)
check: fmt vet test-race
	@echo "✓ fmt + vet + test-race all passed"

## swagger: regenerate OpenAPI spec from annotations (run after editing handler docs)
swagger:
	$(SWAG) init -g cmd/api/main.go -o docs --parseDependency --parseInternal

## swagger-check: format swagger annotations in source (no spec regen)
swagger-check:
	$(SWAG) fmt -d ./cmd/api,./internal
