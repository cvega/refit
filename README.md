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

Requires **Go 1.23+**, **Git**, and **Git LFS** (tested with 3.7.1).

```sh
go build -o bin/refit ./cmd/refit
./bin/refit help
```

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

## Validation Status

- **Passed:** local integration tests and mock HTTP tests.
- **Pending:** customer archive compatibility and a live GEI staging import.

Review [supported layouts and limitations](docs/runbook.md#recovery-and-limitations) before use.
