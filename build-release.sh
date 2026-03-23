#!/bin/sh
set -e

ROOT="$(cd "$(dirname "$0")" && pwd)"
PREBUILTS_DIR="$ROOT/prebuilts"

"$ROOT/build-app.sh"
"$ROOT/build-creator.sh"

cd "$PREBUILTS_DIR"
rm -f SHA256SUMS.txt
find . -maxdepth 1 -type f \
    \( -name 'WhitelistBypass Creator-*' -o -name 'whitelist-bypass.apk' \) \
    -print | LC_ALL=C sort | while IFS= read -r file; do
        shasum -a 256 "$file"
    done > SHA256SUMS.txt

echo "Release manifest ready: prebuilts/SHA256SUMS.txt"
