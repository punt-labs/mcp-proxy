.PHONY: vet test test-race test-e2e test-cover build clean

vet:
	go vet ./...

test: vet
	go test -race -count=1 ./...

test-e2e: build
	go test -race -count=1 -tags=e2e ./internal/e2e/...

test-cover:
	go test -cover -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

build:
	go build -o mcp-proxy .

clean:
	rm -f mcp-proxy coverage.out
	rm -rf dist/

dist: clean
	mkdir -p dist
	GOOS=darwin  GOARCH=arm64 go build -o dist/mcp-proxy-darwin-arm64 .
	GOOS=darwin  GOARCH=amd64 go build -o dist/mcp-proxy-darwin-amd64 .
	GOOS=linux   GOARCH=arm64 go build -o dist/mcp-proxy-linux-arm64  .
	GOOS=linux   GOARCH=amd64 go build -o dist/mcp-proxy-linux-amd64  .
