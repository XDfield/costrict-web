SWAG_VERSION := v1.16.6
SWAG := go run github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION)

.PHONY: run dev swagger-serve swagger swagger-check

run:
	go run ./cmd/api

dev: swagger run

swagger-serve: swagger run

swagger:
	$(SWAG) init -g cmd/api/main.go -o docs --parseDependency --parseInternal

swagger-check:
	$(SWAG) fmt -d ./cmd/api,./internal
