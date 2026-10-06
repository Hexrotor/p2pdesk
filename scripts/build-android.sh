#!/bin/bash
# Cross-compile the rustdesk Android cdylib (arm64) from a Windows host.
#
# Manual env setup instead of cargo-ndk (3.1.2 has two Windows-host bugs):
# openssl's make mangles the backslashed CC_ path, and the linker wrapper
# drops quoted arguments when rebuilding the command line. So CC/CXX/AR/
# RANLIB point directly at the NDK tools (the cc crate spawns them and adds
# --target itself) and only the rustc linker stays on the cargo-ndk wrapper
# (the link line has no quoted args, so the bug never triggers).
#
# Required environment:
#   NDK          Android NDK root (e.g. ~/android-ndk-r26d)
#   VCPKG_ROOT   vcpkg with libvpx/libyuv/opus/aom/ffmpeg (arm64-android)
#   OPENSSL_DIR  prebuilt OpenSSL for android-arm64 (openssl-sys skips its
#                source build when OPENSSL_DIR is set)
# Optional:
#   SODIUM_LIB_DIR       dir holding libsodium.lib (MSVC host) + liblibsodium.a;
#                        required on Windows hosts — libsodium-sys has no
#                        Windows source build and panics without it (Linux
#                        hosts build it from source)
#   WRAP                 cargo-ndk wrapper dir (default target/.cargo-ndk-3.1.2;
#                        generate it once by running any `cargo ndk` command)
#   HTTP_PROXY/HTTPS_PROXY honored if set
#
# Usage: bash scripts/build-android.sh [--lib]   (--lib builds the cdylib only)
set -e

: "${NDK:?set NDK to your Android NDK root}"
: "${VCPKG_ROOT:?set VCPKG_ROOT to a vcpkg checkout with libvpx/libyuv/opus/aom/ffmpeg}"
: "${OPENSSL_DIR:?set OPENSSL_DIR to a prebuilt OpenSSL for android-arm64}"

if [ -d "$NDK/toolchains/llvm/prebuilt/windows-x86_64" ]; then
    HOST=windows-x86_64
    SUF=.exe
    CLANG_LINKER=aarch64-linux-android21-clang.cmd
elif [ -d "$NDK/toolchains/llvm/prebuilt/linux-x86_64" ]; then
    HOST=linux-x86_64
    SUF=""
    CLANG_LINKER=aarch64-linux-android21-clang
else
    echo "no known prebuilt toolchain under $NDK/toolchains/llvm/prebuilt" >&2
    exit 1
fi
CLANG_BIN=$NDK/toolchains/llvm/prebuilt/$HOST/bin
SYSROOT=$NDK/toolchains/llvm/prebuilt/$HOST/sysroot

# scrap's build.rs needs msys2 make/sh on a Windows host; vcpkg ships one.
# A no-match glob keeps the literal pattern, so this is a no-op elsewhere.
for d in "$VCPKG_ROOT"/downloads/tools/msys2/*/usr/bin; do
    [ -d "$d" ] && PATH="$d:$PATH"
done

export CC_aarch64_linux_android=$CLANG_BIN/clang$SUF
export CXX_aarch64_linux_android=$CLANG_BIN/clang++$SUF
export AR_aarch64_linux_android=$CLANG_BIN/llvm-ar$SUF
export RANLIB_aarch64_linux_android=$CLANG_BIN/llvm-ranlib$SUF
WRAP=${WRAP:-$(pwd)/target/.cargo-ndk-3.1.2}
# git-bash $(pwd) is a POSIX path; rustc on Windows resolves that as a
# drive-relative path and cannot find the linker. Convert only on the
# Windows host — the Linux path is untouched.
if [ "$HOST" = "windows-x86_64" ]; then
    WRAP=$(cygpath -m "$WRAP")
fi
export CARGO_TARGET_AARCH64_LINUX_ANDROID_LINKER=$WRAP/triple-linker$SUF
export CARGO_TARGET_AARCH64_LINUX_ANDROID_AR=$WRAP/triple-ar$SUF
export CARGO_NDK_TRIPLE_LINKER=$CLANG_BIN/$CLANG_LINKER
export CARGO_NDK_TRIPLE_AR=$CLANG_BIN/llvm-ar$SUF
export BINDGEN_EXTRA_CLANG_ARGS_aarch64_linux_android="--sysroot=$SYSROOT -I$SYSROOT/usr/include/aarch64-linux-android"
if [ -n "${SODIUM_LIB_DIR:-}" ]; then
    export SODIUM_LIB_DIR
fi

EXTRA=""
[ "${1:-}" = "--lib" ] && EXTRA="--lib"
cargo build --target aarch64-linux-android --release --features flutter,hwcodec $EXTRA
