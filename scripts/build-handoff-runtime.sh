#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 1 ]]; then
    echo "Usage: bash $0 OUTPUT.tar.gz" >&2
    exit 2
fi
output=$(realpath -m -- "$1")
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

node_bin=$(command -v node)
if ! node_deps=$(env -u LD_LIBRARY_PATH -u LD_PRELOAD -u LD_AUDIT LC_ALL=C ldd "$node_bin"); then
    printf 'Cannot verify shared libraries for %s. Use upstream Linux x64 Node.js.\n' "$node_bin" >&2
    exit 1
fi
extra_deps=$(awk '
    $1 == "linux-vdso.so.1" { next }
    $1 ~ /^\/(usr\/)?lib(64)?\/.*ld-linux-x86-64\.so\.2$/ { next }
    $1 ~ /^(libc\.so\.6|libm\.so\.6|libdl\.so\.2|libpthread\.so\.0|librt\.so\.1|libgcc_s\.so\.1|libstdc\+\+\.so\.6)$/ &&
        $2 == "=>" && $3 ~ /^\/(usr\/)?lib(64)?\// { next }
    { print }
' <<< "$node_deps")
if [[ -n $extra_deps ]]; then
    printf 'Node.js at %s requires shared libraries outside the base system:\n%s\nUse upstream Linux x64 Node.js.\n' "$node_bin" "$extra_deps" >&2
    exit 1
fi

mkdir -p .tmp
work=$(mktemp -d "$PWD/.tmp/handoff-runtime.XXXXXX")
trap 'chmod -R u+w -- "$work"; rm -rf -- "$work"' EXIT
runtime=$work/runtime
mkdir -p "$runtime/cache/npm" "$work/home"

cp -a -- "$(go env GOROOT)" "$runtime/go"
find "$runtime/go" -type d -exec chmod u+w {} +
node_root=$(cd -- "$(dirname -- "$node_bin")/.." && pwd -P)
npm_cli=$(realpath -- "$(command -v npm)")
npm_root=$(dirname -- "$(dirname -- "$npm_cli")")
mkdir -p "$runtime/node/bin" "$runtime/node/lib/node_modules"
cp -aL -- "$node_bin" "$runtime/node/bin/node"
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
