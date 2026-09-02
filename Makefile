export CGO_ENABLED=0

.PHONY: build test lint cover cover-tiers ratchet
build:
	go build ./...
test:
	CGO_ENABLED=1 go test -race ./...
cover:
	CGO_ENABLED=1 go test -race -covermode=atomic -coverprofile=coverage.txt ./...
	go tool cover -func=coverage.txt | tail -1
cover-tiers: cover
	go run ./test/requirements/cmd/covreport -profile coverage.txt
ratchet: cover
	go run ./test/requirements/cmd/covreport -profile coverage.txt -update
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run
