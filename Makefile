.PHONY: build run test demo load clean
build:
	go build -trimpath -o bin/huanjing ./cmd/server
run:
	go run ./cmd/server
test:
	go test -race ./...
demo:
	python3 scripts/demo.py
load:
	python3 scripts/loadtest.py --help
clean:
	rm -rf bin

.PHONY: test-e2e
test-e2e: build
	E2E_BINARY=$(CURDIR)/bin/huanjing python3 scripts/e2e_contract.py
	E2E_BINARY=$(CURDIR)/bin/huanjing python3 scripts/e2e_lifecycle.py

.PHONY: test-ai-protocol test-ai-live
test-ai-protocol: build
	E2E_BINARY=$(CURDIR)/bin/huanjing python3 scripts/e2e_ai_protocol.py
# Explicit opt-in: uses configured credentials and incurs real provider usage.
test-ai-live: build
	E2E_BINARY=$(CURDIR)/bin/huanjing python3 scripts/e2e_semantics_live.py
