POSTGRES_VERSION?=14
POSTGRES_DB=pgfs_test
POSTGRES_USER=pgfs
POSTGRES_PASSWORD=password
POSTGRES_PORT=5432
POSTGRES_URL=postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)

COVERALLS_TOKEN?=$(shell cat COVERALLS_REPO_TOKEN)

export

.PHONY: all test db coverage vet

all: test

test:
	@./testing/setup.sh go test ./...

db: DOCKER_IMAGE=postgres:$(POSTGRES_VERSION)-alpine
	docker run --env POSTGRES_DB=$(POSTGRES_DB) --env POSTGRES_USER=$(POSTGRES_USER) --env POSTGRES_PASSWORD=$(POSTGRES_PASSWORD) --publish 5432:$(POSTGRES_PORT) $(DOCKER_IMAGE)

coverage:
	@./testing/setup.sh goveralls

vet: coverage
	go vet ./...
	gosec --quiet ./...
	govulncheck ./...
