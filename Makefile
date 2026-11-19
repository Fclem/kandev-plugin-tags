.PHONY: build run test fmt vet package package-host clean setup check-sdk

BIN := bin/kandev-plugin-tags
VERSION := 0.20.0
STAGE := .build/stage
PKG_OUT := kandev-plugin-tags-$(VERSION).tar.gz

# The SDK is a standalone module; the package tool remains in apps/backend.
# KANDEV_ROOT selects one matching host checkout for both.
GO_MOD_KANDEV_SDK := ../kandev/pluginsdk
SETUP_KANDEV_REPO := ./.build/kandev
KANDEV_ROOT ?= $(if $(wildcard $(GO_MOD_KANDEV_SDK)/go.mod),../kandev,$(SETUP_KANDEV_REPO))
KANDEV_SDK ?= $(KANDEV_ROOT)/pluginsdk
KANDEV_BACKEND := $(abspath $(KANDEV_ROOT)/apps/backend)
# Development integration pin; canonical main does not yet provide sidebar grouping.
KANDEV_REPOSITORY ?= https://github.com/Fclem/kandev.git
KANDEV_REVISION ?= 1154f0ff1771aef71091b8f7457ada905e97af3b

check-sdk:
	@test -f "$(KANDEV_SDK)/go.mod" || { \
		echo "ERROR: Kandev standalone SDK not found at $(KANDEV_SDK)."; \
		echo "Run 'make setup', or set KANDEV_ROOT=/path/to/kandev."; \
		exit 1; \
	}

.PHONY: check-packager
check-packager:
	@test -d "$(KANDEV_BACKEND)/cmd/plugin-pack" || { \
		echo "ERROR: Kandev package tool not found at $(KANDEV_BACKEND)."; \
		echo "Run 'make setup', or set KANDEV_ROOT=/path/to/kandev."; \
		exit 1; \
	}

setup:
	@set -e; ROOT="$(KANDEV_ROOT)"; \
	if [ -f "$$ROOT/pluginsdk/go.mod" ] && [ -d "$$ROOT/apps/backend/cmd/plugin-pack" ]; then \
		echo "Kandev SDK and package tool already exist at $$ROOT."; \
	elif [ -e "$$ROOT" ]; then \
		echo "ERROR: existing checkout $$ROOT is incomplete; select a compatible KANDEV_ROOT."; exit 1; \
	else \
		mkdir -p "$$(dirname "$$ROOT")"; \
		STAGING="$$(mktemp -d "$$ROOT.setup.XXXXXX")"; \
		trap 'rm -rf "$$STAGING"' EXIT; \
		git -C "$$STAGING" init; \
		git -C "$$STAGING" remote add origin "$(KANDEV_REPOSITORY)"; \
		git -C "$$STAGING" sparse-checkout init --cone; \
		git -C "$$STAGING" sparse-checkout set pluginsdk apps/backend; \
		git -C "$$STAGING" fetch --depth=1 --filter=blob:none origin "$(KANDEV_REVISION)"; \
		git -C "$$STAGING" checkout --detach FETCH_HEAD; \
		mv "$$STAGING" "$$ROOT"; \
		trap - EXIT; \
	fi; \
	test -f "$$ROOT/pluginsdk/go.mod" && test -d "$$ROOT/apps/backend/cmd/plugin-pack"; \
	$(MAKE) --no-print-directory check-compatible-sdk KANDEV_ROOT="$$ROOT"

# Alternate module files keep path overrides and dependency writes out of source.
define with-sdk-override
@set -e; KSDK="$(abspath $(KANDEV_SDK))"; tmp="$$(mktemp -d)"; \
trap 'rm -rf "$$tmp"' EXIT; \
cp go.mod "$$tmp/go.mod"; \
if [ -f go.sum ]; then cp go.sum "$$tmp/go.sum"; fi; \
go mod edit -modfile="$$tmp/go.mod" -replace github.com/kdlbs/kandev/pluginsdk="$$KSDK"; \
plugin_go() { subcommand=$$1; shift; go "$$subcommand" -modfile="$$tmp/go.mod" "$$@"; }; \
( $(1) )
endef

.PHONY: check-compatible-sdk
check-compatible-sdk: check-sdk
	$(call with-sdk-override,plugin_go test -run '^$$' ./server/...)

build: check-sdk
	$(call with-sdk-override,mkdir -p bin && plugin_go build -buildvcs=false -o $(BIN) ./server/...)

run: build
	./$(BIN)

test: check-sdk
	$(call with-sdk-override,plugin_go test ./server/...)
	node --test ui/bundle.test.js
	node --test setup.test.cjs

fmt:
	gofmt -l ./server

vet: check-sdk
	$(call with-sdk-override,plugin_go vet ./server/...)

package: check-sdk check-packager
	rm -rf $(STAGE)
	mkdir -p $(STAGE)/server $(STAGE)/ui
	cp manifest.yaml $(STAGE)/manifest.yaml
	cp README.md $(STAGE)/README.md
	cp ui/bundle.js $(STAGE)/ui/bundle.js
	$(call with-sdk-override,\
		GOOS=linux   GOARCH=amd64 plugin_go build -buildvcs=false -o $(STAGE)/server/plugin-linux-amd64       ./server && \
		GOOS=linux   GOARCH=arm64 plugin_go build -buildvcs=false -o $(STAGE)/server/plugin-linux-arm64       ./server && \
		GOOS=darwin  GOARCH=amd64 plugin_go build -buildvcs=false -o $(STAGE)/server/plugin-darwin-amd64      ./server && \
		GOOS=darwin  GOARCH=arm64 plugin_go build -buildvcs=false -o $(STAGE)/server/plugin-darwin-arm64      ./server && \
		GOOS=windows GOARCH=amd64 plugin_go build -buildvcs=false -o $(STAGE)/server/plugin-windows-amd64.exe ./server && \
		PLUGIN_ROOT="$$(pwd)" && \
		(cd "$(KANDEV_BACKEND)" && go run ./cmd/plugin-pack -dir "$$PLUGIN_ROOT/$(STAGE)" -out "$$PLUGIN_ROOT/$(PKG_OUT)"))
	rm -rf $(STAGE)
	@echo "Wrote $(PKG_OUT)"

package-host: check-sdk check-packager
	rm -rf $(STAGE)
	mkdir -p $(STAGE)/server $(STAGE)/ui
	cp manifest.yaml $(STAGE)/manifest.yaml
	cp README.md $(STAGE)/README.md
	cp ui/bundle.js $(STAGE)/ui/bundle.js
	$(call with-sdk-override,\
		plugin_go build -buildvcs=false -o $(STAGE)/server/plugin-$$(go env GOOS)-$$(go env GOARCH)$$(go env GOEXE) ./server && \
		PLUGIN_ROOT="$$(pwd)" && \
		(cd "$(KANDEV_BACKEND)" && go run ./cmd/plugin-pack -dir "$$PLUGIN_ROOT/$(STAGE)" -out "$$PLUGIN_ROOT/$(PKG_OUT)" -platform-only))
	rm -rf $(STAGE)
	@echo "Wrote $(PKG_OUT)"

clean:
	rm -rf bin $(STAGE) kandev-plugin-tags-*.tar.gz
