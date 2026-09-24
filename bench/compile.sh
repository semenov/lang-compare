#!/bin/bash
# Measures build times inside Linux containers (same VM, all CPUs). Source is copied
# into the container first so bind-mount I/O doesn't skew results.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=results/compile.txt
: > "$OUT"
T='t() { s=$(cut -d" " -f1 /proc/uptime); "$@" >/dev/null 2>&1 || { echo "FAILED: $*"; exit 1; }; e=$(cut -d" " -f1 /proc/uptime); awk "BEGIN{print int(($e-$s)*1000)}"; }'

run() { # name image script
  echo "== $1" | tee -a "$OUT"
  docker run --rm -v "$PWD/$1":/mnt:ro "$2" sh -c "$T; $3" | tee -a "$OUT"
}

for i in 1 2 3; do
run ts node:24-alpine '
  mkdir /w && cd /w && cp -r /mnt/package.json /mnt/package-lock.json /mnt/tsconfig.json /mnt/src .
  echo "deps_install_ms $(t npm ci)"
  echo "clean_build_ms $(t npx tsc -p .)"
  echo "// change" >> src/app.ts
  echo "rebuild_after_edit_ms $(t npx tsc -p .)"
  echo "typecheck_only_ms $(t npx tsc -p . --noEmit)"'

run go golang:1.27-alpine '
  mkdir /w && cd /w && cp -r /mnt/go.mod /mnt/go.sum /mnt/*.go .
  export CGO_ENABLED=0
  echo "deps_download_ms $(t go mod download)"
  echo "clean_build_ms $(t go build -trimpath -ldflags=-s\ -w -o app .)"
  echo "// change" >> handlers.go
  echo "rebuild_after_edit_ms $(t go build -trimpath -ldflags=-s\ -w -o app .)"
  echo "vet_ms $(t go vet ./...)"'
done

run rust rust:1-alpine '
  apk add -q musl-dev >/dev/null 2>&1
  mkdir /w && cd /w && cp -r /mnt/Cargo.toml /mnt/Cargo.lock /mnt/src .
  echo "deps_download_ms $(t cargo fetch)"
  echo "clean_debug_build_ms $(t cargo build)"
  echo "// change" >> src/handlers.rs
  echo "rebuild_debug_after_edit_ms $(t cargo build)"
  echo "clean_release_build_ms $(t cargo build --release)"
  echo "// change2" >> src/handlers.rs
  echo "rebuild_release_after_edit_ms $(t cargo build --release)"
  echo "// change3" >> src/handlers.rs
  echo "check_after_edit_ms $(t cargo check)"'
run rust rust:1-alpine '
  apk add -q musl-dev >/dev/null 2>&1
  mkdir /w && cd /w && cp -r /mnt/Cargo.toml /mnt/Cargo.lock /mnt/src .
  cargo fetch >/dev/null 2>&1
  echo "clean_release_build_ms $(t cargo build --release)"
  echo "// change2" >> src/handlers.rs
  echo "rebuild_release_after_edit_ms $(t cargo build --release)"'
