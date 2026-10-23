#!/bin/sh
# q2-deezer for the Shanling Q2: static Go for 32-bit little-endian MIPS with soft float, staged with
# its card files in OUT/.deezer and zipped as a release ships it, OUT/q2-deezer-vX.Y-SHA.zip, to unzip
# at the card's root (or for System settings -> Apps to install).
#   contrib/shanlingq2/build.sh [OUT]
# Needs Go 1.22 or later and python3. Its dependencies are vendored: no network.
set -eu
APP=deezer
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
OUT=${1:-$ROOT/target/shanlingq2}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
VERSION=$(sed -n 's/^const version = "q2-\(v[0-9.]*\)"$/\1/p' "$ROOT/cmd/q2-$APP/main.go")
[ -n "$VERSION" ] || { echo "build.sh: no version constant in cmd/q2-$APP/main.go" >&2; exit 1; }
SHA=$(git -C "$ROOT" rev-parse --short=7 HEAD 2>/dev/null || echo local)

R="$OUT/.$APP"
rm -rf "$R"
mkdir -p "$R"
cd "$ROOT"
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat \
    go build -trimpath -ldflags='-s -w -buildid=' -o "$R/q2-$APP" "./cmd/q2-$APP"
cp "$HERE/launcher.sh" "$R/run"
cp "$HERE/q2app" "$R/q2app"
chmod 755 "$R/q2-$APP" "$R/run"
chmod 644 "$R/q2app"
rm -f "$OUT"/q2-$APP-*.zip
python3 -I "$HERE/package.py" "$R" "$OUT/q2-$APP-$VERSION-$SHA.zip"
