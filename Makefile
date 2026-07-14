# Convenience targets for building the CLIProxyAPI server and its embedded web UI.
#
# The management web UI (webui/) is a React/Vite app that is compiled to a single
# index.html and embedded into the Go binary at build time via //go:embed. The
# embedded copy lives at internal/managementasset/management.html. The commands
# below mirror the workflow documented in CLAUDE.md so that any edit under
# webui/ can be picked up by the binary with a single command.

BINARY      := cli-proxy-api
PKG_MAIN    := ./cmd/server
WEBUI_DIR   := webui
EMBED_HTML  := internal/managementasset/management.html
# At runtime the server prefers an on-disk static/management.html over the
# bundle embedded in the binary (see internal/api/server.go serveManagementControlPanel).
# Keep that override in sync with the embedded copy so a stale on-disk file never
# shadows freshly built UI changes. Leave STATIC_HTML empty to disable.
STATIC_HTML ?= static/management.html

.PHONY: all build webui webui-install webui-build refresh-embed clean run fmt test

# Full pipeline: rebuild web UI from source, refresh the embedded copy, then
# rebuild the Go binary so the new bundle is baked in.
all: webui-build refresh-embed build

# Build the Go binary from the current (already-embedded) HTML. Fast path when
# only Go code changed.
build:
	go build -o $(BINARY) $(PKG_MAIN)

# Rebuild the web UI from webui/ WITHOUT touching the embedded copy or the Go
# binary. Useful to eyeball the compiled output under webui/dist before baking.
webui: webui-build

webui-install:
	cd $(WEBUI_DIR) && npm install --no-audit --no-fund

# Install npm deps only when webui/node_modules is missing.
$(WEBUI_DIR)/node_modules:
	cd $(WEBUI_DIR) && npm install --no-audit --no-fund

# Always run `npm run build` so edits under webui/src/ are picked up. Vite's own
# cache keeps no-op builds fast, so there's no downside to running it each time.
webui-build: $(WEBUI_DIR)/node_modules
	cd $(WEBUI_DIR) && npm run build

# Copy the freshly built bundle into the embed location so `go build` picks it
# up. Also refresh the on-disk static/management.html override (when present) so
# the runtime override never serves a stale bundle older than the embed.
refresh-embed: webui-build
	cp $(WEBUI_DIR)/dist/index.html $(EMBED_HTML)
	@if [ -n "$(STATIC_HTML)" ] && [ -d '$(dir $(STATIC_HTML))' ]; then \
		cp $(WEBUI_DIR)/dist/index.html $(STATIC_HTML); \
		echo "refreshed runtime override: $(STATIC_HTML)"; \
	fi

# Convenience: rebuild web UI, refresh embed, and rebuild binary.
refresh: refresh-embed build

clean:
	rm -f $(BINARY)
	rm -rf $(WEBUI_DIR)/dist

run: build
	./$(BINARY)

fmt:
	gofmt -w .

test:
	go test ./...
