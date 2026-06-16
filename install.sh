#!/usr/bin/env bash
#
# gpm installer — downloads the right prebuilt binary, verifies it, installs it
# to ~/.gpm/bin, adds that to your PATH, and starts the daemon.
#
#   curl -fsSL https://raw.githubusercontent.com/parichit13/gpm/main/install.sh | bash
#
# Env overrides:
#   GPM_VERSION       install a specific tag (default: latest release)
#   GPM_INSTALL_DIR   install location (default: ~/.gpm/bin)
#
set -euo pipefail

REPO="parichit13/gpm"
BIN="gpm"
INSTALL_DIR="${GPM_INSTALL_DIR:-$HOME/.gpm/bin}"

info() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# ── http helpers (curl or wget) ──────────────────────────────────────────────
if command -v curl >/dev/null 2>&1; then
  fetch()    { curl -fsSL "$1"; }            # to stdout
  fetch_to() { curl -fsSL -o "$2" "$1"; }    # to file
elif command -v wget >/dev/null 2>&1; then
  fetch()    { wget -qO- "$1"; }
  fetch_to() { wget -qO "$2" "$1"; }
else
  err "need curl or wget installed"
fi

# ── detect platform ──────────────────────────────────────────────────────────
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) err "unsupported OS: $os (gpm supports macOS and Linux)";;
esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  arch=amd64;;
  arm64|aarch64) arch=arm64;;
  *) err "unsupported architecture: $arch";;
esac
asset="${BIN}_${os}_${arch}"

# ── resolve version ──────────────────────────────────────────────────────────
tag="${GPM_VERSION:-}"
if [ -z "$tag" ]; then
  info "Finding the latest release…"
  tag=$(fetch "https://api.github.com/repos/${REPO}/releases/latest" \
        | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/') || true
  [ -n "$tag" ] || err "could not determine the latest release of ${REPO} (none published yet?)"
fi
base="https://github.com/${REPO}/releases/download/${tag}"
info "Installing gpm ${tag} (${os}/${arch})"

# ── download + verify ────────────────────────────────────────────────────────
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

info "Downloading ${asset}…"
fetch_to "${base}/${asset}" "${tmp}/${BIN}" || err "download failed: ${base}/${asset}"

if fetch_to "${base}/checksums.txt" "${tmp}/checksums.txt" 2>/dev/null; then
  info "Verifying checksum…"
  expected=$(grep " ${asset}\$" "${tmp}/checksums.txt" | awk '{print $1}')
  if [ -n "$expected" ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      got=$(sha256sum "${tmp}/${BIN}" | awk '{print $1}')
    else
      got=$(shasum -a 256 "${tmp}/${BIN}" | awk '{print $1}')
    fi
    [ "$expected" = "$got" ] || err "checksum mismatch (expected $expected, got $got)"
  else
    warn "no checksum entry for ${asset}; skipping verification"
  fi
else
  warn "no checksums.txt in release; skipping verification"
fi

# ── install ──────────────────────────────────────────────────────────────────
mkdir -p "$INSTALL_DIR"
chmod +x "${tmp}/${BIN}"
if [ "$os" = "darwin" ]; then
  xattr -dr com.apple.quarantine "${tmp}/${BIN}" 2>/dev/null || true
  codesign --force --sign - "${tmp}/${BIN}" 2>/dev/null || true
fi
# Move into place (fresh inode — safe to replace a running binary).
mv -f "${tmp}/${BIN}" "${INSTALL_DIR}/${BIN}"
info "Installed to ${INSTALL_DIR}/${BIN}"

# ── PATH wiring ──────────────────────────────────────────────────────────────
add_to_path() {
  local profile="$1"
  local line="export PATH=\"${INSTALL_DIR}:\$PATH\""
  [ -f "$profile" ] || return 1
  if ! grep -qF "$INSTALL_DIR" "$profile" 2>/dev/null; then
    printf '\n# Added by gpm installer\n%s\n' "$line" >> "$profile"
    return 0
  fi
  return 2
}

on_path=0
case ":$PATH:" in *":$INSTALL_DIR:"*) on_path=1;; esac

if [ "$on_path" -eq 0 ]; then
  shell_name=$(basename "${SHELL:-}")
  case "$shell_name" in
    zsh)  profile="$HOME/.zshrc";;
    bash) if [ "$os" = "darwin" ]; then profile="$HOME/.bash_profile"; else profile="$HOME/.bashrc"; fi;;
    *)    profile="$HOME/.profile";;
  esac
  if add_to_path "$profile"; then
    info "Added ${INSTALL_DIR} to PATH in ${profile}"
  fi
  warn "Open a new terminal, or run:  export PATH=\"${INSTALL_DIR}:\$PATH\""
fi

# ── start the daemon ─────────────────────────────────────────────────────────
info "Starting the gpm daemon…"
"${INSTALL_DIR}/${BIN}" daemon start || warn "could not start the daemon; run: gpm daemon start"

cat <<EOF

gpm ${tag} is installed and running.

  gpm version          # confirm the version
  gpm list             # show managed services (empty for now)
  gpm start ./app web --port 8080 --watch
  gpm update           # upgrade gpm later

EOF
