#!/usr/bin/env bash
# Tests the s3client binary against a local RustFS server (bucket: test-bucket).
#
# NO ARGUMENTS  -> test suite (14 checks)
#   ./tests/run-tests.sh                 # 200 MB test file
#   BIG_MB=1100 ./tests/run-tests.sh     # >1 GB test file
#   CLEANUP=1 ./tests/run-tests.sh       # stop RustFS and DELETE its data afterwards
#
# WITH ARGUMENTS -> your own files, real s3client output (live progress bar), then an independent check
#   ./tests/run-tests.sh big.iso notes.txt                 # upload, then verify the objects are on the server
#   ./tests/run-tests.sh -prefix demo/ -parallel 2 a.bin   # any s3client flag
#   ./tests/run-tests.sh download demo/a.bin               # download into tests/downloads, then verify it
#   ./tests/run-tests.sh download -out ./elsewhere demo/a.bin
#   ./tests/run-tests.sh help
#
# RustFS is started if needed and LEFT RUNNING, with its data kept in the docker volume "tests_rustfs-data",
# so you can look at /data in the container. Stop it with
#   docker compose -f tests/docker-compose.yml --profile tools down      (keep the data)
#   docker compose -f tests/docker-compose.yml --profile tools down -v   (delete the data)
#
# Container mode (IN_COMPOSE=1) is used by `docker compose ... run tester`; see tests/README.md.
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(dirname "$HERE")"
WORK="${WORK:-$HERE/.work}"
DOWNLOADS="$HERE/downloads"
COMPOSE="$HERE/docker-compose.yml"
RESULTS="$HERE/results"
BIN="$WORK/s3client"
BIG_MB="${BIG_MB:-200}"
S3_HOST="${S3_HOST:-localhost}"
IN_COMPOSE="${IN_COMPOSE:-}"
CONTAINER=s3client-test-rustfs
GOOD_ENV="$WORK/good.env"
MANIFEST="$WORK/uploaded.tsv"        # key <TAB> sha256 <TAB> size   (what we uploaded, for the download check)

PASS=0; FAIL=0; LOG=()
check() { # name, ok(0 = pass), detail
  if [ "$2" = 0 ]; then PASS=$((PASS+1)); line="PASS  $1"; echo -e "\033[32m$line\033[0m"
  else FAIL=$((FAIL+1)); line="FAIL  $1  ${3:-}"; echo -e "\033[31m$line\033[0m"; fi
  LOG+=("$line")
}
sha() { sha256sum "$1" | cut -d' ' -f1; }
fsize() { wc -c < "$1" | tr -d ' '; }
run() { OUT="$("$BIN" "$@" 2>&1)"; CODE=$?; }   # sets OUT and CODE
has() { echo "$OUT" | grep -q -- "$1"; }
say() { echo -e "\033[36m$*\033[0m" >&2; }

command -v go   >/dev/null || { echo "'go' not found in PATH (see tests/README.md)"; exit 2; }
command -v curl >/dev/null || { echo "'curl' (7.75+) not found in PATH (see tests/README.md)"; exit 2; }
if [ -z "$IN_COMPOSE" ]; then
  command -v docker >/dev/null || { echo "'docker' not found in PATH (see tests/README.md)"; exit 2; }
  docker info >/dev/null 2>&1 || { echo "Docker daemon is not running / not accessible."; exit 2; }
fi
mkdir -p "$WORK"

# ---- build + config ---------------------------------------------------------
say "== Building s3client ($(go env GOOS)/$(go env GOARCH))"
(cd "$ROOT" && go build -o "$BIN" .) || { echo "go build failed"; exit 2; }
sed "s#localhost:9000#${S3_HOST}:9000#" "$HERE/test.env" > "$GOOD_ENV"
envval() { grep "^$1=" "$GOOD_ENV" | head -1 | cut -d= -f2-; }
BUCKET="$(envval S3_BUCKET)"; ENDPOINT="$(envval S3_ENDPOINT_URL)"
# old curl (< 8.x) does not add x-amz-content-sha256 itself, so send it (SHA-256 of an empty body: GET/HEAD)
CURL_AUTH=(-H "x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" --user "$(envval S3_ACCESS_KEY):$(envval S3_SECRET_KEY)" --aws-sigv4 "aws:amz:$(envval S3_REGION):s3")

# ---- server helpers ---------------------------------------------------------
start_rustfs() {
  [ -n "$IN_COMPOSE" ] && return 0
  if [ -n "$(docker ps -q --filter name=$CONTAINER --filter status=running)" ]; then
    say "== Using the RustFS server that is already running"; return 0
  fi
  say "== Starting RustFS and creating test-bucket"
  docker compose -f "$COMPOSE" up -d rustfs >&2 || exit 2
  docker compose -f "$COMPOSE" --profile tools run --rm createbucket >&2 || exit 2
}
urlkey() { echo "${1// /%20}"; }
# size of an object as reported by the SERVER (independent of s3client: plain curl + SigV4); empty if missing
obj_size() {
  curl -s -I "${CURL_AUTH[@]}" "$ENDPOINT/$BUCKET/$(urlkey "$1")" | tr -d '\r' \
    | awk 'NR==1 && $2!="200"{exit} tolower($1)=="content-length:"{print $2}'
}
# "size<TAB>key" for every object in the bucket (optionally under a prefix)
list_bucket() {
  # '/' must be sent as %2F inside a query string (old curl does not encode it before signing)
  local p="${1:-}"; p="${p// /%20}"; p="${p//\//%2F}"
  curl -s "${CURL_AUTH[@]}" "$ENDPOINT/$BUCKET?list-type=2&prefix=$p" | sed 's/<Contents>/\n/g' \
    | sed -n 's#.*<Key>\(.*\)</Key>.*<Size>\([0-9]*\)</Size>.*#\2\t\1#p' | awk 'NF'   # drop empty lines; makes sure the last line ends with a newline (GNU and BusyBox sed differ)
}
# does the object physically exist inside the container's /data ? (RustFS stores key/xl.meta + part files)
on_disk() { [ -z "$IN_COMPOSE" ] && docker exec $CONTAINER test -f "/data/$BUCKET/$1/xl.meta"; }
show_disk() { docker exec $CONTAINER sh -c "du -ah '/data/$BUCKET/$1' 2>/dev/null" | sed 's/^/        /'; }

############################ manual mode: arguments given ############################
if [ $# -gt 0 ]; then
  start_rustfs
  case "$1" in
    help|-h|-help|--help) exec "$BIN" help ;;
    upload|download) CMD="$1"; shift ;;
    *) CMD=upload ;;
  esac

  # find prefix / out dir / positional arguments (files for upload, keys for download)
  PREFIX=""; OUTDIR=""; POS=(); ARGS=("$@")
  while [ $# -gt 0 ]; do
    case "$1" in
      -prefix|--prefix) PREFIX="${2:-}"; shift 2 ;;
      -prefix=*|--prefix=*) PREFIX="${1#*=}"; shift ;;
      -out|--out) OUTDIR="${2:-}"; shift 2 ;;
      -out=*|--out=*) OUTDIR="${1#*=}"; shift ;;
      -env|--env|-parallel|--parallel|-part-size|--part-size|-concurrency|--concurrency) shift 2 ;;
      -*) shift ;;
      *) POS+=("$1"); shift ;;
    esac
  done
  PREFIX="${PREFIX//\\//}"; PREFIX="${PREFIX#/}"; [ -n "$PREFIX" ] && [ "${PREFIX: -1}" != "/" ] && PREFIX="$PREFIX/"

  EXTRA=()
  if [ "$CMD" = download ] && [ -z "$OUTDIR" ]; then OUTDIR="$DOWNLOADS"; EXTRA=(-out "$OUTDIR"); fi

  say "== \$ s3client $CMD -env tests/.work/good.env ${EXTRA[*]:-} ${ARGS[*]}"
  echo >&2
  "$BIN" "$CMD" -env "$GOOD_ENV" ${EXTRA[@]+"${EXTRA[@]}"} "${ARGS[@]}"      # not captured: exactly what s3client prints
  CODE=$?
  echo >&2
  say "== s3client exit code: $CODE"

  BAD=0
  say "== Independent check against the server (curl + SigV4, not s3client)"
  if [ "$CMD" = upload ]; then
    touch "$MANIFEST"
    for f in "${POS[@]}"; do
      [ -f "$f" ] || continue
      key="$PREFIX$(basename "$f")"; local_size="$(fsize "$f")"; remote="$(obj_size "$key")"
      if [ "$remote" = "$local_size" ]; then
        echo "  VERIFIED   $key  ($local_size bytes is on the server)" >&2
        if on_disk "$key"; then echo "        stored in the container at /data/$BUCKET/$key/ :" >&2; show_disk "$key" >&2; fi
        grep -v -F "$key"$'\t' "$MANIFEST" > "$MANIFEST.tmp" 2>/dev/null; mv "$MANIFEST.tmp" "$MANIFEST"
        printf '%s\t%s\t%s\n' "$key" "$(sha "$f")" "$local_size" >> "$MANIFEST"
      else
        echo "  MISSING    $key  (local $local_size bytes, server: ${remote:-not found})" >&2; BAD=1
      fi
    done
    echo "  Bucket now contains:" >&2; list_bucket | sort -k2 | awk -F'\t' '{printf "        %12d  %s\n",$1,$2}' >&2
  else
    for key in "${POS[@]}"; do
      key="${key#/}"; f="$OUTDIR/$(basename "$key")"; remote="$(obj_size "$key")"
      if [ ! -f "$f" ]; then echo "  MISSING    $f was not downloaded" >&2; BAD=1; continue; fi
      if [ "$(fsize "$f")" != "$remote" ]; then echo "  MISMATCH   $f is $(fsize "$f") bytes, server has ${remote:-nothing}" >&2; BAD=1; continue; fi
      want="$(awk -F'\t' -v k="$key" '$1==k{print $2}' "$MANIFEST" 2>/dev/null | tail -1)"
      if [ -z "$want" ]; then echo "  VERIFIED   $f  (size matches the server; no upload record to compare content with)" >&2
      elif [ "$want" = "$(sha "$f")" ]; then echo "  VERIFIED   $f  (identical to the file you uploaded, sha256 ${want:0:16}...)" >&2
      else echo "  MISMATCH   $f differs from the file you uploaded" >&2; BAD=1; fi
    done
    echo "  Downloaded files are in: $OUTDIR" >&2
  fi
  [ $BAD = 0 ] || CODE=1
  [ -z "$IN_COMPOSE" ] && say "== RustFS left running; objects live in /data of container $CONTAINER (volume tests_rustfs-data)"
  exit $CODE
fi

############################ test-suite mode: no arguments ############################
rm -rf "$WORK/data" "$WORK/out"; mkdir -p "$WORK/data" "$WORK/out" "$RESULTS"
DATA="$WORK/data"; OUTD="$WORK/out"

cleanup() {
  if [ -z "$IN_COMPOSE" ]; then
    if [ -n "${CLEANUP:-}" ]; then
      echo "== Stopping RustFS and deleting its data"; docker compose -f "$COMPOSE" --profile tools down -v >/dev/null 2>&1
    else
      echo "== RustFS left running with the test objects in it (inspect: docker exec $CONTAINER ls /data/$BUCKET/tests)."
      echo "   Stop: docker compose -f tests/docker-compose.yml --profile tools down      (add -v to delete the data)"
    fi
  fi
}
trap cleanup EXIT
start_rustfs

echo "== Generating test files (big = ${BIG_MB} MB)"
head -c $((BIG_MB*1024*1024)) /dev/urandom > "$DATA/big.bin"
head -c $((13*1024*1024))     /dev/urandom > "$DATA/mid.bin"   # not a multiple of the 8 MB chunk size
echo "hello s3client" > "$DATA/small.txt"

echo "== Running tests"
run; has Usage; G=$?
check "T1 no arguments prints usage, exit 0" $(( CODE==0 && G==0 ? 0 : 1 )) "$OUT"

: > "$WORK/empty.env"
OUT="$(env -u S3_BUCKET -u S3_ACCESS_KEY -u S3_SECRET_KEY -u S3_ENDPOINT_URL "$BIN" -env "$WORK/empty.env" "$DATA/small.txt" 2>&1)"; CODE=$?
echo "$OUT" | grep -q "missing required environment variables"; G=$?
check "T2 missing config -> clear error, exit 1" $(( CODE==1 && G==0 ? 0 : 1 )) "$OUT"

sed 's/^S3_SECRET_KEY=.*/S3_SECRET_KEY=WRONG/' "$GOOD_ENV" > "$WORK/bad.env"
run -env "$WORK/bad.env" "$DATA/small.txt"; has SignatureDoesNotMatch; G=$?
check "T3 wrong secret key rejected (SignatureDoesNotMatch)" $(( CODE==1 && G==0 ? 0 : 1 )) "$OUT"

run -env "$GOOD_ENV" "$DATA/does-not-exist.bin"
check "T4 missing local file -> exit 1" $(( CODE==1 ? 0 : 1 )) "$OUT"

run -env "$GOOD_ENV" -prefix tests/run1/ -parallel 2 "$DATA"/*; has "3 uploaded, 0 failed"; G=$?
check "T5 wildcard + prefix + parallel upload of 3 files" $(( CODE==0 && G==0 ? 0 : 1 )) "$OUT"; echo "$OUT"

# independent proof that the files are really on the server
ok=0; for n in big.bin mid.bin small.txt; do [ "$(obj_size "tests/run1/$n")" = "$(fsize "$DATA/$n")" ] || ok=1; done
check "T6 server reports all 3 objects with the exact local size (independent curl check)" $ok
listed="$(list_bucket tests/run1/ | wc -l | tr -d ' ')"
check "T7 bucket listing shows 3 objects under tests/run1/" $(( listed==3 ? 0 : 1 )) "listed=$listed: $(list_bucket tests/run1/ | tr '\n\t' '  ')"
if [ -z "$IN_COMPOSE" ]; then
  ok=0; for n in big.bin mid.bin small.txt; do on_disk "tests/run1/$n" || ok=1; done
  check "T8 objects physically stored in the container under /data/$BUCKET/tests/run1/" $ok
fi

run -env "$GOOD_ENV" -prefix tests/run2/ -part-size 5 -concurrency 1 "$DATA/mid.bin" "$DATA/small.txt"; has "2 uploaded, 0 failed"; G=$?
check "T9 explicit files with -part-size 5 -concurrency 1" $(( CODE==0 && G==0 ? 0 : 1 )) "$OUT"

run download -env "$GOOD_ENV" -out "$OUTD" tests/run1/big.bin tests/run1/mid.bin tests/run1/small.txt; has "3 downloaded, 0 failed"; G=$?
check "T10 download 3 objects" $(( CODE==0 && G==0 ? 0 : 1 )) "$OUT"
for n in big.bin mid.bin small.txt; do
  [ "$(sha "$DATA/$n")" = "$(sha "$OUTD/$n")" ]; check "T11 SHA-256 identical after round trip: $n" $?
done
ls "$OUTD"/*.part >/dev/null 2>&1; check "T12 no leftover .part files" $(( $? == 0 ? 1 : 0 ))

run download -env "$GOOD_ENV" -out "$OUTD" tests/nope/missing.bin
check "T13 download of missing key -> exit 1" $(( CODE==1 ? 0 : 1 )) "$OUT"

SUMMARY="RESULT: $PASS passed, $FAIL failed"
echo; echo "$SUMMARY"
{ echo "s3client test run $(date '+%Y-%m-%d %H:%M:%S')"
  echo "platform: $(uname -sm) | big file: ${BIG_MB} MB | mode: $([ -n "$IN_COMPOSE" ] && echo container || echo host)"
  echo "go: $(go version)"; echo "curl: $(curl --version | head -1)"; command -v docker >/dev/null && echo "docker: $(docker --version)"
  echo; printf '%s\n' "${LOG[@]}"; echo; echo "$SUMMARY"; } > "$RESULTS/last-run.txt"
[ "$FAIL" = 0 ]
