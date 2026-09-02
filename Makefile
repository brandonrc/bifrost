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

# L3: the requirement suite against a live cluster (test/requirements/target/cluster).
# TARGET=kind needs BIFROST_URL/BIFROST_ADMIN_PASSWORD/KUBECONFIG for a kind
# cluster deployed from test/requirements/target/cluster/kind (see
# .github/workflows/l3-kind.yml for the full recipe). TARGET=grace ships the
# binaries to grace and runs them there (scripts/l3-grace.sh).
TARGET ?= kind
test-l3:
ifeq ($(TARGET),grace)
	scripts/l3-grace.sh l3-grace.json
else
	CGO_ENABLED=1 REQ_TARGET=$(TARGET) REQ_RUN_ID=$${REQ_RUN_ID:-t$$(printf '%x' $$(date +%s))} \
	  go test -race -count=1 -p 1 -timeout 45m -json ./test/requirements/... > l3-$(TARGET).json; test_status=$$?; \
	go run ./test/requirements/cmd/reqreport -in l3-$(TARGET).json -lane l3 -out l3-report -allow-untested; \
	exit $$test_status
endif
