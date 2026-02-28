RESOURCE_GROUP ?= aztunnel-e2e
LOCATION       ?= westus2
ENTRA_APP      ?= aztunnel-e2e-ci
GITHUB_ENV     ?= e2e-azure

export RESOURCE_GROUP LOCATION ENTRA_APP GITHUB_ENV

.PHONY: setup ci clean help

.DEFAULT_GOAL := help

setup: ## Deploy Azure infra + grant yourself access (SKIP_RBAC=1 to skip)
	./create-relay.sh
	./create-relay-sas-auth-rules.sh
ifneq ($(SKIP_RBAC),1)
	./grant-relay-access.sh --self
endif

ci: ## Full CI setup: infra + identity + GitHub secrets (maintainer)
	./create-relay.sh
	./create-relay-sas-auth-rules.sh
	./create-entra-oidc-app.sh
	./grant-relay-access.sh --self
	./grant-relay-access.sh --sp $(ENTRA_APP)
	./create-github-ci-secrets.sh --dependabot

clean: ## Delete e2e Azure resource group
	az group delete -n $(RESOURCE_GROUP) --yes

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
