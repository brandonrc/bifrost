export CGO_ENABLED=0

.PHONY: build test lint cover
build:
	go build ./...
test:
	go test -race ./...
cover:
	go test -race -covermode=atomic -coverprofile=coverage.txt ./...
	go tool cover -func=coverage.txt | tail -1
lint:
	golangci-lint run
