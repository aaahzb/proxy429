#!/usr/bin/env bash
# Local CI-equivalent gate: gofmt + go vet + go test in one command.
# CI checks gofmt on an LF checkout (ubuntu); this repo is cloned with autocrlf=true,
# so CRLF files are compared after stripping \r — same semantics as CI.
# Usage: bash test/verify.sh   (exit 0 = all green; prints offenders before failing)
set -u
cd "$(dirname "$0")/.."

bad=""
while IFS= read -r f; do
  if ! diff -q <(tr -d '\r' <"$f") <(tr -d '\r' <"$f" | gofmt) >/dev/null; then
    bad="$bad  $f\n"
  fi
done < <(git ls-files -co --exclude-standard -- '*.go')
if [ -n "$bad" ]; then
  printf 'not gofmt-formatted (CI LF semantics):\n%b' "$bad"
  exit 1
fi
echo "gofmt: OK"

go vet ./... || exit 1
echo "vet: OK"

go test -count=1 ./... || exit 1
echo "VERIFY_OK"
