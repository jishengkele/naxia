#!/usr/bin/env bash
set -euo pipefail

mkdir -p .ci-logs
failed=0
index=0
go list ./... > .ci-logs/packages.txt
while IFS= read -r package; do
  index=$((index + 1))
  database="naxia_ci_${index}"
  createdb "$database"
  log=".ci-logs/${index}-${package##*/}.jsonl"
  echo "::group::${package} (${database})"
  dsn="postgres://${PGUSER}:${PGPASSWORD}@${PGHOST}:${PGPORT}/${database}?sslmode=disable"
  command=(env "ARTEX_PG_DSN=$dsn" go test -json -count=1 -timeout=10m "$package")
  if [[ "$package" == */server ]]; then
    # Complete Linux /proc writer inspection needs the same root privileges as
    # the intended Docker/root deployment. Do not weaken the production check.
    command=(sudo env "PATH=$PATH" "GOCACHE=$(go env GOCACHE)" "GOMODCACHE=$(go env GOMODCACHE)"
      "ARTEX_CONFIG=$ARTEX_CONFIG" "ARTEX_PG_DSN=$dsn" go test -json -count=1 -timeout=10m "$package")
  fi
  if ! "${command[@]}" | tee "$log"; then
    failed=1
  fi
  echo "::endgroup::"
  # Test helpers historically skip unavailable PostgreSQL. A configured CI
  # database failing to initialize is a failure, not a green skipped suite.
  if grep -Eqi 'postgres unavailable|database unavailable|no database config|no test database configured|test postgres not configured|requires isolated ARTEX_PG_DSN|ARTEX_PG_DSN is required' "$log"; then
    echo "::error::${package}: required database tests were skipped"
    failed=1
  fi
done < .ci-logs/packages.txt
test "$index" -gt 0
exit "$failed"
