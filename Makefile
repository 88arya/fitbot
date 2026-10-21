.PHONY: run build test lint generate db-up up down tunnel tf-plan tf-apply

run:
	go run ./cmd/fitbot

build:
	go build -o bin/fitbot ./cmd/fitbot

test:
	go test ./...

lint:
	gofmt -l .
	go vet ./...

generate:
	docker run --rm -v "$(CURDIR):/src" -w /src sqlc/sqlc:1.29.0 generate

db-up:
	docker compose up -d db

up:
	docker compose up -d --build

down:
	docker compose down

tunnel:
	./scripts/tunnel.sh

tf-plan:
	cd infra/terraform && terraform init -input=false && terraform plan

tf-apply:
	cd infra/terraform && terraform apply
