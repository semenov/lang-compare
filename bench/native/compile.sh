#!/bin/bash
# Native (macOS) compile-time benchmark. Sources are copied to /tmp so caches start cold.
# TS/Go: 3 runs each; Rust: 1 run (clean release builds take minutes). -> results/native/compile.txt
set -euo pipefail
cd "$(dirname "$0")/../.."
export PATH=~/.nvm/versions/node/v26.10.0/bin:$PATH
OUT=$PWD/results/native/compile.txt; mkdir -p results/native; : > "$OUT"
W=/tmp/lc-compile
t() { local s e; s=$(python3 -c 'import time;print(time.time())'); "$@" >/dev/null 2>&1 || { echo "FAILED: $*" >&2; exit 1; }
      e=$(python3 -c 'import time;print(time.time())'); python3 -c "print(int(($e-$s)*1000))"; }
log() { echo "$1 $2" | tee -a "$OUT"; }
fresh() { [ -d $W ] && chmod -R u+w $W; rm -rf $W; mkdir -p $W; cp -R "$@" $W/; cd $W; }

echo "# $(date) $(sysctl -n machdep.cpu.brand_string), node $(node -v), $(go version | cut -d' ' -f3), $(rustc -V | cut -d' ' -f2)" | tee -a "$OUT"
for i in 1 2 3; do
  (fresh ts/package.json ts/package-lock.json ts/tsconfig.json ts/src
   log ts_deps_install_ms "$(t npm ci --no-audit --no-fund)"
   log ts_clean_build_ms "$(t npx tsc -p .)"
   echo "// change" >> src/app.ts
   log ts_rebuild_after_edit_ms "$(t npx tsc -p .)"
   log ts_typecheck_ms "$(t npx tsc -p . --noEmit)"
   npm i -q --no-save typescript@5.9 >/dev/null 2>&1
   log ts59_clean_build_ms "$(t npx tsc -p .)")
  (fresh go/go.mod go/go.sum go/*.go
   export GOCACHE=$W/gocache GOMODCACHE=$W/gomod CGO_ENABLED=0
   log go_deps_download_ms "$(t go mod download)"
   log go_clean_build_ms "$(t go build -trimpath -ldflags='-s -w' -o app .)"
   echo "// change" >> handlers.go
   log go_rebuild_after_edit_ms "$(t go build -trimpath -ldflags='-s -w' -o app .)"
   log go_vet_ms "$(t go vet ./...)")
done
(fresh rust/Cargo.toml rust/Cargo.lock rust/src
 export CARGO_TARGET_DIR=$W/target
 log rust_deps_fetch_ms "$(t cargo fetch)"
 log rust_clean_check_ms "$(t cargo check)"
 echo "// c" >> src/handlers.rs
 log rust_check_after_edit_ms "$(t cargo check)"
 log rust_clean_debug_build_ms "$(t cargo build)"
 echo "// c" >> src/handlers.rs
 log rust_debug_rebuild_after_edit_ms "$(t cargo build)"
 log rust_clean_release_lto_ms "$(t cargo build --release)"
 echo "// c" >> src/handlers.rs
 log rust_release_lto_rebuild_after_edit_ms "$(t cargo build --release)"
 export CARGO_PROFILE_RELEASE_LTO=false CARGO_TARGET_DIR=$W/target-nolto
 log rust_clean_release_nolto_ms "$(t cargo build --release)"
 echo "// c" >> src/handlers.rs
 log rust_release_nolto_rebuild_after_edit_ms "$(t cargo build --release)")
chmod -R u+w $W; rm -rf $W
