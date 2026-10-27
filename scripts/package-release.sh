#!/usr/bin/env bash
# Builds plugin-store release archives into dist/: keeper-usage-push_<version>_<goos>_<goarch>.zip,
# the offline spool operator keeper-push-spool_<version>_<goos>_<goarch>.zip and checksums.txt.
# Linux targets are built in manylinux2014 containers so both binaries need only GLIBC 2.17,
# like official CPA Linux archives. darwin targets build natively on macOS.
# Windows builds use MinGW-w64 GCC (or an explicitly supplied cross-compiler CC).
#
# Usage: scripts/package-release.sh <version> [target ...]
#   targets: linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64 (default: both Linux targets)
# Environment: GOPROXY (passed to builds), GO_VERSION (default: from go.mod), CC (Windows compiler).
set -euo pipefail

cd "$(dirname "$0")/.."
VERSION="${1:?usage: scripts/package-release.sh <version> [target ...]}"
VERSION="${VERSION#v}"
shift
TARGETS=("$@")
[ ${#TARGETS[@]} -gt 0 ] || TARGETS=(linux/amd64 linux/arm64)
GO_VERSION="${GO_VERSION:-$(sed -n 's/^go //p' go.mod)}"
GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}"
LDFLAGS="-s -w -X main.Version=${VERSION}"
DIST=dist
mkdir -p "$DIST"

build_linux() {
  local arch="$1" image go_arch
  case "$arch" in
    amd64) image=quay.io/pypa/manylinux2014_x86_64; go_arch=amd64 ;;
    arm64) image=quay.io/pypa/manylinux2014_aarch64; go_arch=arm64 ;;
    *) echo "unsupported linux arch $arch" >&2; exit 1 ;;
  esac
  docker run --rm --platform "linux/${arch}" \
    -v "$PWD:/src" -w /src \
    -e GO_VERSION="$GO_VERSION" -e GOPROXY="$GOPROXY" -e LDFLAGS="$LDFLAGS" -e GOARCH_TARGET="$go_arch" \
    -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
    "$image" bash -euo pipefail -c '
      # The checkout belongs to the runner user; Go reads its Git metadata as container root.
      git config --global --add safe.directory /src
      curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH_TARGET}.tar.gz" -o /tmp/go.tgz
      tar -C /usr/local -xzf /tmp/go.tgz
      export PATH="/usr/local/go/bin:$PATH" GOFLAGS=-mod=readonly GOTOOLCHAIN=local
      out="dist/build/linux_${GOARCH_TARGET}"
      mkdir -p "$out"
      CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags="$LDFLAGS" -o "$out/keeper-usage-push.so" .
      CGO_ENABLED=1 go build -trimpath -ldflags="$LDFLAGS" -o "$out/keeper-push-spool" ./cmd/spool
      rm -f "$out/keeper-usage-push.h"
      for binary in keeper-usage-push.so keeper-push-spool; do
        max_glibc="$(readelf --version-info "$out/$binary" | sed -n "s/.*Name: GLIBC_\([0-9.]*\).*/\1/p" | sort -V | tail -n 1)"
        if [ -n "$max_glibc" ] && [ "$(printf "2.17\n%s\n" "$max_glibc" | sort -V | tail -n 1)" != "2.17" ]; then
          echo "$binary requires GLIBC_${max_glibc}, expected 2.17 or older" >&2; exit 1
        fi
      done
      gcc -Wall -Wextra verify/native.c -ldl -o /tmp/verify-native
      /tmp/verify-native "$out/keeper-usage-push.so"
      chown -R "$HOST_UID:$HOST_GID" dist
    '
}

build_darwin() {
  local arch="$1" out="dist/build/darwin_$1"
  mkdir -p "$out"
  local cc=(clang)
  [ "$arch" = "$(go env GOHOSTARCH)" ] || cc=(clang -arch "$([ "$arch" = amd64 ] && echo x86_64 || echo arm64)")
  CC="${cc[*]}" CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" GOFLAGS=-mod=readonly \
    go build -trimpath -buildmode=c-shared -ldflags="$LDFLAGS" -o "$out/keeper-usage-push.dylib" .
  CC="${cc[*]}" CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" GOFLAGS=-mod=readonly \
    go build -trimpath -ldflags="$LDFLAGS" -o "$out/keeper-push-spool" ./cmd/spool
  rm -f "$out/keeper-usage-push.h"
  if [ "$arch" = "$(go env GOHOSTARCH)" ]; then
    cc -Wall -Wextra verify/native.c -o "$out/verify-native"
    "$out/verify-native" "$out/keeper-usage-push.dylib"
    rm -f "$out/verify-native"
  fi
}

build_windows() {
  local arch="$1" out="dist/build/windows_$1" compiler="${CC:-gcc}"
  [ "$arch" = amd64 ] || { echo "unsupported windows arch $arch" >&2; exit 1; }
  if [ "$(go env GOHOSTOS)" != windows ] && [ -z "${CC:-}" ]; then
    compiler=x86_64-w64-mingw32-gcc
  fi
  mkdir -p "$out"
  CC="$compiler" CGO_ENABLED=1 GOOS=windows GOARCH="$arch" GOFLAGS=-mod=readonly \
    go build -trimpath -buildmode=c-shared -ldflags="$LDFLAGS -extldflags=-static-libgcc" -o "$out/keeper-usage-push.dll" .
  CC="$compiler" CGO_ENABLED=1 GOOS=windows GOARCH="$arch" GOFLAGS=-mod=readonly \
    go build -trimpath -ldflags="$LDFLAGS -extldflags=-static-libgcc" -o "$out/keeper-push-spool.exe" ./cmd/spool
  rm -f "$out/keeper-usage-push.h"
  if [ "$(go env GOHOSTOS)" = windows ]; then
    # CC may contain arguments, just like Go's CC setting.
    $compiler -Wall -Wextra verify/native.c -o "$out/verify-native.exe"
    "$out/verify-native.exe" "$out/keeper-usage-push.dll"
    rm -f "$out/verify-native.exe"
  fi
}

for target in "${TARGETS[@]}"; do
  goos="${target%/*}" goarch="${target#*/}"
  tool_suffix=""
  echo "==> ${goos}/${goarch}"
  case "$goos" in
    linux) build_linux "$goarch"; lib="dist/build/linux_${goarch}/keeper-usage-push.so" ;;
    darwin) build_darwin "$goarch"; lib="dist/build/darwin_${goarch}/keeper-usage-push.dylib" ;;
    windows) build_windows "$goarch"; lib="dist/build/windows_${goarch}/keeper-usage-push.dll"; tool_suffix=.exe ;;
    *) echo "unsupported target $target" >&2; exit 1 ;;
  esac
  go run ./cmd/packager -lib "$lib" -tool "$(dirname "$lib")/keeper-push-spool${tool_suffix}" \
    -version "$VERSION" -goos "$goos" -goarch "$goarch" -out "$DIST"
done

echo "==> ${DIST}/"
ls -l "$DIST"/*.zip
cat "$DIST/checksums.txt"
