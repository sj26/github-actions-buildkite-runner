# GitHub Actions, on Buildkite

This is a terrible idea, but here's something you can do: run a GitHub runner
scale set which schedules each matching GitHub job as a Buildkite job.

```yaml
runs-on: buildkite
```

GitHub still runs Actions. Checkout, actions, logs, artifacts and job conclusions
belong to GitHub. Buildkite supplies a hosted machine running the official
ephemeral Actions runner. There is no workflow importer, Kubernetes cluster,
webhook receiver or public port.

The small Go process uses GitHub's public-preview
[`actions/scaleset` SDK](https://github.com/actions/scaleset/tree/v0.5.0) to listen
for demand over outbound HTTPS. It creates a one-job JIT credential, puts it in a
pipeline-restricted Buildkite secret, then creates a build using the REST API.
GitHub picks the job for that runner. On completion, the secret is deleted.
Only jobs targeting **this scale-set name** are scheduled here—not every job in
your repository or organisation.

## Set up

Use a disposable, trusted repository and a dedicated Buildkite pipeline. This
runs workflow code on a machine with Buildkite credentials and network access.
Do not point it at public pull requests from strangers.

1. Install [Go](https://go.dev/dl/) 1.27.3 or newer on Linux. The listener uses
   `flock`; the runner bootstrap supports Linux/x64 only.
2. Put [runner.sh](runner.sh) in the root of the pipeline's repository. Create a
   pipeline with the single step in [pipeline.yml](pipeline.yml), on an existing
   **hosted** Linux/x64 queue. Disable automatic build webhooks and schedules.
   Set a pipeline timeout too. The example step times out after 15 minutes;
   the runner itself has a 12-minute limit.
3. Create a GitHub PAT with runner-registration authority. For a repository,
   the SDK documents a classic PAT with `repo` and repository admin access;
   for an organisation, `admin:org` and organisation owner access. A fine-grained
   PAT needs repository **Administration: write**, or organisation
   **Self-hosted runners: write** plus **Administration: read**, respectively. See
   [GitHub's authentication requirements](https://docs.github.com/en/actions/tutorials/use-actions-runner-controller/authenticate-to-the-api).
   This toy implements PAT auth only, using the default runner group (ID 1).
4. Create a Buildkite API token restricted to that organisation with
   `write_builds`, `write_secrets`, `read_secrets_details`, `read_pipelines` and
   `read_builds`. Alternatively, supply the last two scopes through a separate
   `BUILDKITE_READ_TOKEN` belonging to the same user. GraphQL is not needed.
   Use finite expiry beyond the run's lifetime. These scopes are org-wide:
   the code targets one pipeline, but the token is **not** pipeline-scoped.

Load tokens from your secret manager into `GITHUB_TOKEN` and
`BUILDKITE_API_TOKEN`. Do not put them in this repository, a workflow, build
environment, command-line argument or shell history. Then configure:

```sh
export GITHUB_CONFIG_URL=https://github.com/YOU/YOUR-REPOSITORY
# Or https://github.com/YOUR-ORGANISATION
export SCALE_SET_NAME=buildkite
export BUILDKITE_ORG=your-organisation
export BUILDKITE_PIPELINE=github-runners
export BUILDKITE_QUEUE=linux-small
export BUILDKITE_BRANCH=main
export BUILDKITE_COMMIT=HEAD # Prefer a trusted full commit SHA for the bootstrap.
export MAX_RUNNERS=2
export MAX_BUILDS=10
export LIFETIME=20m
export STATE_PATH=.state/buildkite.json
go test ./...
go build -o github-actions-buildkite-runner .
./github-actions-buildkite-runner
```

Keep that process running on a machine with outbound HTTPS to GitHub and
Buildkite. No browser is used. `MAX_RUNNERS` is concurrent capacity (1–10);
`MAX_BUILDS` counts attempts across restarts (1–100). The lifetime is at most one
hour and its absolute deadline is saved, so restarting does not extend it.
The queue setting validates the pipeline's existing queue; it does not edit it.

Two instances? Use different `SCALE_SET_NAME` and `STATE_PATH` values. Give each
its own pipeline/queue config if desired. Jobs select one name in `runs-on`.
Do not run two listeners for the same scale set, or copy active state to another
machine. The lock only protects one local state file.

## Try a job

Add this to `.github/workflows/buildkite.yml` in the target repository, then
push a commit. The listener reacts to GitHub's queue; it never dispatches a
workflow itself. The matrix spends **two Buildkite builds**.

```yaml
name: What have we done
on: push
permissions:
  contents: read
jobs:
  test:
    runs-on: buildkite
    strategy:
      matrix:
        example: [one, two]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: '22'
      - run: |
          test "$BUILDKITE" = true
          test "$(git rev-parse HEAD)" = "$GITHUB_SHA"
          node -e 'if (process.versions.node.split(".")[0] !== "22") process.exit(1)'
          printf 'Running on %s#%s\n' "$BUILDKITE_BUILD_URL" "$BUILDKITE_JOB_ID"
```

Inspect the native Actions job, its runner name and the linked Buildkite build.
The runner archive is pinned to 2.338.2 and SHA256-checked. Update both the
version and digest when GitHub requires a newer runner. Action tags above are
examples; pin them to reviewed commits for anything more serious.

## Stop and clean up

Send SIGINT (Ctrl-C) or SIGTERM. The listener closes its GitHub message session;
already-created builds keep running. Let native completion messages arrive
before stopping if you want automatic secret deletion. Each saved runner has
its build URL, registration ID, secret ID/key and completion result.

Restart with the same config and state. Saved live builds count towards capacity
and budget. If the deadline expired, inspect existing jobs first, then set
`ExpiresAt` to `null` in the JSON to grant another bounded session. Raising
`MAX_BUILDS` is an explicit new spend decision; deleting state is not a reset.

After stopping, remove only this instance's remaining secrets using the saved
IDs in the Buildkite cluster's Secrets UI. Remove stale runners and the owned
scale set in GitHub's runner settings (or SDK `DeleteRunnerScaleSet` with the
saved ID). Revoke your temporary API tokens in their account settings. Keep the
state until resources are reconciled. Delete the disposable pipeline/repository
only when you no longer need their evidence. There is no automatic teardown of
shared queues or pipelines.

## The catch

Each job pays for a cold Buildkite hosted build, runner download and startup.
That includes canceled jobs which already caused provisioning. There is no warm
pool or cancellation propagation, and the build cap is local, not an org-wide
spending limit. Check both providers' current billing terms.

This is deliberately not a provider. A claim is saved **before** a remote write.
If a request times out or the process dies during JIT/secret/build creation, it
stops rather than guessing and making another build. Inspect the exact runner
name and remote resources before repairing state. There are no mutation retries,
distributed leases, automatic replacement builds or exactly-once promises.
Restart also stops if a hosted build ended without a recorded native completion.
A crash between receiving completion and saving it needs manual reconciliation.

JIT credentials never enter the build environment or state file; only a secret
key does. The bootstrap unsets the Buildkite agent access-token environment
variable before starting Actions, but **that is not a security boundary**.
Workflow code shares the agent machine, processes, filesystem and network.
Use trusted code, minimal credentials and a dedicated cluster if experimenting.
State contains repository/job metadata; keep it private. Don't enable shell
tracing around the bootstrap's secret fetch. Buildkite masks are not a sandbox.

Not sensible infrastructure. A working toy.
