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

if [ -z "$JAVA_HOME" ] && [ -d "$BREW_JAVA_HOME" ]; then
    export JAVA_HOME="$BREW_JAVA_HOME"
    export PATH="/opt/homebrew/opt/openjdk@17/bin:$PATH"
fi

[ -n "$ANDROID_HOME" ] && [ -d "$ANDROID_HOME" ] || { echo "Android SDK not found. Set ANDROID_HOME or ANDROID_SDK_ROOT."; exit 1; }

cd "$ROOT/android-app"

[ -f "./gradlew" ] || { echo "gradlew not found"; exit 1; }

printf 'sdk.dir=%s\n' "$ANDROID_HOME" > local.properties

echo "Building APK..."
./gradlew assembleDebug

APK="app/build/outputs/apk/debug/app-debug.apk"
if [ -f "$APK" ]; then
    mkdir -p "$ROOT/prebuilts"
    cp "$APK" "$ROOT/prebuilts/whitelist-bypass.apk"
    echo "APK ready: prebuilts/whitelist-bypass.apk ($(du -h "$ROOT/prebuilts/whitelist-bypass.apk" | cut -f1))"
else
    echo "Build failed, APK not found"
    exit 1
fi
