# Refit How-To

Use `migrate` for the normal workflow. Refit handles exports, waiting, preparation,
private import, and LFS transfer; you review the metadata policy and final result.
The source stays unchanged. Production promotion is not part of this tool.

| I want to...                                  | Start here                                                |
| --------------------------------------------- | --------------------------------------------------------- |
| Install Refit                                 | [Get a binary](#install-a-release)                        |
| Migrate a repository                          | [First migration](#automated-workflow)                    |
| Use an already-reviewed policy                | [Run straight through](#i-already-have-a-reviewed-policy) |
| Continue an interrupted run                   | [Resume](#resume-a-migration)                             |
| Try again from scratch                        | [Start fresh](#start-fresh)                               |
| Use GHES, data residency, or my own LFS cache | [Other setups](#other-setups)                             |
| Understand a pause or error                   | [Troubleshooting](#recovery-and-limitations)              |
| Check the imported repository                 | [Final review](#review-the-result)                        |

Examples assume `refit` is on your PATH. Use `./refit` instead when running the
extracted binary directly, or `./bin/refit` for a source build. Replace uppercase
placeholders with your values; keep using the same terminal for your credentials.

## Install A Release

Download a binary archive and `SHA256SUMS` from
[Releases](https://github.com/cvega/refit/releases), not GitHub's automatically
generated source archives. Until the first release is published, use the README's
source-build command. Published binaries do not require Go; Git and Git LFS are
still runtime dependencies.

Choose `darwin` for macOS or `linux` for Linux, and `arm64` for Apple Silicon/ARM64
or `amd64` for Intel/AMD x86-64. Windows binaries are not currently provided.
Archive names follow `refit_VERSION_OS_ARCH.tar.gz`.

For example, after downloading a macOS Apple Silicon release (replace the version):

```sh
VERSION=v0.1.0
ARCHIVE="refit_${VERSION}_darwin_arm64.tar.gz"
awk -v file="./$ARCHIVE" '$2 == file' SHA256SUMS > selected.sha256
test -s selected.sha256 && shasum -a 256 -c selected.sha256 && tar -xzf "$ARCHIVE"
./refit help
```

On Linux, use `sha256sum -c selected.sha256` instead. Extract into a new directory;
archives include the executable, README, runbook, and unreviewed policy example.
Checksums detect corruption, not publisher identity. macOS binaries are not
Developer ID signed or notarized; managed devices may require administrator approval.
Do not disable platform security controls to run them.

<details>
<summary>For maintainers: publish a release</summary>

The Release workflow runs on pushed `v*` tags, tests the tagged commit with
`go test ./...`, `go test -race ./...`, and `go vet ./...`, then cross-compiles
macOS/Linux amd64/arm64 binaries and creates a **draft prerelease** with checksums.
Only the Linux runner's native binary receives a smoke test; cross-compilation
does not establish runtime compatibility on every target.

After committing and pushing the reviewed release changes, a maintainer can run:

```sh
git tag -a v0.1.0 -m "Refit v0.1.0 preview"
git push origin v0.1.0
```

Review the draft's notes and assets, verify downloaded binaries on intended
platforms, and publish it explicitly in GitHub Releases. Keep preview status and
the README's validation limitations until broader live validation is complete.
Existing releases are not overwritten by workflow retries.

</details>

## Automated Workflow

### 1. Get Ready

- Install Git and Git LFS: `git --version` and `git lfs version` must work.
- Provide classic PATs through your secure environment or secret manager:

| Environment variable | Required scopes                 | Account access                        |
| -------------------- | ------------------------------- | ------------------------------------- |
| `GH_SOURCE_PAT`      | `repo`, `admin:org`             | Active source organization owner      |
| `GH_PAT`             | `repo`, `admin:org`, `workflow` | Active destination organization owner |

Never paste tokens into command arguments, URLs, policy files, or shell history.
Refit checks access before exporting; it does not save credentials. Delegated
migrator roles are not supported by this automatic preflight.

- Arrange a quiet source window for both exports. Refit does not lock the source;
  the two exports are not an atomic snapshot.
- Use private local storage with room for archives, extracted copies, and LFS data.
- Confirm the destination's 1 GB Git feature flag and separate LFS storage/object limits.
- Choose a **new** name ending in `-staging`. Do not create the repository yourself.

### 2. Start Your First Migration

Start without a policy or import approval so you can review the actual archives:

```sh
refit migrate -work work/MIGRATION \
  -source-url https://github.com/SOURCE_ORG/REPO \
  -target-org DEST_ORG -staging-repo REPO-staging
```

**What happens:** Refit exports, waits, downloads, and inspects. It then exits with
`metadata policy review required`. This is the expected review gate, not a failed
import. Nothing has been imported yet. Originals and progress are in `work/MIGRATION`.

### 3. Review The Policy And Approve Staging

A policy tells Refit **which metadata fields should follow rewritten commits and
which should stay unchanged**. Moving a blob to LFS changes commit IDs; PRs and
reviews may still reference the old IDs. The policy lets Refit update those
references without blindly replacing hashes in comments or unrelated fields.

1. Copy [policy.example.json](../policy.example.json) to `reviewed-policy.json`.
2. Review the JSON schema in **both** downloaded originals with a trusted archive
   viewer. Define rules using [the policy guide](#review-a-metadata-policy).
   Merely changing `reviewed` to `true` is not sufficient.
3. Confirm the rewritten-archive route with your migration team, especially for
   GHEC exports. Ensure destination Actions, Pages, webhooks, and other automation
   stay inactive until reviewed; private visibility alone does not disable them.
4. Continue using the same work directory:

```sh
refit migrate -work work/MIGRATION -policy reviewed-policy.json \
  -confirm-staging -archive-route-reviewed
```

**What happens:** Refit fetches existing LFS payloads, prepares and verifies the
archives, creates the private staging import, waits for success, and uploads LFS.
No Git refs are fetched or pushed to the source. A successful run ends with
`"state":"SUCCEEDED"` and `"review":"required"`. Finish with [staging review](#review-the-result).

To review prepared output before authorizing import, supply `-policy` but omit
the approval flags. Refit pauses after preparation; add the flags when ready.

### I Already Have A Reviewed Policy

If your policy applies to the actual archive schema and staging is approved, run
the complete workflow in one command:

```sh
refit migrate -work work/MIGRATION \
  -source-url https://github.com/SOURCE_ORG/REPO \
  -target-org DEST_ORG -staging-repo REPO-staging \
  -policy reviewed-policy.json \
  -confirm-staging -archive-route-reviewed
```

## Resume A Migration

Use the same terminal credentials and work directory:

```sh
refit migrate -work work/MIGRATION
```

Completed steps and saved export/import IDs are reused. Do not repeat source or
destination flags. Keep the work directory at its original path and run only one
process against it. Policy/cache paths and approvals supplied on resume are saved.

Need a longer wait? Each wait defaults to 30 minutes with 10-second status checks:

```sh
refit migrate -work work/MIGRATION -wait-timeout 2h -poll-interval 20s
```

Interrupting or timing out stops local waiting, **not** the remote migration.
An uncertain remote write is deliberately not retried; see [troubleshooting](#recovery-and-limitations).

## Start Fresh

Choose **both** a new work directory and a new staging name:

```sh
refit migrate -work work/MIGRATION-2 \
  -source-url https://github.com/SOURCE_ORG/REPO \
  -target-org DEST_ORG -staging-repo REPO-retry-staging
```

This starts new exports and pauses for policy review. Add your reviewed policy
and approval flags to run straight through. It does not cancel an earlier remote
migration or delete its destination. Preserve old archives and receipts.

## Other Setups

Set source/destination options on the **first** invocation; resume using `-work`.

| Situation                                    | What to change                                                                                                                 |
| -------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| GHES source                                  | Use `-source-url https://HOST/ORG/REPO -source-api https://HOST/api/v3`.                                                       |
| Data-resident GHEC destination               | Supply approved `-target-api` and `-upload-api` origins. Confirm the upload origin with GitHub; do not derive it.              |
| Exact feature-flag boundary differs          | Set `-threshold-bytes BYTES`. Default: 1,000,000,000; equality is allowed. No public 400 MiB limit is imposed.                 |
| Existing LFS on the standard source endpoint | No extra flag: `migrate` fetches payloads from the explicit source URL, not archived config.                                   |
| Custom LFS service or existing local cache   | Add `-lfs-objects /absolute/path/to/lfs/objects`, containing `aa/bb/full-sha256` entries. This can also be supplied on resume. |
| Larger extraction budget needed              | Set `-max-extracted-bytes` and `-max-files` deliberately before starting. Defaults: 200 GiB and 2,000,000 members per archive. |

Source URLs use `https://HOST/ORG/REPO`, without `.git` or a trailing slash.
Custom endpoints authorize sending the corresponding PAT to that endpoint.

## Recovery And Limitations

| What you see                                             | What to do next                                                                                                                    |
| -------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| Missing `GH_SOURCE_PAT` or `GH_PAT`                      | Set them securely in the terminal running Refit, then resume.                                                                      |
| Missing scopes or owner access                           | Correct the classic PAT/access using the table above, then resume.                                                                 |
| Policy review required or policy not reviewed            | Review both archives, then resume with `-policy PATH`.                                                                             |
| Preparation complete, approval required                  | Review the route and destination automation, then resume with both approval flags.                                                 |
| Wait timed out, connection interrupted, or Ctrl-C        | Resume with the same `-work`; remote work may still be running.                                                                    |
| Destination already exists                               | Do not delete it blindly. Reconcile any prior import; for a separate trial use a new work directory and destination name.          |
| Workflow is locked                                       | Check for an active process. Only after confirming none exists, remove a stale `migration.lock` and resume.                        |
| Unresolved export/staging attempt                        | Inspect GitHub export/import status and destination activity. Preserve attempt files; do not remove them to force resubmission.    |
| Original exists without a completed download record      | Reconcile the file and export. Do not overwrite the original.                                                                      |
| Missing or corrupt LFS data                              | Check source access or supply a verified cache with `-lfs-objects`, then resume.                                                   |
| Preparation fails                                        | Fix the reported policy/layout/cache issue, then resume; `migrate` creates a fresh disposable preparation.                         |
| Import fails or validation fails                         | Read the destination Migration Log when available and GitHub migration status. Re-running cannot repair a terminal remote failure. |
| Unsupported layout or signed history requires conversion | Stop and review the [supported layouts](#supported-layouts); do not bypass the check with policy approval.                         |

Saved valid receipts recover missing completion checkpoints. When no receipt
exists, a lost response may still mean GitHub accepted the operation; reconcile
it before taking further action. Uploaded archives or unused migration sources
may need operator cleanup. Refit never deletes remote exports or unlocks sources.

## Review The Result

After automatic import **and** LFS upload succeed:

1. Read the destination Migration Log and resolve warnings/errors.
2. Compare branches, tags, default branch, and available hidden-ref evidence with
   the preparation report. Importer handling of hidden refs needs live validation.
3. Check PRs, issues, authors, comments, diffs, deleted branches, and review anchors.
4. Clone the **destination for validation only**. Run `git fsck --full`, fetch
   historical LFS with `git lfs fetch --all`, and verify representative revisions
   and payload SHA-256 hashes, not just the default branch. Hidden-only content
   may need explicit PR/ref access or GitHub-assisted validation.
5. Review permissions and disabled automation, then record staging approval.
   Production promotion is a separate process.

The final JSON names `prepared_work`. Its `report.json` records checksums, refs,
blob inventory, LFS OIDs, and metadata changes. Keep the entire workflow directory
private and backed up, including originals, maps, checkpoints, and import/LFS receipts.

## Review A Metadata Policy

### What The Policy Does

Think of the policy as instructions for handling fields, **not a list of commit
IDs you maintain by hand**. Refit generates the old-to-new commit map during
preparation and applies your rules to disposable copies of the archives.

For example, suppose conversion changes commit `OLD` to `NEW` (short labels here,
not actual SHAs). A PR's head-commit field should become `NEW`. A comment quoting
`OLD` may need to remain exactly as written. The rules distinguish those cases:

| Rule         | Use it for                                        | What Refit does                                         |
| ------------ | ------------------------------------------------- | ------------------------------------------------------- |
| `commit`     | A field containing a full commit SHA              | Looks up its replacement in the generated commit map.   |
| `commit-url` | A URL with a full commit SHA as a path segment    | Updates that commit segment using the map.              |
| `preserve`   | Reviewed text or values that must not be remapped | Leaves the value unchanged, even if it contains a hash. |

This avoids two mistakes: leaving structural references pointing at replaced
commits, and changing historical discussion or unrelated hashes. Unknown JSON
files, unmapped commit references, or unreviewed full SHAs stop preparation rather
than being guessed. Policy approval does not guarantee GEI importer compatibility.

### What You Need To Review

1. Start from [policy.example.json](../policy.example.json). Its `git` and
   `metadata` sections describe the two archives; the Git archive can contain JSON too.
2. List every JSON file outside Git internals by its exact archive-relative path.
   For each commit-bearing field, choose the appropriate rule above. Use an empty
   rule list only after checking the file needs no commit-reference rules.
3. Set both sections' `reviewed` fields to `true` only after reviewing the schema.
   Do not apply blanket `preserve` rules to get past an error.
4. Resume with `refit migrate -work work/MIGRATION -policy reviewed-policy.json`.
   Add staging approval flags when ready to import.

You can reuse a policy when the filenames, schema, and field meanings still match;
it is not tied to one set of commit IDs. Check compatibility with each archive pair.

**What if nothing needs conversion?** The current tool still requires a reviewed
policy. Unchanged commits have identity mappings (old ID equals new ID), so those
references stay the same. This is a conservative Refit requirement, not a GitHub
requirement; it does not mean the run needs to rewrite commit IDs.

<details>
<summary>Policy rules, example, and schema-review checklist</summary>

Review commit-bearing fields: PR base/head/merge SHAs, review commit IDs, original
review positions, commit comments, and commit URLs. REST response field names are
not necessarily archive schema fields. Understand references outside the exported
repository rather than silently reassigning them.

Create a policy for the actual archive schema. The following is an **illustration
only**, assuming a metadata file `pull_requests.json` containing an array with
`head_sha`, `base_sha`, and `body`. Those names are not claimed to be the GitHub
archive schema:

```json
{
  "git": { "reviewed": true, "files": {} },
  "metadata": {
    "reviewed": true,
    "files": {
      "pull_requests.json": [
        { "path": "/*/head_sha", "action": "commit" },
        { "path": "/*/base_sha", "action": "commit" },
        { "path": "/*/body", "action": "preserve" }
      ]
    }
  }
}
```

Start from [policy.example.json](../policy.example.json), which is deliberately
unreviewed. Each JSON file outside Git internals needs an exact relative filename
entry in its archive's policy. Use an empty rule list only after confirming no
commit references need rewriting. The Git archive may itself contain metadata;
its rules belong under `git`, not `metadata`.

- `commit`: replace an exact full commit SHA using the complete commit map,
  including identity entries for unchanged commits. Null/empty fields are allowed.
- `commit-url`: remap full commit SHAs appearing as URL path segments. No arbitrary
  substring replacement is performed.
- `preserve`: intentionally retain a value or subtree, such as quoted discussion
  text. Do not use this to bypass unresolved structural commit references.

Paths are RFC6901 JSON pointers. `*` matches array elements only. Escape `/` as
`~1` and `~` as `~0`. Unreviewed full SHA references fail closed. Abbreviated SHAs,
base64/other encodings, and application-specific cross-record relationships are
not inferred: schema review must account for them. JSON member ordering and
whitespace can change in rewritten files; numbers retain their literal precision.
Non-JSON files are retained and scanned for textual changed full SHAs.

</details>

## Manual Commands

You do **not** need these for a normal `migrate` run. They are for existing archive
pairs, separately reviewed delegated-access workflows, and operator-led recovery.
Do not mix manual mutations into an active automated workspace.

<details>
<summary>Export, inspect, prepare, stage, and upload individually</summary>

### Export And Inspect

Set `SOURCE_API=https://api.github.com`, or `https://HOST/api/v3` for GHES.
Keep the source quiet for both exports; reject inconsistent archive pairs.

```sh
refit export -source-api "$SOURCE_API" -org SOURCE_ORG -repo REPO -kind git
refit export -source-api "$SOURCE_API" -org SOURCE_ORG -repo REPO -kind metadata
```

Record both IDs. Repeat status checks until each is `exported`, then download:

```sh
refit export-status -source-api "$SOURCE_API" -org SOURCE_ORG -export-id GIT_ID
refit export-status -source-api "$SOURCE_API" -org SOURCE_ORG -export-id METADATA_ID
refit download -source-api "$SOURCE_API" -org SOURCE_ORG -export-id GIT_ID -out original-git.tar.gz
refit download -source-api "$SOURCE_API" -org SOURCE_ORG -export-id METADATA_ID -out original-metadata.tar.gz
refit inspect -git-archive original-git.tar.gz -work inspection
```

Already have both archives? Skip export/download and start with `inspect`.
Downloads require new files and do not forward the source PAT to storage.
Inspection requires a new directory and scans all stored objects, including
unreachable ones. Compare hidden refs such as `refs/pull/*` with expected export
contents; a network clone cannot recover omitted archive history.
Review both archives using the [policy guide](#review-a-metadata-policy).

### Prepare And Verify

```sh
refit prepare \
  -git-archive original-git.tar.gz \
  -metadata-archive original-metadata.tar.gz \
  -policy reviewed-policy.json \
  -work prepared \
  -threshold-bytes 1000000000
refit verify -work prepared
```

If the source already uses LFS, first fetch its existing payloads separately into
an operator-controlled LFS cache using approved source authentication. That fetch
does not replace the archive or fetch Git history. Supply its `objects` directory
using `-lfs-objects /absolute/path/to/lfs/objects`; entries must have the standard
`aa/bb/full-sha256` layout. Missing, corrupt, or noncanonical LFS pointers/payloads
block preparation. The CLI never fetches them implicitly from archived config.

Preparation produces:

- `prepared/git/` and `prepared/metadata/`: disposable rewritten trees.
- `prepared/commit-map.csv`: complete old/new commit map.
- `prepared/git-rewritten.tar.gz` and `prepared/metadata-rewritten.tar.gz`.
- `prepared/report.json`: source/output checksums, threshold, before/after refs,
  blob inventory, LFS OIDs, and metadata replacement count.

The Git implementation roots reachable commits under temporary local refs for
Git LFS, then explicitly restores all original ref names through the commit map,
including custom/hidden/remote refs. Unsigned annotated tags are reconstructed.
It removes temporary refs, expires reflogs, and prunes obsolete/unreachable
objects on the disposable copy. Metadata references to unmapped commits block
completion. Review unreachable history before approving that pruning.

When no reachable blobs need conversion, commit and tag objects (including signed
objects) retain their original IDs and content. Signatures are preserved, not
cryptographically validated. Signed history still blocks runs requiring conversion
until an explicit signature policy is implemented.

Repacking retains surviving member paths, order, and tar headers. LFS payloads,
hooks, and reflogs are excluded from the Git archive. Local payloads remain for
the later upload. The rewritten Git archive is re-extracted and its refs and
blob sizes checked. Originals are hashed before and after preparation.

`report.json` appears only after successful preparation. It is a locally trusted
manifest, not a signature or defense against an attacker who can edit both report
and artifacts. Keep the whole workspace private and immutable between review and
staging. Verification detects changed output archives, commit-map bytes, local
refs, oversized objects, and missing/corrupt LFS payloads.

### Import Into Private Staging

Choose a destination name ending in `-staging` or containing `-staging-`.
**Do not pre-create the repository**: GEI creates it. Existing destinations are
rejected. Ensure destination policy keeps Actions, Pages, webhooks, and other
automation inactive until reviewed; private visibility alone does not disable
automation or imported workflows.

```sh
refit stage \
  -work prepared \
  -source-url https://SOURCE_HOST/SOURCE_ORG/REPO \
  -target-org TARGET_ORG \
  -staging-repo REPO-staging \
  -confirm-staging \
  -archive-route-reviewed
```

For a data-resident GHEC destination, supply its `-target-api` and `-upload-api`
origins. Verify the correct upload origin with GitHub rather than deriving it.
Staging re-verifies prepared artifacts, checks the repository is absent, uploads
both archives to GitHub-owned storage in chunks, creates a `GITHUB_ARCHIVE`
migration source, and invokes GEI with `private` visibility and
`continueOnError=false`.

The returned migration ID is saved in `prepared/staging.json`:

```sh
refit status -migration-id MIGRATION_NODE_ID
```

To follow the saved import until it finishes, without copying its ID:

```sh
refit status -work prepared -wait
```

Progress and elapsed time go to stderr; the terminal result is JSON on stdout.
The default polling interval is 10 seconds and the wait timeout is 30 minutes;
adjust with `-poll-interval` and `-wait-timeout`. Failure returns a nonzero exit
code. Interrupting or timing out stops only the local wait, not the remote import;
resume with the same command. `stage -wait` also follows completion after saving
the migration ID. Import success still requires the separate LFS upload below.

Use the same `-target-api` when checking a nondefault destination. A successful
GEI import does not mean LFS content is available yet.

### Upload LFS

```sh
refit lfs-push -work prepared -confirm-staging
```

This reads the saved staging identity, requires GEI state `SUCCEEDED`, checks the
destination is the expected private repository, verifies local objects, and
uploads the explicit SHA-256 object list, including objects found only in hidden
history. It never runs `git push`. Destination authentication is supplied through
the subprocess environment, not arguments or archived credential helpers. A
successful upload writes `prepared/lfs-uploaded.json`.

Then [review the result](#review-the-result) before sign-off.

### Manual Recovery

A failed preparation may leave a diagnostic extraction but no successful report.
Correct the layout/policy/cache issue and run preparation into a **new** work
directory from the originals. Never reuse a half-rewritten extraction.

`staging-attempt.json` is created before remote writes. A second stage attempt
against that work directory is blocked. If a request times out, inspect migration
status and destination activity before retrying: a lost response may follow a
successful enqueue. Do not blindly remove the attempt file. Uploaded archives and
unused migration sources may need operator cleanup. Export, enqueue, and upload
operations are not automatically retried. LFS upload itself can be rerun.

</details>

## Supported Layouts

Supported today: separate single-stream tar.gz archives, regular files/directories,
one extracted SHA-1 files-backend bare Git repository, and reviewed JSON metadata.
Unsupported layouts stop the run rather than dropping refs or guessing.

<details>
<summary>Unsupported archive, Git, LFS, and metadata formats</summary>

- Git bundles, nested compressed archives (including compressed attachments),
  combined Git/metadata input, multiple repositories/wikis, worktrees and gitfiles.
- Archive links/special files, duplicate/case-colliding paths, shallow/partial
  repositories, alternates, grafts, replace/notes refs, detached HEAD, noncommit
  refs, symbolic non-HEAD refs, and unrecognized archived Git config.
- Signed commits/tags or embedded merge tags when conversion is needed, pending an explicit signature policy;
  SHA-256 Git repositories; commit/tag bodies above 1 MiB.
- Extended/noncanonical LFS pointers; JSONL/NDJSON, duplicate JSON keys, JSON files
  above 64 MiB, and unknown metadata transformations.

These restrictions may require an archive-specific adapter before the customer
export is usable. Do not relabel an unsupported layout as reviewed to bypass it.
GEI's normal metadata exclusions and follow-up tasks still apply; this tool cannot
promise preservation of data that GEI itself does not migrate.

</details>

## Verification And References

<details>
<summary>Test coverage and upstream documentation</summary>

Tests use real Git/Git LFS with small byte thresholds and local HTTP servers.
They cover hidden/custom/remote refs, exact-threshold behavior, annotated tags,
source immutability, metadata maps, malicious archives, artifact tampering,
API redirects/upload protocol, and LFS batch/payload delivery. They do not prove
customer archive compatibility, real 1 GB resource usage, or live GEI acceptance.

- [REST organization migration API](https://docs.github.com/en/rest/migrations/orgs)
- [Archive upload and GEI API workflow](https://docs.github.com/en/migrations/using-github-enterprise-importer/migrating-between-github-products/migrating-repositories-from-github-enterprise-server-to-github-enterprise-cloud?tool=api)
- [GEI migration scope and limitations](https://docs.github.com/en/migrations/using-github-enterprise-importer/migrating-between-github-products/about-migrations-between-github-products)
- [Original LFS migration procedure](https://gist.github.com/cvega/6cb161bb9c642c9f3d4b232ed4e3f39d)

</details>
