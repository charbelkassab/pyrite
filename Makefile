# natural-quant

BINARY  := natural-quant
PKG     := ./cmd/natural-quant
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := build

.PHONY: build
build: ## Build the binary
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

.PHONY: run
run: build ## Build and start the web app
	./$(BINARY) serve --open

.PHONY: dev
dev: build ## Serve with live front-end assets (no rebuild needed for web/)
	./$(BINARY) serve --dev ./web

.PHONY: offline
offline: build ## Serve with synthetic data, no network or API keys
	./$(BINARY) serve --offline

.PHONY: test
test: ## Unit tests — no network, no API keys
	go test ./...

.PHONY: test-race
test-race:
	go test -race ./...

.PHONY: corpus
corpus: ## Live prompt corpus — uses real API calls and costs money
	NQ_LIVE_TESTS=1 go test ./internal/strategy/ -run TestPromptCorpus -v -timeout 60m

.PHONY: check
check: fmt vet test ## Format, vet and test

.PHONY: fmt
fmt:
	gofmt -w ./cmd ./internal ./web

.PHONY: vet
vet:
	go vet ./...

.PHONY: doctor
doctor: build ## Report provider, data and cache status
	./$(BINARY) doctor

.PHONY: clean
clean:
	rm -f $(BINARY)
	rm -rf dist

.PHONY: dist
dist: ## Cross-compile release binaries
	@mkdir -p dist
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		out=dist/$(BINARY)-$$os-$$arch; \
		[ "$$os" = "windows" ] && out=$$out.exe; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build -ldflags "$(LDFLAGS)" -o $$out $(PKG) || exit 1; \
	done
	@ls -lh dist

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
