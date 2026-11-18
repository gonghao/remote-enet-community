#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
output_dir=${1:-"$repo_dir/dist"}
if [ -n "${RENET_BUILD_VERSION:-}" ]; then
    exec python3 "$repo_dir/tools/build-desktop.py" --platform darwin --role client --output "$output_dir" --version "$RENET_BUILD_VERSION"
fi
exec python3 "$repo_dir/tools/build-desktop.py" --platform darwin --role client --output "$output_dir"
