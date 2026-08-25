# OpenTalon clones this repo and runs `make build BINARY_NAME=<plugin-key>`,
# expecting the binary at ./<BINARY_NAME>. Honor it so the config map key
# (any name) resolves to the built binary. Local `make build` defaults to
# openapi-plugin.
BINARY_NAME ?= openapi-plugin

.PHONY: build test tidy lint

build: tidy
	go build -o $(BINARY_NAME) .

test:
	go test ./...

tidy:
	go mod tidy

lint:
	go vet ./...
