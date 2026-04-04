#!/bin/sh
set -e

ROOT="$(cd "$(dirname "$0")" && pwd)"
RELAY_DIR="$ROOT/relay"
CREATOR_DIR="$ROOT/creator-app"
PREBUILTS_DIR="$ROOT/prebuilts"

cleanup_stale_outputs() {
    rm -f \
        "$PREBUILTS_DIR"/WhitelistBypass\ Creator-* \
        "$PREBUILTS_DIR/SHA256SUMS.txt"
}

cleanup_intermediates() {
    rm -rf \
        "$PREBUILTS_DIR/mac-arm64" \
        "$PREBUILTS_DIR/win-unpacked" \
        "$PREBUILTS_DIR/win-ia32-unpacked" \
        "$PREBUILTS_DIR/linux-unpacked"
    rm -f "$PREBUILTS_DIR/builder-debug.yml"
}

echo "=== Building relay binaries ==="
cd "$RELAY_DIR"

echo "macOS (universal)..."
GOOS=darwin GOARCH=amd64 go build -o relay-darwin-amd64 .
GOOS=darwin GOARCH=arm64 go build -o relay-darwin-arm64 .
lipo -create -output relay-darwin relay-darwin-amd64 relay-darwin-arm64
rm relay-darwin-amd64 relay-darwin-arm64

echo "Windows x64..."
GOOS=windows GOARCH=amd64 go build -o relay-windows-x64.exe .
echo "Windows x86..."
GOOS=windows GOARCH=386 go build -o relay-windows-ia32.exe .

echo "Linux x64..."
GOOS=linux GOARCH=amd64 go build -o relay-linux-x64 .

ls -lh relay-darwin relay-windows-*.exe relay-linux-x64

echo ""
echo "=== Building headless creator ==="
mkdir -p "$PREBUILTS_DIR"
cd "$RELAY_DIR/headless"

echo "macOS (universal)..."
GOOS=darwin GOARCH=amd64 go build -o "$RELAY_DIR/headless-darwin-amd64" .
GOOS=darwin GOARCH=arm64 go build -o "$RELAY_DIR/headless-darwin-arm64" .
lipo -create -output "$PREBUILTS_DIR/headless-darwin" "$RELAY_DIR/headless-darwin-amd64" "$RELAY_DIR/headless-darwin-arm64"
rm "$RELAY_DIR/headless-darwin-amd64" "$RELAY_DIR/headless-darwin-arm64"

echo "Linux x64..."
GOOS=linux GOARCH=amd64 go build -o "$PREBUILTS_DIR/headless-linux-x64" .

echo "Windows x64..."
GOOS=windows GOARCH=amd64 go build -o "$PREBUILTS_DIR/headless-windows-x64.exe" .

ls -lh "$PREBUILTS_DIR"/headless-*

cd "$RELAY_DIR"

echo ""
echo "=== Building Electron apps ==="
cd "$CREATOR_DIR"
cleanup_stale_outputs
cleanup_intermediates
npm ci

# macOS (universal binary already)
echo ""
echo "--- macOS ---"
npx electron-builder --mac --publish never

# Windows x64
echo ""
echo "--- Windows x64 ---"
cp "$RELAY_DIR/relay-windows-x64.exe" "$RELAY_DIR/relay-bundle.exe"
npx electron-builder --win --x64 --publish never

# Windows x86
echo ""
echo "--- Windows x86 ---"
cp "$RELAY_DIR/relay-windows-ia32.exe" "$RELAY_DIR/relay-bundle.exe"
npx electron-builder --win --ia32 --publish never

# Linux x64
echo ""
echo "--- Linux x64 ---"
cp "$RELAY_DIR/relay-linux-x64" "$RELAY_DIR/relay-bundle"
npx electron-builder --linux --x64 --publish never

# Cleanup
rm -f \
    "$RELAY_DIR/relay-bundle" \
    "$RELAY_DIR/relay-bundle.exe" \
    "$RELAY_DIR/relay-windows-x64.exe" \
    "$RELAY_DIR/relay-windows-ia32.exe" \
    "$RELAY_DIR/relay-linux-x64"
cleanup_intermediates

echo ""
echo "=== Done ==="
ls -lh "$PREBUILTS_DIR/"
