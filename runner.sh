#!/usr/bin/env bash
# One official ephemeral GitHub runner, inside one Buildkite job.
set -euo pipefail
test "${BUILDKITE:-}" = true
test "$(uname -sm)" = 'Linux x86_64'
test "$BUILDKITE_AGENT_META_DATA_QUEUE" = "$RUNNER_QUEUE"
: "${GITHUB_RUNNER_JIT_SECRET:?Set by the listener}"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"
version=2.338.2
curl --fail --silent --show-error --location \
  "https://github.com/actions/runner/releases/download/v${version}/actions-runner-linux-x64-${version}.tar.gz" \
  -o runner.tar.gz
echo 'baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40  runner.tar.gz' | sha256sum -c -
tar xzf runner.tar.gz
rm runner.tar.gz
jit=$(buildkite-agent secret get "$GITHUB_RUNNER_JIT_SECRET")
unset BUILDKITE_AGENT_ACCESS_TOKEN
export RUNNER_ALLOW_RUNASROOT=1
echo "Official Actions runner ${version} on ${BUILDKITE_BUILD_URL}#${BUILDKITE_JOB_ID}"
timeout --signal=TERM --kill-after=30s 12m ./run.sh --jitconfig "$jit"
