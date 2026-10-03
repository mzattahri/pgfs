POSTGRES_VERSION ?= 14
COVERALLS_TOKEN ?= $(shell cat COVERALLS_REPO_TOKEN)

POSTGRES_DB := pgfs_test
POSTGRES_USER := pgfs
POSTGRES_PASSWORD := password
POSTGRES_PORT ?= 5432
POSTGRES_URL := postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)
CONTAINER_IMAGE := docker.io/library/postgres:$(POSTGRES_VERSION)-alpine
CONTAINER_RUNTIME ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)

export

.PHONY: all test db coverage vet

all: test

test:
	@./testing/setup.sh go test ./...

db:
	$(CONTAINER_RUNTIME) run --env POSTGRES_DB=$(POSTGRES_DB) --env POSTGRES_USER=$(POSTGRES_USER) --env POSTGRES_PASSWORD=$(POSTGRES_PASSWORD) --publish $(POSTGRES_PORT):5432 $(CONTAINER_IMAGE)

coverage:
	@./testing/setup.sh goveralls

vet: coverage
	go vet ./...
	gosec --quiet ./...
	govulncheck ./...
