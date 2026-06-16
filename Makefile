BINARY    := gpm
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS   := -ldflags "-X github.com/parichit13/gpm/cmd.Version=$(VERSION) -s -w"
# Install into the same user-writable location the curl installer uses, so a
# from-source install needs no sudo and never leaves a second copy elsewhere.
# Override with: make install INSTALL_DIR=/some/other/bin
INSTALL_DIR ?= $(HOME)/.gpm/bin
INSTALL     := $(INSTALL_DIR)/$(BINARY)

.PHONY: build install uninstall clean test run-daemon

build:
	go build $(LDFLAGS) -o $(BINARY) .

install: build
	@mkdir -p "$(INSTALL_DIR)"
	@# Remove first so the copy lands on a FRESH inode. On Apple Silicon,
	@# overwriting a previously-executed binary in place leaves the kernel's
	@# per-vnode code-signature hash stale, and it SIGKILLs the new binary on
	@# exec ("zsh: killed"). A new inode forces the kernel to re-read the
	@# signature; the re-sign is belt-and-suspenders (and a no-op off macOS).
	rm -f "$(INSTALL)"
	cp $(BINARY) "$(INSTALL)"
	@codesign --force --sign - "$(INSTALL)" 2>/dev/null || true
	@echo "Installed $(BINARY) to $(INSTALL)"
	@case ":$$PATH:" in \
		*":$(INSTALL_DIR):"*) echo "Run: gpm daemon start" ;; \
		*) echo "Add to PATH (then restart your shell): export PATH=\"$(INSTALL_DIR):\$$PATH\"" ;; \
	esac

uninstall:
	rm -f "$(INSTALL)"
	@echo "Removed $(INSTALL)"

clean:
	rm -f $(BINARY)

test:
	go test ./...

# Local cross-compile for manual release uploads. Asset names match what the
# installer and `gpm update` expect (gpm_<os>_<arch>). CI normally uses
# GoReleaser instead (see .goreleaser.yaml).
release-linux:
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY)_linux_amd64 .
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BINARY)_linux_arm64 .

release-darwin:
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY)_darwin_amd64 .
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BINARY)_darwin_arm64 .

release: release-linux release-darwin
