#!/usr/bin/env sh
set -eu

# VektorWeb Cross-Platform Build Script
# - Builds native host client + relay by default (./build.sh)
# - Pass "release" (./build.sh release) to cross-compile vektorweb-relay across
#   Linux, FreeBSD, OpenBSD, macOS, and Windows (amd64 + arm64) and cross-compile
#   vektorweb-client.exe for Windows if x86_64-w64-mingw32-gcc is installed.

MODE="${1:-local}"
OUT_DIR="bin"
mkdir -p "$OUT_DIR"

HOST_OS="$(go env GOOS)"
HOST_ARCH="$(go env GOARCH)"

echo "==> Tidying Go modules..."
go mod tidy

build_relay() {
    os="$1"
    arch="$2"
    ext=""
    if [ "$os" = "windows" ]; then
        ext=".exe"
    fi
    out="$OUT_DIR/vektorweb-relay-${os}-${arch}${ext}"
    if [ "$os" = "$HOST_OS" ] && [ "$arch" = "$HOST_ARCH" ] && [ "$MODE" = "local" ]; then
        out="$OUT_DIR/vektorweb-relay${ext}"
    fi
    echo " -> Building $out (CGO_ENABLED=0)"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "$out" ./cmd/vektorweb-relay
}

build_native_client() {
    ext=""
    if [ "$HOST_OS" = "windows" ]; then
        ext=".exe"
    fi
    out="$OUT_DIR/vektorweb-client${ext}"
    echo " -> Building native $out (${HOST_OS}/${HOST_ARCH})"
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o "$out" ./cmd/vektorweb-client
}

build_windows_client() {
    if command -v x86_64-w64-mingw32-gcc >/dev/null 2>&1; then
        out="$OUT_DIR/vektorweb-client-windows-amd64.exe"
        echo " -> Cross-compiling $out via MinGW"
        CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
            go build -trimpath -ldflags="-s -w -H=windowsgui" -o "$out" ./cmd/vektorweb-client
    else
        echo " -> Skipping Windows client cross-compile (x86_64-w64-mingw32-gcc not found)"
    fi
}

if [ "$MODE" = "release" ]; then
    echo "==> Building VektorWeb multi-platform release artifacts..."
    # Pure-Go relay targets (Linux, FreeBSD, OpenBSD, macOS, Windows)
    build_relay linux amd64
    build_relay linux arm64
    build_relay freebsd amd64
    build_relay freebsd arm64
    build_relay openbsd amd64
    build_relay openbsd arm64
    build_relay darwin amd64
    build_relay darwin arm64
    build_relay windows amd64

    # Native client for current OS + optional Windows .exe cross-build
    build_native_client
    if [ "$HOST_OS" != "windows" ]; then
        build_windows_client
    fi
else
    echo "==> Building native VektorWeb binaries (${HOST_OS}/${HOST_ARCH})..."
    build_relay "$HOST_OS" "$HOST_ARCH"
    build_native_client
fi

echo "==> Done! Artifacts in ./$OUT_DIR/:"
ls -lh "$OUT_DIR"
