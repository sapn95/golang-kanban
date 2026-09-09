#!/bin/sh
# Compile assets/tailwind.css from the classes used in the templates and in
# app.js. Run it after adding a class that was not on a page before:
#
#   go generate ./assets/        # or: sh assets/tailwind.sh
#
# CI runs the same script and fails if the result differs from what is
# committed, so a class that reaches a page without reaching the stylesheet is
# a red build rather than an element with no styling. The compiler is not
# needed to build or run the app; the output is committed.
#
# The compiler is Tailwind's standalone CLI: one binary per platform, no Node
# installation. It is downloaded on first use, kept in .tools/, and checked
# against the digest below, because it is a binary from the internet that then
# writes a file we serve.
set -eu

VERSION=3.4.17

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)  asset=tailwindcss-macos-arm64
                 want=a1d0c7985759accca0bf12e51ac1dcbf0f6cf2fffb62e6e0f62d091c477a10a3 ;;
  Darwin-x86_64) asset=tailwindcss-macos-x64
                 want=6cbdad74be776c087ffa5e9a057512c54898f9fe8828d3362212dfe32fc933a3 ;;
  Linux-x86_64)  asset=tailwindcss-linux-x64
                 want=7d24f7fa191d2193b78cd5f5a42a6093e14409521908529f42d80b11fde1f1d4 ;;
  Linux-aarch64) asset=tailwindcss-linux-arm64
                 want=69b1378b8133192d7d2feb12a116fa12d035594f58db3eff215879e4ad8cf39b ;;
  *) echo "no pinned tailwindcss $VERSION for $(uname -s)-$(uname -m)." >&2
     echo "add its asset name and sha256 from the release below to this script:" >&2
     echo "https://github.com/tailwindlabs/tailwindcss/releases/tag/v$VERSION" >&2
     exit 1 ;;
esac

# The repository root, so the content globs in tailwind.config.js mean the same
# thing however the script was called. Tailwind resolves them against the
# working directory, not against the config file.
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

cli=".tools/$asset-$VERSION"
if [ ! -x "$cli" ]; then
  mkdir -p .tools
  echo "fetching tailwindcss $VERSION ($asset)"
  curl -fsSL -o "$cli.part" \
    "https://github.com/tailwindlabs/tailwindcss/releases/download/v$VERSION/$asset"
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$cli.part" | cut -d' ' -f1)
  else
    got=$(shasum -a 256 "$cli.part" | cut -d' ' -f1)
  fi
  if [ "$got" != "$want" ]; then
    rm -f "$cli.part"
    echo "tailwindcss $asset: sha256 $got, expected $want" >&2
    exit 1
  fi
  chmod +x "$cli.part"
  mv "$cli.part" "$cli"
fi

# Not --minify: the diff of this file is the review of what a new class added,
# and gzip on the wire takes most of what minifying would.
"./$cli" \
  --config assets/tailwind.config.js \
  --input assets/tailwind.input.css \
  --output assets/tailwind.css
