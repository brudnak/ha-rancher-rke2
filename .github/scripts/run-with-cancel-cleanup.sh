#!/usr/bin/env bash
set -euo pipefail

cleanup_started=0

cleanup_on_cancel() {
  signal="$1"
  if [ "$cleanup_started" -eq 1 ]; then
    exit 130
  fi
  cleanup_started=1

  echo "Received ${signal}; attempting best-effort cleanup before exiting."

  if [ "${SIGNOFF_LANE:-}" != "fresh-alpha-local-suites" ]; then
    go test -v -run '^TestHADeleteLinodeDownstream$' -timeout "${CANCEL_DOWNSTREAM_CLEANUP_TIMEOUT:-10m}" ./terratest || true
  fi

  go test -v -run '^TestHACleanup$' -timeout "${CANCEL_TERRAFORM_CLEANUP_TIMEOUT:-20m}" ./terratest || true
  exit 130
}

trap 'cleanup_on_cancel INT' INT
trap 'cleanup_on_cancel TERM' TERM

"$@"
