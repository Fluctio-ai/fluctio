.PHONY: build build-web bundle-skills clean release-local install test dev

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
# Stamp build identity into both the main package (legacy `fluctio
# version` consumer) and internal/buildinfo (the agent runtime + system
# prompt reader). Keeping both in sync from one VERSION variable means
# release builds hand the model the same string the CLI reports.
BUILDINFO = github.com/fluctio-ai/fluctio/internal/buildinfo
LDFLAGS  = -s -w \
	-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE) \
	-X $(BUILDINFO).Version=$(VERSION) -X $(BUILDINFO).Commit=$(COMMIT) -X $(BUILDINFO).Date=$(DATE)

# Install destination. Default is the per-user XDG-style bin so a plain
# `make install` doesn't need sudo. Override with e.g.
#   make install PREFIX=/usr/local        (system-wide; needs sudo)
#   make install PREFIX=/opt/homebrew     (Apple Silicon brew layout)
PREFIX ?= $(HOME)/.local

build-web:
	cd web && pnpm install --frozen-lockfile && pnpm build
	rm -rf internal/setup/web
	cp -r web/out internal/setup/web

# bundle-skills syncs skills the binary should ship with into the embed
# tree under internal/agent/bundled_skills/. Source of truth lives at
# repo-root skills/<name>/ so editing happens in one place; this target
# overwrites the embed copy each build so drift can't accumulate.
# `go:embed` can't follow symlinks or escape the package dir, so a real
# copy is the only path that works.
bundle-skills:
	@rm -rf internal/agent/bundled_skills/skill-creator
	@cp -R skills/skill-creator internal/agent/bundled_skills/skill-creator
	@rm -rf internal/agent/bundled_skills/find-skills
	@cp -R skills/find-skills internal/agent/bundled_skills/find-skills
	@echo "==> bundled skills synced"

build: build-web bundle-skills
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/fluctio ./cmd/fluctio

install: build
	install -d $(PREFIX)/bin
	install -m 0755 bin/fluctio $(PREFIX)/bin/fluctio
	@echo
	@echo "==> installed: $(PREFIX)/bin/fluctio"
	@case ":$$PATH:" in *":$(PREFIX)/bin:"*) ;; *) \
	  echo "    NOTE: $(PREFIX)/bin is not on your PATH."; \
	  echo "    Add to ~/.zshrc:  export PATH=\"$(PREFIX)/bin:\$$PATH\"" ;; \
	esac

test:
	go test ./...

dev: build-web
	air

clean:
	rm -rf bin/ dist/ tmp/

# Build all platforms
release-local: build-web bundle-skills
	@mkdir -p dist
	@# macOS
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/fluctio_darwin_arm64/fluctio  ./cmd/fluctio
	GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/fluctio_darwin_amd64/fluctio  ./cmd/fluctio
	@# Linux
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/fluctio_linux_arm64/fluctio   ./cmd/fluctio
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/fluctio_linux_amd64/fluctio   ./cmd/fluctio
	@# Windows
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/fluctio_windows_amd64/fluctio.exe ./cmd/fluctio
	GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/fluctio_windows_arm64/fluctio.exe ./cmd/fluctio
	@# Package: tar.gz for unix, zip for windows
	@cd dist && for d in fluctio_darwin_* fluctio_linux_*; do tar -czf "$${d}.tar.gz" -C "$$d" fluctio; done
	@cd dist && for d in fluctio_windows_*; do (cd "$$d" && zip -q "../$${d}.zip" fluctio.exe); done
	@echo "Release artifacts:"
	@ls -lh dist/*.tar.gz dist/*.zip 2>/dev/null
