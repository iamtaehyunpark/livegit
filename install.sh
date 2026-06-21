#!/usr/bin/env sh
# Live Git (lg) installer — downloads a prebuilt, self-contained binary.
# No Go toolchain required to run lg; this just fetches the right binary.
#
#   curl -fsSL https://raw.githubusercontent.com/iamtaehyunpark/livegit/main/install.sh | sh
#
# Override install dir:  PREFIX=/usr/local sh install.sh
set -eu

REPO="iamtaehyunpark/livegit"
PREFIX="${PREFIX:-$HOME/.local}"
BINDIR="$PREFIX/bin"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin|linux) ;;
  *) echo "unsupported os: $os" >&2; exit 1 ;;
esac

asset="lg-${os}-${arch}"
url="https://github.com/${REPO}/releases/latest/download/${asset}"

echo "downloading ${asset} ..."
mkdir -p "$BINDIR"
tmp="$BINDIR/.lg.new.$$"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$url" -o "$tmp"
else
  wget -qO "$tmp" "$url"
fi
chmod +x "$tmp"
if [ "$os" = "darwin" ]; then
  # Clear quarantine and apply a robust ad-hoc signature so macOS doesn't
  # SIGKILL it as "Code Signature Invalid".
  xattr -c "$tmp" 2>/dev/null || true
  codesign --force --sign - "$tmp" >/dev/null 2>&1 || true
fi
# Atomic rename (fresh inode) — never overwrite the binary in place.
mv -f "$tmp" "$BINDIR/lg"

echo "installed $BINDIR/lg"
case ":$PATH:" in
  *":$BINDIR:"*) ;;
  *) echo "NOTE: add $BINDIR to your PATH:  export PATH=\"$BINDIR:\$PATH\"" ;;
esac
"$BINDIR/lg" --version || true
