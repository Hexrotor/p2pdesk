# p2pdesk build guide

## Requirements

| Tool | Version | Notes |
|---|---|---|
| Rust (MSVC) | recent stable | Windows host |
| Visual Studio | 2022 | C++ toolchain |
| vcpkg | — | libvpx, libyuv, opus, aom installed; `VCPKG_ROOT` set |
| Go | 1.26 | for the c-shared module (`p2pdesk-net/`) |
| Flutter | **3.24.5** | newer Flutter breaks this 1.4.9 codebase (Dart API removals) |
| Android SDK + NDK | NDK 26.x | controller APK builds |
| Python | 3.x | UI resource helpers |

## Windows (target side)

```powershell
# 1. UI resources (src/ui/inline.rs is a byte snapshot of the .tis/.css)
python res/inline-sciter.py

# 2. Go c-shared module
cd p2pdesk-net
$env:CGO_ENABLED = '1'; $env:GOOS = 'windows'; $env:GOARCH = 'amd64'
go build -trimpath -buildmode=c-shared -ldflags='-s -w' -o p2pdesk_net.dll .
cd ..

# 3. rustdesk (`inline` embeds UI; hwcodec/vram enable H.264/H.265 hardware paths)
cargo build --release --features inline,hwcodec,vram --bin p2pdesk

# 4. place p2pdesk_net.dll next to p2pdesk.exe (target\release)
```

Deployment = three files side by side:

- `p2pdesk.exe`
- `sciter.dll` — the Sciter engine (external, from the Sciter SDK `bin.win/x64/`), loaded at runtime
- `p2pdesk_net.dll` — the go-libp2p module, loaded from the exe's directory

`p2pdesk.exe` and `p2pdesk_net.dll` must always come from the same build:
`p2pd_status_t` grows append-only across ABI v1, and while the Rust side
zero-initializes the struct (an older module writing fewer fields is safe),
the reverse pairing — a newer module writing a larger struct into an older
exe's smaller one — would corrupt the caller's stack on every status poll.
Never mix an old exe with a new DLL (or vice versa).

### Windows test installer

After the three files above are built, generate the self-contained installer
from a clean payload directory:

```powershell
python scripts/package_windows.py --force
```

The output is `dist/windows/P2PDesk-<version>-windows-x64-install.exe`, with
`MANIFEST.json` and `README.txt` beside it. The script embeds exactly
`p2pdesk.exe`, `p2pdesk_net.dll`, and `sciter.dll`; it never packages the whole
`target\release` directory. The manifest records the source and installer
SHA256 values and the embedded entrypoint. The installer extracts a clean
payload before opening the normal P2PDesk installation UI.

### macOS arm64 test package

Build the Apple Silicon client on an arm64 Mac with Xcode, Flutter 3.24.5,
Go 1.26+, Rust, and `create-dmg` installed:

```bash
bash scripts/package_macos_arm64.sh
```

The script builds the Rust Flutter library, the `service` helper, and the
`p2pdesk-net` c-shared module, places `libp2pdesk_net.dylib` beside the app
executable, and writes an unsigned DMG to `dist/macos-arm64/`. The macOS Rust
loader selects `.dylib` on macOS; Windows and Android names remain unchanged.

Run modes: no args = main window (PeerId display, QR, connection box); `--server` = headless target.
The main process spawns the connection manager (`--cm`) automatically; inbound phone sessions require
it — a missing `sciter.dll` crashes that child and surfaces as "failed to connect to connection manager".

## Go module (Windows + Android)

```bash
# Windows (above) — or:
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
  go build -trimpath -buildmode=c-shared -ldflags='-s -w' -o p2pdesk_net.dll .

# Android arm64 (from p2pdesk-net/):
export CGO_ENABLED=1 GOOS=android GOARCH=arm64
export CC="$NDK/toolchains/llvm/prebuilt/<host>/bin/aarch64-linux-android21-clang"
# SELinux patch: generate the stdlib overlay, then build with it (see below)
python -c "
import json, subprocess, os
goroot = subprocess.check_output(['go','env','GOROOT']).decode('utf-8').strip()
src = os.path.join(goroot, 'src', 'syscall', 'netlink_linux.go')
src_text = open(src, encoding='utf-8').read()
assert '\tif err := Bind(s, sa); err != nil {\n' in src_text, \
    'unexpected syscall/netlink_linux.go layout: re-port _android-overlay/netlink_linux.go for this Go version'
dst = os.path.abspath('_android-overlay/netlink_linux.go')
json.dump({'Replace': {src: dst}}, open('_android-overlay/overlay.json','w', encoding='utf-8'), indent=2)
"
go build -trimpath -buildmode=c-shared -ldflags='-s -w -checklinkname=0' \
  -overlay _android-overlay/overlay.json -o libp2pdesk_net.so .
```

The Android `.so` needs `-checklinkname=0`: the gVisor `wlynxg/anet` dependency reaches into
`net.zoneCache` via linkname, which the Go linker rejects for that target.

The `-overlay` patches one stdlib file: Android 11+ SELinux denies untrusted apps the
netlink socket bind, so every `net.InterfaceAddrs` call fails and libp2p announces
loopback-only addresses. `_android-overlay/netlink_linux.go` is a copy of the Go stdlib file
with the bind removed (kernel auto-assigns the port id on first send; anet uses the same
approach). After a Go toolchain upgrade, regenerate the copy from the new GOROOT file
(`cp $(go env GOROOT)/src/syscall/netlink_linux.go _android-overlay/`), re-remove the
`Bind(s, sa)` call, and keep the header comment; the python assert above fails loudly
until the copy matches the installed toolchain. The leading underscore keeps the Go
toolchain from treating the directory as a package, so `go vet ./...`, `go build ./...`
and `go test ./...` never compile the stdlib copy.

The Android `.so` goes to `flutter/android/app/src/main/jniLibs/arm64-v8a/libp2pdesk_net.so`;
the final APK's `lib/arm64-v8a/` must contain both `librustdesk.so` and `libp2pdesk_net.so`.

## Bootstrap node (bootstrapd)

`p2pdesk-net/cmd/bootstrapd` is the root node of a self-hosted network: a
server-mode kad DHT on a custom protocol prefix plus a circuitv2 relay. It has
no CGO dependency, so it builds and cross-compiles with plain Go:

```bash
# native (Windows/Linux)
cd p2pdesk-net
go build -trimpath -ldflags='-s -w' -o bootstrapd ./cmd/bootstrapd

# cross-compile for a Linux VPS
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' -o bootstrapd-linux-amd64 ./cmd/bootstrapd
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags='-s -w' -o bootstrapd-linux-arm64 ./cmd/bootstrapd
```

Running it and wiring clients into the network is covered in
[docs/NETWORK.md](NETWORK.md).

## Android APK (controller)

```bash
# 0. flutter_rust_bridge codegen — required on a fresh clone (see below)
flutter_rust_bridge_codegen flutter_rust_bridge.yaml

# 1. Rust core cdylib (see the script header for the required env vars).
#    The script wires the NDK toolchain manually — cargo-ndk 3.1.2 mangles
#    paths when cross-compiling from a Windows host. On Linux, cargo-ndk
#    works directly, like upstream:
#    cargo ndk --platform 21 --target aarch64-linux-android build --release --features flutter,hwcodec --lib
bash scripts/build-android.sh --lib

# 2. copy into flutter (next to libp2pdesk_net.so)
cp target/aarch64-linux-android/release/liblibrustdesk.so \
   flutter/android/app/src/main/jniLibs/arm64-v8a/librustdesk.so

# 3. APK — Flutter 3.24.5
flutter build apk --release --target-platform android-arm64
# output: flutter/build/app/outputs/flutter-apk/app-arm64-v8a-release.apk
```

Notes: minSdk 22 (rustls-platform-verifier requirement, set in `flutter/android/app/build.gradle`);
six plugins needed an explicit `namespace` added (pub cache pinned by checksum). Android builds use
`flutter,hwcodec` and the arm64 FFmpeg libraries for H.264/H.265 support.
Build FFmpeg using this repository's overlay (with `ANDROID_NDK_HOME` set):
`vcpkg install ffmpeg:arm64-android --classic --overlay-ports=res/vcpkg`.
The locally patched `libs/hwcodec` dependency calls system MediaCodec on Android;
initialize `libs/hwcodec/externals` from the root Git submodule configuration.
No separate decoder application is installed on the device.
Hardware availability is checked at runtime on both sides. Auto priority is
AV1 → VP9 → H.265 → H.264, followed by VP8 as a compatibility fallback; explicit
codec preferences continue to override Auto when supported.

## UI (index.tis) pre-check

The `inline` feature embeds a byte snapshot of the UI sources (`src/ui/inline.rs`). After
editing the Sciter sources (`src/ui/*.tis`, `*.css`), re-snapshot from the repository root —
otherwise the build embeds stale UI:

```bash
python res/inline-sciter.py
```

## flutter_rust_bridge codegen

Required before any Android APK build: the generated `src/bridge_generated.rs` and
`flutter/lib/generated_bridge.dart` are gitignored (upstream convention), so a fresh clone does
not have them. Copy `flutter_rust_bridge.yaml.example` to `flutter_rust_bridge.yaml`, fill in the
paths, and run `flutter_rust_bridge_codegen flutter_rust_bridge.yaml` (requires libclang for
ffigen). The Windows desktop build does not need this (the flutter feature is off there).

## Logging

Go logs are bridged into the rustdesk log with a `[go]` prefix. libp2p-family churn (identify, DHT
gossip, connmgr chatter) is capped at Error; `P2PDESK_GO_LOG=debug` on the process unlocks full
verbosity. CLI Go tools (`findpeer`) without a registered callback print to stderr as usual.

## Identity / PeerId

The p2p identity (`identity.key`, an ed25519 protobuf) is machine-wide on Windows
(`%ProgramData%\P2PDesk\config\identity.key`) so the service node and a portable GUI share one
PeerId — it must not fork per process account. `P2PDESK_IDENTITY_DIR` points the identity at an
explicit directory (required for same-machine dual-instance tests). Deleting the file mints a new
PeerId on the next start; on Windows the file is owned by whoever created it, so resetting it
needs an admin (or the installed service, which the GUI delegates to).

On Linux/macOS the identity lives in the rustdesk config directory (`~/.rustdesk`) — not yet
machine-scoped, so a root-run service and the user GUI hold separate identities there (known
limitation, the same failure mode the Windows change fixes). Android uses the app-private dir.


## AV1 CQ mode

The software AV1 encoder follows the session's image-quality preset. The
"best" preset runs CQ (quality-first: the bitrate target may be exceeded on
complex scenes), while balanced, fastest and custom-bitrate sessions run CBR
and honour the bitrate target. The Windows Sciter menu item
`Enhancements > AV1 CQ (beta)` overrides that: ticked forces CQ on, unticked
forces it off, and an untouched item follows the preset. A change restarts the
video service, so it takes effect immediately. Network adaptation still uses
the existing ABR option. Both native AV1 builds use libaom 3.15.0.
