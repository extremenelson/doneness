BINARY  := doneness
# bin/ holds the committed helper scripts, so build output goes to build/.
OUT     := build
SCRIPTS := $(wildcard bin/*)
SCRIPT_DIR ?= $(HOME)/bin

.DEFAULT_GOAL := help
.PHONY: help build run test cover lint fmt tidy fix check install install-scripts clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Compile to ./build
	go build -o $(OUT)/$(BINARY) .

run: ## Run without building a binary (make run ARGS="-flat demo")
	go run . $(ARGS)

test: ## Run tests with the race detector
	go test -race ./...

cover: ## Run tests and open an HTML coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

lint: ## Run golangci-lint (includes go vet and staticcheck)
	golangci-lint run

fmt: ## Format code and fix imports (rewrites files)
	golangci-lint fmt

tidy: ## Sync go.mod and go.sum with the imports in the code (rewrites files)
	go mod tidy

fix: fmt tidy ## Rewrite formatting and go.mod/go.sum; run when check complains

# check only verifies, so doneness's own done-gate (.doneness.json) can run it
# and trust that a green tree is the tree that was tested.
check: ## Everything that must pass before a commit (read-only)
	golangci-lint fmt --diff
	go mod tidy -diff
	golangci-lint run
	go test -race ./...

install: ## Install doneness into $(go env GOBIN) or ~/go/bin
	go install .

install-scripts: ## Copy the helper scripts in bin/ to $(SCRIPT_DIR) (default ~/bin)
	mkdir -p $(SCRIPT_DIR)
	install -m 0755 $(SCRIPTS) $(SCRIPT_DIR)/

clean: ## Remove build and coverage output
	rm -rf $(OUT) coverage.out
