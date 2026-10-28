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
  if ! ARTEX_PG_DSN="postgres://${PGUSER}:${PGPASSWORD}@${PGHOST}:${PGPORT}/${database}?sslmode=disable" \
       go test -json -count=1 -timeout=10m "$package" | tee "$log"; then
    failed=1
  fi
  echo "::endgroup::"
  # Test helpers historically skip unavailable PostgreSQL. A configured CI
  # database failing to initialize is a failure, not a green skipped suite.
  if grep -Eqi 'postgres unavailable|no database config|no test database configured|requires isolated ARTEX_PG_DSN' "$log"; then
    echo "::error::${package}: required database tests were skipped"
    failed=1
  fi
done < .ci-logs/packages.txt
test "$index" -gt 0
exit "$failed"
