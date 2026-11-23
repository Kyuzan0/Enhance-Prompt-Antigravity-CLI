#!/usr/bin/env bash
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
version=$(node -p 'require("./package.json").version')
name="agy-enhance-linux-$version"
staging=$(mktemp -d)
trap 'rm -rf -- "$staging"; if [[ -n ${temporary:-} ]]; then rm -f -- "$temporary"; fi' EXIT
temporary=$(mktemp '.agy-enhance-linux-XXXXXX.tar.gz')

runtime_files=(wrapper/linux/enhance.mjs \
    core-engine/prompt-enhancer.mjs contracts/strict-deny-policy.json \
    README.md LICENSE CHANGELOG.md)
for file in "${runtime_files[@]}"; do
    install -D -m 0644 -- "$file" "$staging/$name/$file"
done
launcher=wrapper/linux/agy-enhance
install -D -m 0755 -- "$launcher" "$staging/$name/$launcher"

tar --create --gzip --file "$temporary" --owner=0 --group=0 --numeric-owner \
    --directory "$staging" -- "$name"
mv -f -- "$temporary" "$name.tar.gz"
sha256sum -- "$name.tar.gz"
