#!/usr/bin/env bash
set -euo pipefail

# Build an unsigned Apple Silicon DMG containing the Flutter client, the Rust
# native library, the service helper, and the go-libp2p c-shared module.

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="$(sed -n 's/^version[[:space:]]*=[[:space:]]*"\([^"]*\)"/\1/p' "$ROOT/Cargo.toml" | head -n 1)"
if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "package_macos_arm64: run this script on macOS" >&2
  exit 1
fi
if [[ "$(uname -m)" != "arm64" ]]; then
  echo "package_macos_arm64: this script requires an Apple Silicon host" >&2
  exit 1
fi

require() {
  command -v "$1" >/dev/null || {
    echo "package_macos_arm64: $1 not found in PATH" >&2
    exit 1
  }
}
require cargo
require go
require flutter
require create-dmg

export MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-10.14}"
FEATURES="flutter,hwcodec,unix-file-copy-paste"
APP="$ROOT/flutter/build/macos/Build/Products/Release/P2PDesk.app"
APP_MACOS="$APP/Contents/MacOS"
OUT="$ROOT/dist/macos-arm64"

rm -rf "$OUT"
mkdir -p "$OUT"

echo "[1/5] building Rust Flutter library"
cargo build --locked --release --features "$FEATURES" --lib
cargo build --locked --release --features "$FEATURES" --bin service
cp "$ROOT/target/release/liblibrustdesk.dylib" "$ROOT/target/release/librustdesk.dylib"

echo "[2/5] building go-libp2p arm64 dylib"
(
  cd "$ROOT/p2pdesk-net"
  CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
    go build -trimpath -buildmode=c-shared -ldflags='-s -w' \
    -o "$ROOT/target/release/libp2pdesk_net.dylib" .
)

echo "[3/5] building Flutter macOS app"
(
  cd "$ROOT/flutter"
  flutter build macos --release --no-pub \
    --build-name="$VERSION" --build-number=1
)

echo "[4/5] assembling App Bundle"
cp "$ROOT/target/release/librustdesk.dylib" "$APP_MACOS/librustdesk.dylib"
cp "$ROOT/target/release/libp2pdesk_net.dylib" "$APP_MACOS/libp2pdesk_net.dylib"
cp "$ROOT/target/release/service" "$APP_MACOS/service"
chmod 755 "$APP_MACOS/service"

DMG="$OUT/P2PDesk-$VERSION-macos-arm64.dmg"
echo "[5/5] creating DMG"
create-dmg \
  --volname "P2PDesk" \
  --window-pos 200 120 \
  --window-size 800 400 \
  --icon-size 100 \
  --app-drop-link 600 185 \
  --icon "P2PDesk.app" 200 190 \
  --hide-extension "P2PDesk.app" \
  "$DMG" "$APP"

GO_DYLIB_SHA256="$(shasum -a 256 "$APP_MACOS/libp2pdesk_net.dylib" | awk '{print toupper($1)}')"
DMG_SHA256="$(shasum -a 256 "$DMG" | awk '{print toupper($1)}')"
cat > "$OUT/MANIFEST.json" <<EOF
{
  "version": "$VERSION",
  "platform": "macos-arm64",
  "package": "$(basename "$DMG")",
  "package_sha256": "$DMG_SHA256",
  "go_dylib": "libp2pdesk_net.dylib",
  "go_dylib_sha256": "$GO_DYLIB_SHA256",
  "signed": false,
  "notarized": false
}
EOF
echo "DMG: $DMG"
echo "SHA256: $DMG_SHA256"
