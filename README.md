# Refit

Migrate **GHES or GHEC to GitHub Enterprise Cloud** using migration archives.
Convert oversized Git blobs to LFS, including archived hidden PR history, and remap metadata commits before importing into private staging.

## Jump In

Download and extract your platform's binary from [Releases](https://github.com/cvega/refit/releases) ([installation](docs/runbook.md#install-a-release)); no Go required.
Install **Git** and **Git LFS**. Set classic PATs in `GH_SOURCE_PAT` and `GH_PAT`; see [required access and approvals](docs/runbook.md#automated-workflow).

Replace the placeholders. Use a new staging name and a policy reviewed against your archives; the approval flags confirm route and staging review.

```sh
./refit migrate -work work/MIGRATION \
	-source-url https://github.com/SOURCE_ORG/REPO \
	-target-org DEST_ORG -staging-repo REPO-staging \
	-policy reviewed-policy.json \
	-confirm-staging -archive-route-reviewed
```

Refit exports, waits, downloads, prepares, imports, and uploads LFS automatically.
No reviewed policy yet? Omit `-policy` to pause after inspection.

Resume or continue after policy review:

```sh
./refit migrate -work work/MIGRATION # add -policy PATH after review
```

See the [runbook](docs/runbook.md) for GHES endpoints, recovery, and final staging review.

## Safeguards

- **1 GB default:** convert only blobs above 1,000,000,000 bytes; configurable for the destination feature flag.
- **Source unchanged:** immutable archives, disposable rewrites, no Git ref pushes.
- **Fail closed:** unsupported layouts and unreviewed metadata stop the run.
- **Staging only:** private destination; production promotion is out of scope.

## Development

Build with **Go 1.26+**, using the latest supported patch. CI tests `oldstable` and `stable` on Ubuntu 24.04.

```sh
go build -o bin/refit ./cmd/refit
go test ./...
go test -race ./...
go vet ./...
```

## Validation Status

- **Passed:** local integration and HTTP tests; a live private GEI no-conversion
  control import with unchanged commit IDs and three existing LFS payload uploads.
- **Pending:** live oversized-blob conversion, hidden-PR/metadata rewrites, customer
  archive compatibility, and a live run of the new resumable coordinator.
