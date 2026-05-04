.PHONY: build build-ds test lint clean db-reset

BINARY := docstore
BUILD_DIR := bin
DEFAULT_REMOTE ?= https://docstore.dev
# OAuth client ID for docstore.dev (public — Desktop app clients are distributed openly).
OAUTH_CLIENT_ID ?= 320310132788-md94jtp0d9fdcdlq2quma4mcpr0ul1qr.apps.googleusercontent.com
# OAuth client secret — do NOT commit. Set via env var or GitHub Actions secret.
# Local builds: make build-ds OAUTH_CLIENT_SECRET=<secret>
# CI builds: injected from the OAUTH_CLIENT_SECRET Actions secret.
OAUTH_CLIENT_SECRET ?=

build:
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/docstore

build-ds:
	go build -ldflags "-X main.defaultRemote=$(DEFAULT_REMOTE) -X main.defaultOAuthClientID=$(OAUTH_CLIENT_ID) -X main.defaultOAuthClientSecret=$(OAUTH_CLIENT_SECRET)" -o $(BUILD_DIR)/ds ./cmd/ds

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
