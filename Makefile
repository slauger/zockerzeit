BINARY  := zockerzeit
IMAGE   := ghcr.io/slauger/zockerzeit
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: help build run demo test cover lint vet vulncheck fmt tidy image helm-lint helm-template helm-unittest helm-docs ci clean

help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

build: ## Build the binary.
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/$(BINARY)

run: build ## Run with config.yaml.
	./bin/$(BINARY) -config config.yaml

demo: build ## Run with a simulated UniFi controller and sample data.
	./bin/$(BINARY) -demo

test: ## Run tests with race detector.
	go test -race ./...

cover: ## Run tests and enforce 80% coverage.
	go test -race -coverprofile=cover.out ./...
	./hack/check-coverage.sh cover.out 80

lint: ## Run golangci-lint.
	golangci-lint run

vet: ## Run go vet.
	go vet ./...

vulncheck: ## Scan for known vulnerabilities.
	go tool govulncheck ./...

fmt: ## Format code.
	gofmt -s -w .

tidy: ## Tidy go.mod and fail on changes.
	go mod tidy
	git diff --exit-code go.mod go.sum

image: ## Build the container image.
	docker build -f images/$(BINARY)/Containerfile --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

helm-lint: ## Lint the Helm chart.
	helm lint charts/$(BINARY)

helm-template: ## Render the Helm chart.
	helm template test charts/$(BINARY) --set config.unifi.url=https://unifi:8443 > /dev/null

helm-unittest: ## Run Helm unit tests.
	helm unittest charts/$(BINARY)

helm-docs: ## Generate the chart README.
	helm-docs --chart-search-root charts

ci: tidy vet lint vulncheck cover helm-lint helm-template helm-unittest ## Run all checks.

clean: ## Remove build artifacts.
	rm -rf bin/ cover.out
