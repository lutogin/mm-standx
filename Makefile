BIN := bin
GO  ?= go

.DEFAULT_GOAL := help

.PHONY: help
help: ## Показать список целей
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Собрать все бинарники в ./bin
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/mmbot ./cmd/mmbot
	$(GO) build -o $(BIN)/recorder ./cmd/recorder

.PHONY: test
test: ## Юнит- и property-тесты с детектором гонок
	$(GO) test -race ./...

.PHONY: test-int
test-int: ## Интеграционные тесты против fake-биржи
	$(GO) test -race -tags=integration ./test/integration/...

.PHONY: test-live
test-live: ## Дымовой тест с реальными ордерами (нужны STANDX_LIVE=1 и STANDX_WALLET_PK)
	@test "$(STANDX_LIVE)" = "1" || (echo "откажусь без STANDX_LIVE=1: тест отправляет настоящие ордера" && exit 1)
	$(GO) test -v -tags=live ./test/live/...

.PHONY: cover
cover: ## Отчёт о покрытии
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Статический анализ
	$(GO) vet ./...
	@command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck не установлен, пропускаю"

.PHONY: fmt
fmt: ## Форматирование
	$(GO) fmt ./...

.PHONY: tidy
tidy: ## Привести go.mod в порядок
	$(GO) mod tidy

.PHONY: record
record: build ## Собрать рыночные данные для replay-тестов (make record DURATION=1h)
	$(BIN)/recorder -config config.yaml -duration $(or $(DURATION),15m)

.PHONY: run
run: build ## Запустить бота с текущей конфигурацией
	$(BIN)/mmbot -config config.yaml

.PHONY: clean
clean: ## Удалить артефакты сборки
	rm -rf $(BIN) coverage.out
