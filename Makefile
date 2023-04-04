.PHONY: help test deps clean

# Ref: https://gist.github.com/prwhite/8168133
help:  ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

GOLANGCI_LINT_VERSION ?= "v1.52.2"

local-test: SHELL:=/bin/bash
local-test:  ## Run test cases. (Args: GOLANGCI_LINT_VERSION=latest)
	GOLANGCI_LINT_CMD=golangci-lint; \
	_VERSION=$(GOLANGCI_LINT_VERSION); _VERSION=$${_VERSION#v}; \
	if [[ ! -x $$(command -v golangci-lint) ]] || [[ "$${_VERSION}" != "latest" && $$(golangci-lint version 2>&1) != *"$${_VERSION}"* ]]; then \
		if [[ ! -e ./bin/golangci-lint ]]; then \
			curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s $(GOLANGCI_LINT_VERSION) || exit 1; \
		fi; \
		GOLANGCI_LINT_CMD=./bin/golangci-lint; \
	fi; \
	$${GOLANGCI_LINT_CMD} run ./...
	pushd /tmp && go install github.com/rakyll/gotest@latest && popd
	gotest -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out  # -o coverage.html


deps:  ## Update vendor.
	go get -v ./...
	go mod tidy -v
	go mod verify


clean:  ## Clean up useless files.
	find . -type f -name '*.out' -exec rm -f {} +
	find . -type f -name '.DS_Store' -exec rm -f {} +
	find . -type f -name '*.test' -exec rm -f {} +
	find . -type f -name '*.prof' -exec rm -f {} +
