# Whitelist Bypass

Tunnels internet traffic through video calling platforms (VK Call, Yandex Telemost) to bypass government whitelist censorship.

## How it works

Video calling platforms use WebRTC with an SFU (Selective Forwarding Unit). The SFU forwards SCTP data channels between participants without inspecting them. This tool creates a DataChannel alongside the call's built-in channels and uses it as a bidirectional data pipe.

- **VK Call**: Uses negotiated DataChannel id:2 (alongside VK's animoji channel id:1)
- **Telemost**: Uses non-negotiated DataChannel labeled "sharing" (matching real screen sharing traffic), with SDP renegotiation via signaling WebSocket

```
Joiner (censored, Android)                Creator (free internet, desktop)

All apps
  |
VpnService (captures all traffic)
  |
tun2socks (IP -> TCP)
  |
SOCKS5 proxy (Go, :1080)
  |
WebSocket (:9000)
  |
WebView (call page)                       Electron (call page)
  |                                         |
DataChannel  <------- SFU ------->  DataChannel
                                            |
                                        WebSocket (:9000)
                                            |
                                        Go relay
                                            |
                                        Internet
```

Traffic goes through the platform's TURN servers which are whitelisted. To the network firewall it looks like a normal video call.

## Components

- `hooks/` - JavaScript hooks injected into call pages. Separate hooks per platform and role:
  - `joiner-vk.js`, `creator-vk.js` - VK Call hooks
  - `joiner-telemost.js`, `creator-telemost.js` - Telemost hooks
  - Hooks intercept RTCPeerConnection, create tunnel DataChannel, bridge to local WebSocket
  - Telemost hooks include fake media (camera/mic), message chunking (994B payload, 1000B total), and SDP renegotiation
- `relay/` - Go relay binary and gomobile library
  - SOCKS5 proxy with TCP CONNECT and UDP ASSOCIATE
  - WebSocket server for browser-relay communication
  - tun2socks (Android only, via build tags)
  - Binary framing protocol: `[4B connID][1B msgType][payload]`
- `android-app/` - Android joiner app
  - WebView loading call page with hook injection
  - VpnService capturing all device traffic
  - Go relay as .aar library (gomobile)
- `creator-app/` - Electron desktop creator app
  - Webview with persistent session for login retention
  - CSP header stripping for localhost WebSocket access
  - Auto-permission granting (camera/mic)
  - Go relay spawned as child process
  - Log panels for relay and hook output

## Prebuilt binaries

Run `./build-release.sh` to produce the full release set in `prebuilts/`:

| File | Platform |
|---|---|
| `WhitelistBypass Creator-*-arm64.dmg` | macOS |
| `WhitelistBypass Creator-*-x64.exe` | Windows x64 |
| `WhitelistBypass Creator-*-ia32.exe` | Windows x86 |
| `WhitelistBypass Creator-*.AppImage` | Linux x64 |
| `whitelist-bypass.apk` | Android |
| `SHA256SUMS.txt` | Checksums for release artifacts |

## Setup

### Creator side (free internet, desktop)

Install and run the Electron app from `prebuilts/`. It bundles the Go relay automatically.

1. Open the app
2. Click "VK Call" or "Telemost"
3. Log in, create a call
4. Copy the join link, send it to the joiner

### Joiner side (censored, Android)

1. Install `whitelist-bypass.apk` from `prebuilts/`
2. Paste the call link in the app
3. The app joins the call, establishes the tunnel, starts VPN
4. All device traffic flows through the call

## Building from source

### Requirements

- Go 1.26+
- gomobile (`go install golang.org/x/mobile/cmd/gomobile@latest`)
- gobind (`go install golang.org/x/mobile/cmd/gobind@latest`)
- Android SDK Command-line Tools + NDK `29.0.14206865`
- Java 17+
- Node.js 20+

### Build scripts

```sh
# Build Go .aar for Android (includes hooks copy)
./build-go.sh

# Build Android release APK -> prebuilts/whitelist-bypass.apk
# Rebuilds the gomobile .aar first by default.
./build-app.sh

# Build Electron apps for all release targets -> prebuilts/
./build-creator.sh

# Build the full release set and checksums -> prebuilts/
./build-release.sh
```

`build-go.sh` and `build-app.sh` automatically detect Homebrew-installed Java/Android toolchains on macOS. If you want to reuse an existing gomobile output while iterating on the Android UI, run `SKIP_GO_BUILD=1 ./build-app.sh`.

### Relay cross-compilation

The Go relay is split into platform-specific files:
- `relay/mobile/mobile.go` - Shared networking code (SOCKS5, WebSocket, framing)
- `relay/mobile/tun_android.go` - Android-only: tun2socks + fdsan fix (CGo)
- `relay/mobile/tun_stub.go` - Desktop stub (no tun2socks needed)

This allows cross-compiling the relay for macOS/Windows/Linux without CGo or Android NDK.

## Repository maintenance

- CI runs Go tests and desktop app sanity checks on every push and pull request.
- Release builds are defined in GitHub Actions and can be triggered with a `v*` tag or manually.
- Generated artifacts such as relay binaries, `.aar` files, unpacked Electron bundles, and `prebuilts/` outputs are not meant to be committed.
