ifneq (,$(wildcard ./.env))
    include .env
    export
endif

.PHONY: run
run:
	@go mod tidy
	@go run .

.PHONY: build-prod
build-prod:
	@CGO_ENABLED=0 go build -o skymail-backend ./main.go

.PHONY: docs
docs:
	@go tool swag fmt
	@go tool swag init --v3.1

.PHONY: sqlc
sqlc:
	sqlc generate

.PHONY: migrate-up
migrate-up:
	@migrate -path db/migrations -database "$(DATABASE_URL)" up

.PHONY: migrate-down
migrate-down:
	@migrate -path db/migrations -database "$(DATABASE_URL)" down

.PHONY: create-migration
create-migration:
	migrate create -dir ./db/migrations -ext sql -tz Europe/Istanbul rename_me

.PHONY: deps
deps:
	go mod tidy
	go install -tags "postgres" github.com/golang-migrate/migrate/v4/cmd/migrate@latest
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
