BIN   := bin
IMAGE ?= lutogin/mm-standx:latest
GO  ?= go

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build all binaries into ./bin
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/mmbot ./cmd/mmbot
	$(GO) build -o $(BIN)/recorder ./cmd/recorder
	$(GO) build -o $(BIN)/standxctl ./cmd/standxctl
	$(GO) build -o $(BIN)/analyze ./cmd/analyze
	$(GO) build -o $(BIN)/guardian ./cmd/guardian

.PHONY: check-account
check-account: build ## Verify access to the StandX account
	$(BIN)/standxctl auth
	@echo
	$(BIN)/standxctl balance

.PHONY: test
test: ## Unit and property tests with the race detector
	$(GO) test -race ./...

.PHONY: test-int
test-int: ## Integration tests against the fake exchange
	$(GO) test -race -tags=integration ./test/integration/...

.PHONY: test-live
test-live: ## Smoke test with real orders (requires STANDX_LIVE=1 and STANDX_WALLET_PK)
	@test "$(STANDX_LIVE)" = "1" || (echo "refusing without STANDX_LIVE=1: this test sends real orders" && exit 1)
	$(GO) test -v -tags=live ./test/live/...

.PHONY: cover
cover: ## Coverage report
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Static analysis
	$(GO) vet ./...
	@command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck is not installed, skipping"

.PHONY: fmt
fmt: ## Format sources
	$(GO) fmt ./...

.PHONY: tidy
tidy: ## Tidy go.mod
	$(GO) mod tidy

.PHONY: record
record: build ## Collect market data for replay tests (make record DURATION=1h)
	$(BIN)/recorder -config config.yaml -duration $(or $(DURATION),15m)

.PHONY: run
run: build ## Run the bot with the current configuration
	$(BIN)/mmbot -config config.yaml

.PHONY: image
image: ## Собрать образ (конфигурация запекается внутрь)
	docker build -t $(IMAGE) .

.PHONY: push
push: image ## Опубликовать образ в реестр
	docker push $(IMAGE)

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN) coverage.out
