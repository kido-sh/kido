# Build, sign, package and install kido. `make help` lists the targets.
# Written for the GNU Make 3.81 that ships with macOS.

GO          ?= go
CARGO       ?= cargo
HOST_OS     := $(shell $(GO) env GOHOSTOS)
TARGET_OS   ?= $(if $(GOOS),$(GOOS),$(HOST_OS))
TARGET_ARCH ?= $(if $(GOARCH),$(GOARCH),arm64)
PREFIX      ?= /usr/local
DESTDIR     ?=
# Where builds and release archives go.
OUT     ?= bin
DISTDIR ?= dist

# A tag's leading "v" is dropped (v0.1.0 -> 0.1.0). Falls back to the version of the last build, so
# a later `sudo make install` (where git refuses a checkout owned by another user) neither changes
# the version nor rebuilds as root.
VERSION ?= $(shell v=$$(git describe --always --dirty 2>/dev/null) && echo "$${v\#v}" || cat $(OUT)/.version 2>/dev/null || echo 0.0.0-dev)

# A development QEMU that kido falls back to after $KIDO_QEMU and the one `kido setup` installs
# (internal/setup.DevQEMU); none by default.
DEV_QEMU ?=
# Likewise a development kido-vmm (internal/runtime.DevVMM); installs use libexec/kido/kido-vmm.
DEV_VMM ?=

# DEVELOPER_ID="Developer ID Application: NAME (TEAMID)" switches `make sign` from ad-hoc signing
# to Developer ID with the hardened runtime and a secure timestamp.
DEVELOPER_ID   ?=
SIGN_ID_PREFIX ?= sh.kido.
export DEVELOPER_ID

ifeq ($(TARGET_OS),darwin)
BINARIES      := kido kido-agent kido-vmm
BUILD_TARGETS := $(OUT)/kido $(OUT)/kido-agent $(OUT)/kido-vmm
SIGN_DEP      := sign
KIDO_ENV      := GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH)
HAVE_VMM      := yes
else ifeq ($(TARGET_OS)-$(TARGET_ARCH),linux-arm64)
BINARIES      := kido kido-agent kido-vmm
BUILD_TARGETS := $(OUT)/kido $(OUT)/kido-agent $(OUT)/kido-vmm
SIGN_DEP      := build
KIDO_ENV      := CGO_ENABLED=0 GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH)
HAVE_VMM      := yes
else
BINARIES      := kido kido-agent
BUILD_TARGETS := $(OUT)/kido $(OUT)/kido-agent
SIGN_DEP      := build
KIDO_ENV      := CGO_ENABLED=0 GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH)
HAVE_VMM      := no
endif

ifeq ($(HOST_OS),darwin)
TAR_FLAGS := --no-mac-metadata --no-xattrs --no-acls --uid 0 --gid 0 --uname root --gname wheel
else
TAR_FLAGS := --no-xattrs --no-acls --numeric-owner --owner=0 --group=0
endif

SHA256       := $(shell if command -v shasum >/dev/null 2>&1; then echo "shasum -a 256"; else echo "sha256sum"; fi)
ENTITLEMENTS := packaging/entitlements
LDFLAGS      := -s -w -X main.version=$(VERSION) -X github.com/kido-sh/kido/internal/setup.DevQEMU=$(DEV_QEMU) -X github.com/kido-sh/kido/internal/runtime.DevVMM=$(DEV_VMM)
GOBUILD       = $(GO) build -trimpath -ldflags '$(LDFLAGS)'
GO_SOURCES   := $(shell find . -path ./.git -prune -o -name '*.go' -print) go.mod go.sum
VMM_LINUX_SOURCES := $(shell find vmm-linux/src -name '*.rs' 2>/dev/null) vmm-linux/Cargo.toml vmm-linux/Cargo.lock vmm-linux/.cargo/config.toml
HAVE_AGENT   := $(wildcard agent/*.go)

BINDIR     = $(DESTDIR)$(PREFIX)/bin
LIBEXECDIR = $(DESTDIR)$(PREFIX)/libexec/kido
DOCDIR     = $(DESTDIR)$(PREFIX)/share/doc/kido

RELEASE_PUB       := internal/upgrade/release.pub
RELEASE_NAMESPACE := release@kido.sh

DIST_NAME    := kido-$(VERSION)-$(TARGET_OS)-$(TARGET_ARCH)
DIST_ROOT    := $(DISTDIR)/$(DIST_NAME)
DIST_TAR     := $(DISTDIR)/$(DIST_NAME).tar.gz

.PHONY: all build sign dist sign-dist install uninstall test test-vmm vet clean help FORCE

all: build

build: $(BUILD_TARGETS)

# Rewritten only when the version changes, so the binaries relink exactly then.
$(OUT)/.version: FORCE
	@mkdir -p $(OUT)
	@echo '$(VERSION)' | cmp -s - $@ || echo '$(VERSION)' > $@

$(OUT)/kido: $(GO_SOURCES) $(OUT)/.version
	$(KIDO_ENV) $(GOBUILD) -o $@ ./cmd/kido

ifeq ($(HAVE_AGENT),)
$(OUT)/kido-agent:
	@echo "note: ./agent has no Go files yet; skipping $@" >&2
else
$(OUT)/kido-agent: $(GO_SOURCES) $(OUT)/.version
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GOBUILD) -o $@ ./agent
endif

ifeq ($(TARGET_OS),darwin)
# The Virtualization.framework helper; `make sign` gives it com.apple.security.virtualization.
$(OUT)/kido-vmm: vmm/kido-vmm.m vmm/metal.m vmm/metal.h vmm/kmtl_proto.h vmm/kmtl_shm.c vmm/kmtl_shm.h vmm/notify.c vmm/notify.h
	@mkdir -p $(OUT)
	xcrun clang -fobjc-arc -O2 -Wall -Wextra -Wno-unused-parameter -mmacosx-version-min=13.0 \
	  -framework CoreServices -framework Foundation -framework Metal -framework Virtualization -framework vmnet -o $@ vmm/kido-vmm.m vmm/metal.m vmm/kmtl_shm.c vmm/notify.c
else ifeq ($(TARGET_OS)-$(TARGET_ARCH),linux-arm64)
$(OUT)/kido-vmm: $(VMM_LINUX_SOURCES)
	@mkdir -p $(OUT)
	cd vmm-linux && $(CARGO) build --release --locked --target aarch64-unknown-linux-musl
	cp vmm-linux/target/aarch64-unknown-linux-musl/release/kido-vmm $@
endif

ifeq ($(TARGET_OS),darwin)
sign: build
	@set -e; for name in $(BINARIES); do \
	  bin="$(OUT)/$$name"; \
	  if [ ! -f "$$bin" ]; then echo "note: $$bin was not built; not signing it" >&2; continue; fi; \
	  set -- --force --identifier "$(SIGN_ID_PREFIX)$$name"; \
	  if [ -f "$(ENTITLEMENTS)/$$name.entitlements" ]; then set -- "$$@" --entitlements "$(ENTITLEMENTS)/$$name.entitlements"; fi; \
	  if [ -n "$$DEVELOPER_ID" ]; then set -- "$$@" --sign "$$DEVELOPER_ID" --options runtime --timestamp; \
	  else set -- "$$@" --sign -; fi; \
	  echo "codesign $$* $$bin"; \
	  codesign "$$@" "$$bin"; \
	  codesign --verify --strict "$$bin"; \
	done
else
sign: build
	@echo "note: codesign is macOS-only; skipping sign for $(TARGET_OS)/$(TARGET_ARCH)" >&2
endif

# The release assets: the versioned tarball, install.sh and their SHA256SUMS.
dist: $(SIGN_DEP)
	@if [ ! -f $(OUT)/kido-agent ]; then echo "error: $(OUT)/kido-agent is missing; a release needs the guest agent" >&2; exit 1; fi
	rm -rf $(DIST_ROOT)
	install -d $(DIST_ROOT)/bin $(DIST_ROOT)/libexec/kido $(DIST_ROOT)/share/doc/kido
	install -m 0755 $(OUT)/kido $(DIST_ROOT)/bin/kido
	install -m 0755 $(OUT)/kido-agent $(DIST_ROOT)/libexec/kido/kido-agent
ifeq ($(HAVE_VMM),yes)
	install -m 0755 $(OUT)/kido-vmm $(DIST_ROOT)/libexec/kido/kido-vmm
endif
	install -m 0644 README.md $(DIST_ROOT)/share/doc/kido/README.md
	install -m 0644 LICENSE $(DIST_ROOT)/share/doc/kido/LICENSE
	COPYFILE_DISABLE=1 tar -C $(DIST_ROOT) $(TAR_FLAGS) -czf $(DIST_TAR) bin libexec share
	rm -rf $(DIST_ROOT)
	install -m 0755 install.sh $(DISTDIR)/install.sh
	rm -f $(DISTDIR)/SHA256SUMS $(DISTDIR)/SHA256SUMS.sig
	cd $(DISTDIR) && $(SHA256) $(DIST_NAME).tar.gz install.sh > SHA256SUMS
	@cat $(DISTDIR)/SHA256SUMS

sign-dist:
	@test -n "$(RELEASE_KEY)" || { echo "error: set RELEASE_KEY to the release signing key" >&2; exit 1; }
	@test -f $(DISTDIR)/SHA256SUMS || { echo "error: run make dist first" >&2; exit 1; }
	rm -f $(DISTDIR)/SHA256SUMS.sig
	ssh-keygen -Y sign -f $(RELEASE_KEY) -n $(RELEASE_NAMESPACE) $(DISTDIR)/SHA256SUMS
	@awk '{ print "kido namespaces=\"$(RELEASE_NAMESPACE)\" " $$1 " " $$2 }' $(RELEASE_PUB) > $(DISTDIR)/.allowed_signers
	ssh-keygen -Y verify -f $(DISTDIR)/.allowed_signers -I kido -n $(RELEASE_NAMESPACE) \
	  -s $(DISTDIR)/SHA256SUMS.sig < $(DISTDIR)/SHA256SUMS
	@rm -f $(DISTDIR)/.allowed_signers

install: $(SIGN_DEP)
	install -d $(BINDIR) $(LIBEXECDIR) $(DOCDIR)
	install -m 0755 $(OUT)/kido $(BINDIR)/kido
	@if [ -f $(OUT)/kido-agent ]; then echo "install -m 0755 $(OUT)/kido-agent $(LIBEXECDIR)/kido-agent"; \
	  install -m 0755 $(OUT)/kido-agent $(LIBEXECDIR)/kido-agent; \
	else echo "note: $(OUT)/kido-agent was not built; not installing it" >&2; fi
ifeq ($(HAVE_VMM),yes)
	install -m 0755 $(OUT)/kido-vmm $(LIBEXECDIR)/kido-vmm
endif
	install -m 0644 README.md $(DOCDIR)/README.md
	install -m 0644 LICENSE $(DOCDIR)/LICENSE

uninstall:
	rm -f $(BINDIR)/kido $(LIBEXECDIR)/kido-agent $(LIBEXECDIR)/kido-vmm $(DOCDIR)/README.md $(DOCDIR)/LICENSE
	-rmdir $(LIBEXECDIR) $(DOCDIR) 2>/dev/null

ifeq ($(TARGET_OS),darwin)
test: test-vmm
	$(GO) test ./...
else
test:
	$(GO) test ./...
endif

vet:
	$(GO) vet ./...

clean:
	rm -rf $(OUT) $(DISTDIR)

help:
	@echo "make              build $(BUILD_TARGETS) for $(TARGET_OS)/$(TARGET_ARCH) (VERSION=$(VERSION))"
	@echo "make sign         ad-hoc sign $(OUT)/* on darwin; DEVELOPER_ID='Developer ID Application: ...' for a release"
	@echo "make dist         $(DIST_TAR), $(DISTDIR)/install.sh and $(DISTDIR)/SHA256SUMS"
	@echo "make sign-dist RELEASE_KEY=FILE   sign SHA256SUMS with ssh-keygen -Y sign"
	@echo "make install      install into PREFIX=$(PREFIX) (DESTDIR is honored); then run: kido setup"
	@echo "make uninstall    remove what install put there"
	@echo "make test | vet   go test ./... | go vet ./..."
	@echo "make clean        remove $(OUT)/ and $(DISTDIR)/"
	@echo "TARGET_OS=linux TARGET_ARCH=arm64 cross-builds for Linux; OUT=DIR DISTDIR=DIR set output dirs"

# notify.c and kmtl_shm.c tests, with the address and undefined-behaviour sanitizers.
test-vmm:
	@mkdir -p $(OUT)
	xcrun clang -O1 -g -Wall -Wextra -fsanitize=address,undefined -o $(OUT)/notify_test vmm/notify_test.c vmm/notify.c
	$(OUT)/notify_test
	xcrun clang -O1 -g -Wall -Wextra -fsanitize=address,undefined -o $(OUT)/kmtl_shm_test vmm/kmtl_shm_test.c vmm/kmtl_shm.c
	$(OUT)/kmtl_shm_test
