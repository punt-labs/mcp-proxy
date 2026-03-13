.PHONY: help lint docs test test-e2e check format build clean dist cover

GOBIN := $(shell go env GOPATH)/bin

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-12s %s\n", $$1, $$2}'

lint: ## Lint (go vet + staticcheck)
	go vet ./...
	$(GOBIN)/staticcheck ./...

docs: ## Lint markdown
	npx --yes markdownlint-cli2 "**/*.md" "#node_modules"

test: ## Run tests with race detection
	go test -race -count=1 ./...

test-e2e: build ## Run E2E tests (requires built binary)
	go test -race -count=1 -tags=e2e ./internal/e2e/...

check: lint docs test ## Run all quality gates

format: ## Format code
	gofmt -w .

build: ## Build binary
	go build -o mcp-proxy .

clean: ## Remove build artifacts
	rm -f mcp-proxy coverage.out
	rm -rf dist/

dist: clean ## Cross-compile for all platforms
	mkdir -p dist
	GOOS=darwin  GOARCH=arm64 go build -o dist/mcp-proxy-darwin-arm64 .
	GOOS=darwin  GOARCH=amd64 go build -o dist/mcp-proxy-darwin-amd64 .
	GOOS=linux   GOARCH=arm64 go build -o dist/mcp-proxy-linux-arm64  .
	GOOS=linux   GOARCH=amd64 go build -o dist/mcp-proxy-linux-amd64  .

cover: ## Test with coverage report
	go test -cover -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
