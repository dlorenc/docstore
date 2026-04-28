.PHONY: build build-ds test lint clean db-reset

BINARY := docstore
BUILD_DIR := bin
DEFAULT_REMOTE ?= https://docstore.dev

build:
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/docstore

build-ds:
	go build -ldflags "-X main.defaultRemote=$(DEFAULT_REMOTE)" -o $(BUILD_DIR)/ds ./cmd/ds

test:
	go test ./...

lint:
	go vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; fi

clean:
	rm -rf $(BUILD_DIR)

# db-reset drops and recreates the public schema, destroying all data.
# Migrations will re-run automatically on the next server start.
# Requires DATABASE_URL to be set.
db-reset:
	psql "$(DATABASE_URL)" -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public; GRANT ALL ON SCHEMA public TO PUBLIC;"
