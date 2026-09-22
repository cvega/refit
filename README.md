# Refit

Move oversized Git blobs to LFS, including those hidden in PR history, using
GitHub migration archives.

## Workflow

1. **Export and inspect** migration archives, including archived hidden refs.
2. **Prepare** LFS history and remap metadata using reviewed rules.
3. **Stage** a GEI import into a new private repository.
4. **Upload LFS payloads** separately, then review the migration.

See the [runbook](docs/runbook.md) for commands, policy review, and recovery.

## Safeguards

- **1 GB default:** convert only blobs above 1,000,000,000 bytes, matching the customer's feature flag. The threshold is configurable.
- **Immutable originals:** rewrite disposable archive copies, never the source.
- **Archive-first:** no network clone substitutes for hidden history; no Git refs are pushed.
- **Staging only:** production promotion is out of scope.

## Quick Start

Requires **Go 1.26+**, **Git**, and **Git LFS** (tested with 3.7.1).
Use the latest patch of a [supported Go release](https://go.dev/doc/devel/release#policy).

```sh
go build -o bin/refit ./cmd/refit
./bin/refit help
```

Use `refit migrate -work work/MIGRATION ...` to export, wait, prepare, import,
and upload LFS in one run. Resume with the same `-work` directory; credentials
are never saved. See the [automated workflow](docs/runbook.md#automated-workflow)
for approvals and policy review.

## Development

CI tests the latest patches of Go's two supported release lines (`oldstable` and
`stable`) on Ubuntu 24.04, printing the resolved toolchain version in each run.

```sh
go test ./...
go test -race ./...
go vet ./...
```

## Validation Status

- **Passed:** local integration and HTTP tests; a live private GEI no-conversion
	control import with unchanged commit IDs and three existing LFS payload uploads.
- **Pending:** live oversized-blob conversion, hidden-PR/metadata rewrites, customer
	archive compatibility, and a live run of the new resumable coordinator.

Review [supported layouts and limitations](docs/runbook.md#recovery-and-limitations) before use.
