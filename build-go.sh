#!/bin/sh
set -e

ROOT="$(cd "$(dirname "$0")" && pwd)"
DEFAULT_ANDROID_HOME="$HOME/Library/Android/sdk"
BREW_ANDROID_HOME="/opt/homebrew/share/android-commandlinetools"
BREW_JAVA_HOME="/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home"

if [ -z "$ANDROID_HOME" ] && [ -n "$ANDROID_SDK_ROOT" ]; then
    ANDROID_HOME="$ANDROID_SDK_ROOT"
fi
if [ -z "$ANDROID_HOME" ] && [ -d "$DEFAULT_ANDROID_HOME" ]; then
    ANDROID_HOME="$DEFAULT_ANDROID_HOME"
fi
if [ -z "$ANDROID_HOME" ] && [ -d "$BREW_ANDROID_HOME" ]; then
    ANDROID_HOME="$BREW_ANDROID_HOME"
fi
export ANDROID_HOME
export ANDROID_SDK_ROOT="$ANDROID_HOME"

if [ -z "$ANDROID_NDK_HOME" ]; then
    ANDROID_NDK_HOME="$ANDROID_HOME/ndk/29.0.14206865"
fi
export ANDROID_NDK_HOME

if [ -z "$JAVA_HOME" ] && [ -d "$BREW_JAVA_HOME" ]; then
    export JAVA_HOME="$BREW_JAVA_HOME"
    export PATH="/opt/homebrew/opt/openjdk@17/bin:$PATH"
fi
export CGO_LDFLAGS="-Wl,-z,max-page-size=16384"
export PATH="$PATH:/opt/homebrew/bin:$HOME/go/bin"

# Check deps
command -v go >/dev/null || { echo "go not found"; exit 1; }
command -v gomobile >/dev/null || { echo "gomobile not found, run: go install golang.org/x/mobile/cmd/gomobile@latest"; exit 1; }
command -v gobind >/dev/null || { echo "gobind not found, run: go install golang.org/x/mobile/cmd/gobind@latest"; exit 1; }
[ -n "$ANDROID_HOME" ] && [ -d "$ANDROID_HOME" ] || { echo "Android SDK not found. Set ANDROID_HOME or ANDROID_SDK_ROOT."; exit 1; }
[ -d "$ANDROID_NDK_HOME" ] || { echo "NDK not found at $ANDROID_NDK_HOME"; exit 1; }

cd "$ROOT/relay"

echo "Building gomobile .aar..."
gomobile bind -target=android -androidapi 23 -o mobile.aar ./mobile/

echo "Copying .aar to android-app/libs..."
mkdir -p ../android-app/app/libs
cp mobile.aar ../android-app/app/libs/mobile.aar

echo "Copying hooks to assets..."
mkdir -p ../android-app/app/src/main/assets
cp ../hooks/joiner-vk.js ../android-app/app/src/main/assets/joiner-vk.js
cp ../hooks/joiner-telemost.js ../android-app/app/src/main/assets/joiner-telemost.js

echo "Done. .aar size: $(du -h mobile.aar | cut -f1)"
