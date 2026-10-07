# Build, test and release Tallyfold. Go comes from mise (see mise.toml).
#
#   make test       run the tests
#   make lint       formatting, vet, shellcheck
#   make dev        build for this machine into plugin/bin/dev/ (the launcher prefers it)
#   make dist       build release binaries for every platform into dist/
#   make checksums  rebuild dist/ and record its checksums in plugin/bin/release
#   make verify     rebuild dist/ and fail unless it matches plugin/bin/release
#   make bump NEW=x.y.z  set the version everywhere, then record checksums
#   make og         render site/og.png from design/og.html with headless Chrome

MISE := $(if $(shell command -v mise 2>/dev/null),mise exec --,)
GO ?= $(MISE) go
GOFMT ?= $(MISE) gofmt
VERSION := $(shell sed -n 's/^version //p' plugin/bin/release)
PLATFORMS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64

# Reproducible builds: the same source and toolchain give the same bytes on any
# machine, so CI can confirm that the checksums committed in the plugin describe
# the binaries it publishes.
BUILD_ENV := CGO_ENABLED=0 GOAMD64=v1 GOARM64=v8.0 GOFLAGS=-mod=readonly
BUILD_FLAGS := -trimpath -buildvcs=false -ldflags "-s -w -buildid= -X main.version=$(VERSION)"

.PHONY: test lint dev dist checksums verify bump og clean

CHROME ?= /Applications/Google Chrome.app/Contents/MacOS/Google Chrome

test:
	$(GO) test ./...

lint:
	@out=$$($(GOFMT) -l .); test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }
	$(GO) vet ./...
	shellcheck plugin/bin/tallyfold

dev:
	$(GO) build -o plugin/bin/dev/tallyfold ./cmd/tallyfold

dist:
	rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "build $$os/$$arch"; \
		$(BUILD_ENV) GOOS=$$os GOARCH=$$arch $(GO) build $(BUILD_FLAGS) \
			-o dist/tallyfold_$(VERSION)_$${os}_$${arch} ./cmd/tallyfold || exit 1; \
	done

checksums: dist
	@{ echo "version $(VERSION)"; cd dist && for f in tallyfold_$(VERSION)_*; do \
		echo "sha256 $$(shasum -a 256 $$f | cut -d ' ' -f 1) $$f"; done; } > plugin/bin/release.tmp
	mv plugin/bin/release.tmp plugin/bin/release
	@cat plugin/bin/release

verify: dist
	@cd dist && for f in tallyfold_$(VERSION)_*; do \
		want=$$(awk -v a=$$f '$$1 == "sha256" && $$3 == a { print $$2 }' ../plugin/bin/release); \
		got=$$(shasum -a 256 $$f | cut -d ' ' -f 1); \
		if [ "$$want" != "$$got" ]; then echo "MISMATCH $$f: built $$got, plugin records $$want"; exit 1; fi; \
		echo "ok $$f"; done

bump:
	@test -n "$(NEW)" || { echo "usage: make bump NEW=x.y.z"; exit 1; }
	sed -i.bak -E 's/"version": "[^"]+"/"version": "$(NEW)"/' plugin/plugin.json .claude-plugin/marketplace.json
	rm -f plugin/plugin.json.bak .claude-plugin/marketplace.json.bak
	printf 'version %s\n' "$(NEW)" > plugin/bin/release
	$(MAKE) checksums

og:
	"$(CHROME)" --headless=new --disable-gpu --hide-scrollbars --allow-file-access-from-files \
		--blink-settings=preferredColorScheme=1 --virtual-time-budget=8000 --window-size=1200,630 \
		--screenshot=$(CURDIR)/site/og.png file://$(CURDIR)/design/og.html

clean:
	rm -rf dist plugin/bin/dev
