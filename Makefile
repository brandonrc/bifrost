export CGO_ENABLED=0

.PHONY: build test lint cover cover-tiers ratchet test-l2 report
build:
	go build ./...
test:
	CGO_ENABLED=1 go test -race ./...
# L2: the requirement suite against the in-process target.
test-l2:
	CGO_ENABLED=1 REQ_TARGET=inproc go test -race -count=1 -json ./test/requirements/... > l2.json; test_status=$$?; \
	go run ./test/requirements/cmd/reqreport -in l2.json -lane l2 -out docs/requirements -allow-untested; report_status=$$?; \
	if [ $$test_status -ne 0 ]; then exit $$test_status; fi; \
	exit $$report_status
report: test-l2
cover:
	CGO_ENABLED=1 go test -race -covermode=atomic -coverprofile=coverage.txt ./...
	go tool cover -func=coverage.txt | tail -1
cover-tiers: cover
	go run ./test/requirements/cmd/covreport -profile coverage.txt
ratchet: cover
	go run ./test/requirements/cmd/covreport -profile coverage.txt -update
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run
