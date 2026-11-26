#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 1 ]]; then
    echo "Usage: bash $0 OUTPUT.tar.gz" >&2
    exit 2
fi
output=$(realpath -m -- "$1")
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

mkdir -p .tmp
work=$(mktemp -d "$PWD/.tmp/handoff-runtime.XXXXXX")
trap 'chmod -R u+w -- "$work"; rm -rf -- "$work"' EXIT
runtime=$work/runtime
mkdir -p "$runtime/cache/npm" "$work/home"

cp -a -- "$(go env GOROOT)" "$runtime/go"
find "$runtime/go" -type d -exec chmod u+w {} +
node_root=$(cd -- "$(dirname -- "$(command -v node)")/.." && pwd -P)
npm_cli=$(realpath -- "$(command -v npm)")
npm_root=$(dirname -- "$(dirname -- "$npm_cli")")
mkdir -p "$runtime/node/bin" "$runtime/node/lib/node_modules"
cp -aL -- "$node_root/bin/node" "$runtime/node/bin/"
if [[ $npm_root == "$node_root/lib/node_modules/npm" ]]; then
    cp -a -- "$npm_root" "$runtime/node/lib/node_modules/npm"
else
    npm_archive=$(npm pack "npm@$(npm --version)" --silent --cache "$work/npm-pack-cache" --pack-destination "$work")
    mkdir "$runtime/node/lib/node_modules/npm"
    tar -xzf "$work/$npm_archive" -C "$runtime/node/lib/node_modules/npm" --strip-components=1
fi
ln -s ../lib/node_modules/npm/bin/npm-cli.js "$runtime/node/bin/npm"
ln -s ../lib/node_modules/npm/bin/npx-cli.js "$runtime/node/bin/npx"
node_license="$node_root/LICENSE"
if [[ ! -f $node_license ]]; then
    node_license="$node_root/share/doc/nodejs/copyright"
fi
cp -- "$node_license" "$runtime/node/LICENSE"

env -i PATH="$runtime/go/bin:$runtime/node/bin:/usr/bin:/bin" \
    HOME="$work/home" GOTOOLCHAIN=local CGO_ENABLED=1 GOFLAGS=-modcacherw GOPATH="$work/gopath" \
    GOMODCACHE="$work/go-mod" GOCACHE="$work/go-build" \
    npm_config_cache="$runtime/cache/npm" npm_config_update_notifier=false \
    make check >&2
mv -- "$work/go-mod/cache/download" "$runtime/cache/go-proxy"
find "$runtime/cache/go-proxy" -name '*.lock' -delete
rm -rf -- "$runtime/cache/npm/_logs"

LC_ALL=C tar --sort=name --format=gnu --mtime=@0 --numeric-owner \
    --owner=0 --group=0 --hard-dereference -C "$runtime" -cf - cache go node \
    | gzip -n > "$work/runtime.tar.gz"
mv -- "$work/runtime.tar.gz" "$output"
