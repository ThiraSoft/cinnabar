POSTGRES_TEST_DSN ?= postgres://cinnabar:cinnabar@localhost:5433/cinnabar?sslmode=disable
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  = -s -w \
  -X github.com/ThiraSoft/cinnabar/internal/version.Version=$(VERSION) \
  -X github.com/ThiraSoft/cinnabar/internal/version.Commit=$(COMMIT) \
  -X github.com/ThiraSoft/cinnabar/internal/version.BuildDate=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/cinnabar ./cmd/cinnabar

test:
	go test ./...

test-integration:
	POSTGRES_TEST_DSN="$(POSTGRES_TEST_DSN)" go test ./... -count=1

up:
	docker compose up -d --wait

down:
	docker compose down

lint:
	go vet ./...
	gofmt -l .

eval:
	POSTGRES_DSN="$(POSTGRES_TEST_DSN)" go run ./eval -config config.yaml

.PHONY: build test test-integration up down lint eval
