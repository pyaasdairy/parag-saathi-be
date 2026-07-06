SHELL := /bin/bash

.PHONY: help tidy build run seed test vet smoke docker-build docker-up docker-down

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

tidy: ## Resolve and prune Go module dependencies
	go mod tidy

build: ## Compile server + seed binaries into ./bin
	go build -o bin/saathi-server ./cmd/server
	go build -o bin/saathi-seed ./cmd/seed

run: ## Run the API server (reads .env)
	go run ./cmd/server

seed: ## Seed baseline data (org tree, super admin, rate chart, flags)
	go run ./cmd/seed

test: ## Run all unit tests
	go test ./... -count=1

vet: ## Static analysis
	go vet ./...

smoke: ## End-to-end smoke test against a running server (see scripts/smoke.sh)
	bash scripts/smoke.sh

docker-build: ## Build the API container image
	docker build -t saathi-backend:latest .

docker-up: ## Start MongoDB + API via docker compose
	docker compose up -d --build

docker-down: ## Stop the compose stack
	docker compose down
