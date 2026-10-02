.PHONY: run build test lint generate db-up up down tf-plan tf-apply

run:
	go run ./cmd/spotter

build:
	go build -o bin/spotter ./cmd/spotter

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

tf-plan:
	cd infra/terraform && terraform init -input=false && terraform plan

tf-apply:
	cd infra/terraform && terraform apply
