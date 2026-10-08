#!/usr/bin/env bash
# Cross-compile the supported platforms without requiring their toolchains.
set -euo pipefail

version=${1:?usage: build-release.sh VERSION COMMIT OUTPUT_DIR}
commit=${2:?missing commit}
output=${3:?missing output directory}
mkdir -p "$output"
staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT

for platform in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
  os=${platform%/*}
  arch=${platform#*/}
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
    -trimpath -ldflags "-s -w -X main.buildVersion=$version -X main.buildCommit=$commit" \
    -o "$staging/q" ./cmd/q
  cp LICENSE README.md "$staging/"
  COPYFILE_DISABLE=1 tar -czf "$output/q_${version}_${os}_${arch}.tar.gz" -C "$staging" q LICENSE README.md
done

# Python is available on GitHub-hosted runners and works on macOS too.
python3 - "$output" <<'PY'
import hashlib
import pathlib
import sys

directory = pathlib.Path(sys.argv[1])
with (directory / "checksums.txt").open("w") as checksums:
    for archive in sorted(directory.glob("*.tar.gz")):
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        checksums.write(f"{digest}  {archive.name}\n")
PY
