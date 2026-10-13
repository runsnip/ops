#!/bin/sh
# scripts/release.sh — build a runsnip release: Linux binaries for amd64 and arm64, SHA256SUMS, install.sh and the
# notes, into dist/<tag>/. With --publish it also creates the GitHub release (gh) from them.
#
#   scripts/release.sh v0.3.0                 build and test only (dist/v0.3.0/)
#   scripts/release.sh v0.3.0 --publish       …then gh release create v0.3.0 with those files
#
# The binaries are static (CGO_ENABLED=0), stripped and reproducible (-trimpath, no build id), and carry the tag as
# their version (runsnip --version). dist/ is never committed.
set -eu
cd "$(dirname "$0")/.."

TAG="${1:-}"
PUBLISH=0
[ "${2:-}" = "--publish" ] && PUBLISH=1
case "$TAG" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "usage: scripts/release.sh vX.Y.Z [--publish]" >&2; exit 2 ;;
esac

OUT="dist/$TAG"
rm -rf "$OUT"
mkdir -p "$OUT"

echo "› go vet and go test"
go vet ./...
go test -count=1 ./... >/dev/null

for arch in amd64 arm64; do
  echo "› runsnip-linux-$arch"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -X main.version=$TAG" -o "$OUT/runsnip-linux-$arch" .
done
cp install.sh "$OUT/install.sh"
(cd "$OUT" && sha256sum runsnip-linux-amd64 runsnip-linux-arm64 install.sh > SHA256SUMS 2>/dev/null ||
  shasum -a 256 runsnip-linux-amd64 runsnip-linux-arm64 install.sh > SHA256SUMS)

PREV="$(git describe --tags --abbrev=0 2>/dev/null || true)"
{
  echo "## runsnip $TAG"
  echo
  if [ -n "$PREV" ]; then
    echo "Changes since $PREV:"
    echo
    git log --no-merges --format='- %s' "$PREV..HEAD"
  else
    git log --no-merges --format='- %s' -n 30
  fi
  echo
  echo "Install or update on a server: \`curl -fsSL https://github.com/runsnip/ops/releases/download/$TAG/install.sh | sudo sh -s -- --version $TAG\`"
  echo "or, where runsnip runs already: \`sudo runsnip self update --version $TAG\`."
} > "$OUT/NOTES.md"

ls -l "$OUT"
cat "$OUT/SHA256SUMS"

if [ "$PUBLISH" = 1 ]; then
  [ -z "$(git status --porcelain)" ] || { echo "the tree is not clean: commit first" >&2; exit 1; }
  git tag -a "$TAG" -m "runsnip $TAG"
  git push origin "$TAG"
  gh release create "$TAG" --repo runsnip/ops --title "runsnip $TAG" --notes-file "$OUT/NOTES.md" \
    "$OUT/runsnip-linux-amd64" "$OUT/runsnip-linux-arm64" "$OUT/install.sh" "$OUT/SHA256SUMS"
fi
