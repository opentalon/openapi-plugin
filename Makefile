BINARY := openapi-plugin

.PHONY: build test tidy lint

build: tidy
	go build -o $(BINARY) .

test:
	go test ./...

tidy:
	go mod tidy

lint:
	go vet ./...
