# terox — interactive multi-shard PostgreSQL client + migration runner.
#
# Quick start:
#   make            # clean reinstall: wipe stale copies, rebuild from source,
#                   # install one fresh `terox` (version from main.go), verify
#   terox           # ...then just run it
#
# Other targets: make build | install | install-go | uninstall | verify | test | race | help

BINARY  := terox
PKG     := .
GO      ?= go

# Install dir. By default terox auto-picks a directory ALREADY ON YOUR PATH and
# writable (so `terox` runs right after install — no sudo, no PATH editing). The
# preference order is ~/bin, ~/.local/bin, /opt/homebrew/bin, /usr/local/bin; if
# none qualifies it falls back to ~/.local/bin (and install warns to add it).
# Override explicitly: `make install BINDIR=/usr/local/bin` or `make install PREFIX=/usr/local`.
PREFIX  ?=
BINDIR  ?= $(if $(strip $(PREFIX)),$(PREFIX)/bin,$(shell sh -c 'for d in "$$HOME/bin" "$$HOME/.local/bin" /opt/homebrew/bin /usr/local/bin; do printf ":%s:" "$$PATH" | grep -qF ":$$d:" || continue; if [ -w "$$d" ]; then echo "$$d"; exit 0; fi; if [ ! -e "$$d" ] && [ -w "$${d%/*}" ]; then echo "$$d"; exit 0; fi; done; echo "$$HOME/.local/bin"'))

# Strip symbols (-s -w) for a smaller binary. The version string is the literal
# `var version` in main.go — we deliberately do NOT override it via -ldflags -X,
# so `terox version` always reports exactly what is compiled into main.go.
LDFLAGS ?= -s -w
GOFLAGS ?= -trimpath

.DEFAULT_GOAL := all

# all (default `make`): one-shot clean reinstall. Wipes the local build artifact
# and every installed copy this Makefile knows about, rebuilds from source,
# installs ONE fresh binary into a writable dir already on your PATH (so `terox`
# runs immediately, no sudo) plus the config next to it, then verifies the active
# `terox` reports the version compiled into main.go. Run plain `make` after bumping
# `var version` in main.go.
.PHONY: all
all:
	@$(MAKE) --no-print-directory uninstall
	@$(MAKE) --no-print-directory install
	@$(MAKE) --no-print-directory verify
	@$(MAKE) --no-print-directory clean

# build: compile the binary into the project directory.
.PHONY: build
build:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)
	@echo "✓ built ./$(BINARY) ($$(./$(BINARY) version 2>/dev/null | awk '{print $$NF}'))"

# install: build, then copy the binary into $(BINDIR) (auto-picked to be on your
# PATH; sudo only if not writable). Also deploys the project's ./config.yaml to the
# standard XDG location ~/.config/terox/config.yaml (mode 0600) — the project file
# is the source of truth, so each install refreshes it. If ./config.yaml is absent,
# the config is left untouched.
.PHONY: install
install: build
	@if [ -w "$(BINDIR)" ] || { [ ! -e "$(BINDIR)" ] && [ -w "$(dir $(BINDIR))" ]; }; then \
		install -d "$(BINDIR)" && install -m 0755 $(BINARY) "$(BINDIR)/$(BINARY)"; \
	else \
		echo "→ $(BINDIR) is not writable; installing with sudo"; \
		sudo install -d "$(BINDIR)" && sudo install -m 0755 $(BINARY) "$(BINDIR)/$(BINARY)"; \
	fi
	@echo "✓ installed $(BINARY) → $(BINDIR)/$(BINARY)"
	@cfgdir="$${XDG_CONFIG_HOME:-$$HOME/.config}/terox"; \
	if [ -f config.yaml ]; then \
		mkdir -p "$$cfgdir" && chmod 700 "$$cfgdir" && install -m 0600 config.yaml "$$cfgdir/config.yaml"; \
		echo "✓ config: ./config.yaml → $$cfgdir/config.yaml (0600)"; \
	else \
		echo "· no ./config.yaml in project — config not installed (create one, e.g. from config.example.yaml)"; \
	fi
	@resolved=$$(command -v $(BINARY) 2>/dev/null); \
	if [ -z "$$resolved" ]; then \
		echo "⚠ $(BINDIR) is not on your PATH — add it, e.g.:"; \
		echo "    echo 'export PATH=\"$(BINDIR):\$$PATH\"' >> ~/.zshrc && source ~/.zshrc"; \
	elif [ "$$resolved" != "$(BINDIR)/$(BINARY)" ]; then \
		echo "⚠ another '$(BINARY)' EARLIER on your PATH will run instead of the one just installed:"; \
		echo "    $$resolved  (shadows $(BINDIR)/$(BINARY))"; \
		echo "  run 'make uninstall' to clear stale copies, then re-run 'make install'."; \
	else \
		echo "✓ '$(BINARY)' is on your PATH — run: $(BINARY)"; \
	fi

# install-go: alternative sudo-free install into the Go bin dir (`go env GOPATH`/bin,
# usually ~/go/bin). That dir must be on your PATH.
.PHONY: install-go
install-go:
	$(GO) install $(GOFLAGS) -ldflags '$(LDFLAGS)' $(PKG)
	@gobin=$$($(GO) env GOBIN); [ -n "$$gobin" ] || gobin=$$($(GO) env GOPATH)/bin; \
		echo "✓ go install → $$gobin/$(BINARY)"; \
		case ":$$PATH:" in *":$$gobin:"*) : ;; *) echo "⚠ $$gobin is not on your PATH — add it to use \`$(BINARY)\` directly";; esac

# verify: confirm there is exactly ONE terox on PATH and that it reports the
# version literal from main.go (the source of truth now that we don't stamp -X).
.PHONY: verify
verify:
	@want=$$(sed -n 's/^var version = "\(.*\)".*/\1/p' main.go); \
	bin=$$(command -v $(BINARY) 2>/dev/null); \
	if [ -z "$$bin" ]; then echo "✗ $(BINARY) is not on your PATH"; exit 1; fi; \
	got=$$("$$bin" version 2>/dev/null | awk '{print $$NF}'); \
	copies=$$(which -a $(BINARY) 2>/dev/null | sort -u); \
	n=$$(printf '%s\n' "$$copies" | grep -c .); \
	echo "active:  $$bin"; \
	echo "main.go: $$want    reported: $$got"; \
	if [ "$$want" != "$$got" ]; then echo "✗ version mismatch (stale binary?)"; exit 1; fi; \
	if [ "$$n" != "1" ]; then echo "⚠ $$n copies of $(BINARY) on PATH:"; printf '%s\n' "$$copies" | sed 's/^/    /'; exit 1; fi; \
	echo "✓ single copy, version $$got — matches main.go"

# uninstall: remove EVERY terox binary it can find — $(BINDIR), the Go bin dir,
# the auto-detect candidate dirs, AND every directory on your PATH — so no stale
# or shadowing copy is ever left behind. Uses sudo only for non-writable dirs.
.PHONY: uninstall
uninstall:
	@gobin=$$($(GO) env GOBIN); [ -n "$$gobin" ] || gobin=$$($(GO) env GOPATH)/bin; \
	cands="$(BINDIR) $$gobin $$HOME/bin $$HOME/.local/bin /opt/homebrew/bin /usr/local/bin"; \
	oldifs=$$IFS; IFS=:; for d in $$PATH; do cands="$$cands $$d"; done; IFS=$$oldifs; \
	removed=0; seen=" "; \
	for d in $$cands; do \
		[ -n "$$d" ] || continue; \
		case "$$seen" in *" $$d "*) continue ;; esac; seen="$$seen$$d "; \
		f="$$d/$(BINARY)"; \
		if [ -e "$$f" ]; then \
			if [ -w "$$d" ]; then rm -f "$$f"; else sudo rm -f "$$f"; fi; \
			echo "✓ removed $$f"; removed=1; \
		fi; \
	done; \
	[ "$$removed" = 1 ] || echo "· no $(BINARY) binary found"; \
	if command -v $(BINARY) >/dev/null 2>&1; then \
		echo "⚠ '$(BINARY)' is STILL on your PATH at: $$(command -v $(BINARY))"; \
		echo "  remove it manually:  rm -f $$(command -v $(BINARY))"; \
	else \
		echo "✓ '$(BINARY)' is no longer on your PATH"; \
	fi

# test / race: run the unit suite (race adds the data-race detector).
.PHONY: test
test:
	$(GO) test ./...

.PHONY: race
race:
	$(GO) test -race ./...

# live: run ALL live integration tests against local PostgreSQL fixtures.
#
# There are TWO independent fixtures, each behind its own env gate:
#   TEROX_LIVE       multi-shard fixture: shard_0/1/2 + master on 127.0.0.1:55432
#                    (postgres/secret) + pgbouncer on 6432 (override: TEROX_LIVE_HOST).
#   TEROX_TEST_LIVE  single terox_test DB on 127.0.0.1:5433 (postgres/test) — the repl
#                    diagnostic/COPY/advisor tests (override: TEROX_TEST_HOST/TEROX_TEST_PORT).
#
# `make live` sets BOTH gates so the full set actually runs. Previously it set only
# TEROX_LIVE, so the terox_test-gated tests (TestLiveDiagnosticSQL, TestLiveStatementsSQL,
# TestLiveCopyRoundTrip, TestLiveAdvise) SILENTLY SKIPPED and `make live` went green
# without running them. Bring up both fixtures first — or use the focused targets below
# to run just one fixture's tests when only one is available.
.PHONY: live
live:
	TEROX_LIVE=1 TEROX_TEST_LIVE=1 $(GO) test -count=1 -v -run Live ./internal/db/ ./internal/repl/

# live-shards: only the multi-shard fixture tests (TEROX_LIVE).
.PHONY: live-shards
live-shards:
	TEROX_LIVE=1 $(GO) test -count=1 -v -run Live ./internal/db/ ./internal/repl/

# live-test: only the terox_test single-DB tests (TEROX_TEST_LIVE).
.PHONY: live-test
live-test:
	TEROX_TEST_LIVE=1 $(GO) test -count=1 -v -run Live ./internal/repl/

# vet / fmt: static checks and formatting (skips the vendored tree).
.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: fmt
fmt:
	gofmt -w $$(git ls-files '*.go' | grep -v '^vendor/')

# clean: remove the locally built binary.
.PHONY: clean
clean:
	rm -f $(BINARY)
	@echo "✓ removed ./$(BINARY)"

.PHONY: help
help:
	@echo "terox Makefile targets:"
	@echo "  make / make all   clean reinstall into a PATH dir (+config), then verify"
	@echo "  make build        compile ./terox only"
	@echo "  make install      install binary + config.yaml to $(BINDIR)"
	@echo "  make install-go   install to the Go bin dir (no sudo; must be on PATH)"
	@echo "  make verify       check single copy on PATH + version matches main.go"
	@echo "  make uninstall    remove every terox binary on PATH / in known dirs"
	@echo "  make test | race  run the test suite (race adds the detector)"
	@echo "  make live         run ALL live PostgreSQL tests (TEROX_LIVE + TEROX_TEST_LIVE)"
	@echo "  make live-shards  live tests on the multi-shard fixture only (TEROX_LIVE)"
	@echo "  make live-test    live tests on the terox_test DB only (TEROX_TEST_LIVE)"
	@echo "  make vet | fmt    static checks / formatting"
	@echo "  make clean        remove ./terox"
	@echo ""
	@echo "Install dir auto-picks a writable dir on PATH (now: $(BINDIR))."
	@echo "Override:  make install BINDIR=/usr/local/bin   (or PREFIX=/usr/local)"
