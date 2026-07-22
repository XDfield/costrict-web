SWAG_VERSION := v1.16.6
SWAG := go run github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION)

.PHONY: run dev swagger-serve swagger swagger-check e2e

run:
	go run ./cmd/api

dev: swagger run

swagger-serve: swagger run

swagger:
	$(SWAG) init -g cmd/api/main.go -o docs --parseDependency --parseInternal

swagger-check:
	$(SWAG) fmt -d ./cmd/api,./internal

# e2e runs the opt-in integration suite (//go:build e2e) against a real local
# Gitea fork. Requires env vars (E2E_GITEA_URL / E2E_GITEA_TOKEN /
# E2E_BOT_TOKEN_KEY / E2E_TENANT_ID) — missing vars cause each test to t.Skip
# with a clear reason. See docs/repo-management/E2E_TESTING.md for the runbook.
e2e:
	go test -tags=e2e -v -timeout 300s ./internal/teamns/
