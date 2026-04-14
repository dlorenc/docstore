.PHONY: build build-ds test lint clean

BINARY := docstore
BUILD_DIR := bin

build:
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/docstore

build-ds:
	go build -o $(BUILD_DIR)/ds ./cmd/ds

test:
	go test ./...

lint:
	go vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; fi

clean:
	rm -rf $(BUILD_DIR)
