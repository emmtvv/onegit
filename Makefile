VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
IMAGE   ?= emmtvv/onegit

GOLANGCI_LINT_VERSION ?= v2.14.0

# Backing services of the integration tests (docker-compose.test.yml).
TEST_ENV := ONEGIT_TEST_POSTGRES='postgres://onegit:onegit@127.0.0.1:55432/postgres?sslmode=disable' \
	ONEGIT_TEST_REDIS=redis://127.0.0.1:56379/0 \
	ONEGIT_TEST_S3=http://127.0.0.1:58333 \
	ONEGIT_TEST_S3_ACCESS_KEY=onegit \
	ONEGIT_TEST_S3_SECRET_KEY=onegit-test-secret \
	ONEGIT_TEST_REQUIRE=1

.PHONY: build test test-deps test-deps-down test-integration cover fmt vet lint check docker up down

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/onegit ./cmd/onegit

# Unit tests only: tests that need Postgres, Redis and S3 are skipped.
test:
	go test ./...

test-deps:
	docker compose -f docker-compose.test.yml up -d --wait

test-deps-down:
	docker compose -f docker-compose.test.yml down -v

# Everything, including the end-to-end server tests.
test-integration: test-deps
	$(TEST_ENV) go test -race -count=1 ./...

cover: test-deps
	$(TEST_ENV) go test -count=1 -coverpkg=./... -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	@echo "HTML report: go tool cover -html=coverage.out"

fmt:
	gofmt -w .

vet:
	go vet ./...

# Uses a local golangci-lint if it is installed, Docker otherwise.
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; \
	else docker run --rm -v "$(CURDIR)":/app -w /app golangci/golangci-lint:$(GOLANGCI_LINT_VERSION) golangci-lint run ./...; fi

# What CI runs.
check: vet lint test-integration

docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(VERSION) .

# The full stack from docker-compose.yml, built from source.
up:
	@test -f .env || { cp .env.example .env; echo "created .env from .env.example: set the passwords, then run make up again"; exit 1; }
	docker compose up -d --build

down:
	docker compose down
