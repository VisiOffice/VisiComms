.PHONY: build test vet dev migrate

build:
	go build -o bin/visicomms ./cmd/visicomms

test:
	go test ./...

vet:
	gofmt -l . | (! grep .) && go vet ./...

# Uses local Postgres: visicomms_development (created if missing).
migrate: build
	@psql -d postgres -Atc "select 1 from pg_database where datname='visicomms_development'" | grep -q 1 || createdb visicomms_development
	./bin/visicomms migrate

dev: migrate
	./bin/visicomms serve
