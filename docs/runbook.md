# Refit: Archive-First Staging Migration

## Scope And Prerequisites

This adapts the [LFS migration procedure](https://gist.github.com/cvega/6cb161bb9c642c9f3d4b232ed4e3f39d)
to migration archives. A network mirror clone cannot substitute for archived
hidden history. The source stays unchanged; only newly extracted disposable
copies are rewritten. Production promotion is outside this tool's scope.

Use a trusted machine, private local storage, and enough space for the originals,
extractions, temporary Git packs, LFS payloads, rewritten archives, and a validation
extraction. Do not run concurrent operations against the same workspace. Build
the CLI using the README command. Commands below assume it is on your PATH.

Provide `GH_SOURCE_PAT` and `GH_PAT` through your secure environment or secret
manager. Do not place tokens in command arguments, URLs, policy files, or shell
history. API errors suppress server bodies and signed download URLs. Use approved
source/destination endpoints; setting an API endpoint authorizes sending that
endpoint the corresponding token.

The source can be GHES or GHEC; the destination is GHEC. For GHES, set
`SOURCE_API=https://HOST/api/v3`; for GitHub.com, use
`SOURCE_API=https://api.github.com`. The public API documentation describes archive
ingestion for GHES. Confirm the custom rewritten-archive route, especially a GHEC
export ingested as `GITHUB_ARCHIVE`, with the customer's migration team before
using `-archive-route-reviewed`. That flag records operator approval, not proof
of importer compatibility.

Confirm the customer's 1 GB feature flag is enabled for the destination import.
The default threshold is **1,000,000,000 bytes**, inclusive as an allowed size.
Use `-threshold-bytes` if its exact boundary differs. No public 400 MiB restriction
is applied. Separately confirm the destination's LFS single-object and storage
limits; moving a blob to LFS does not exempt it from LFS limits.

## 1. Export And Download

Coordinate a quiet source window for the two exports. The CLI deliberately does
not lock the source. Separate exports are not an atomic snapshot; reject a pair
if source changes or metadata refers to commits missing from the Git archive.

```sh
refit export -source-api "$SOURCE_API" -org SOURCE_ORG -repo REPO -kind git
refit export -source-api "$SOURCE_API" -org SOURCE_ORG -repo REPO -kind metadata
```

Record both returned IDs. Check each until its state is `exported`:

```sh
refit export-status -source-api "$SOURCE_API" -org SOURCE_ORG -export-id GIT_ID
refit export-status -source-api "$SOURCE_API" -org SOURCE_ORG -export-id METADATA_ID
refit download -source-api "$SOURCE_API" -org SOURCE_ORG -export-id GIT_ID -out original-git.tar.gz
refit download -source-api "$SOURCE_API" -org SOURCE_ORG -export-id METADATA_ID -out original-metadata.tar.gz
```

Replace capitalized placeholders with actual values. Downloads require new files,
stream to disk, and do not forward the source PAT to redirected archive storage.
Retain originals under your normal immutable storage controls. The CLI never
deletes remote exports or unlocks repositories.

## 2. Inspect And Review The Layout

```sh
refit inspect -git-archive original-git.tar.gz -work inspection
```

This extracts to a new directory, lists each detected bare repository's refs and
oversized blob OIDs/sizes, and scans all stored Git objects, not just branch tips.
Review hidden refs such as `refs/pull/*` against the source export's expected
contents. This tool cannot recover a ref that was not exported.

Inspection does not modify Git history. Default extraction budgets are 200 GiB
decompressed bytes per archive and 2,000,000 logical members. Adjust the
`-max-extracted-bytes` and `-max-files` flags deliberately for larger exports.
Never overwrite a previous work directory; use a new name.

Review the metadata archive's JSON schema using a trusted archive viewer. Do not
assume REST response field names equal migration archive fields. Review every
commit-bearing field, including PR base/head/merge SHAs, review commit IDs,
original review positions, commit comments, and commit URLs. References to commits
outside the exported repository must be understood, not silently reassigned.

## 3. Approve Metadata Rules

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

## 4. Prepare Rewritten Archives

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

Repacking retains surviving member paths, order, and tar headers. LFS payloads,
hooks, and reflogs are excluded from the Git archive. Local payloads remain for
the later upload. The rewritten Git archive is re-extracted and its refs and
blob sizes checked. Originals are hashed before and after preparation.

`report.json` appears only after successful preparation. It is a locally trusted
manifest, not a signature or defense against an attacker who can edit both report
and artifacts. Keep the whole workspace private and immutable between review and
staging. Verification detects changed output archives, commit-map bytes, local
refs, oversized objects, and missing/corrupt LFS payloads.

## 5. Import Into Private Staging

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

Use the same `-target-api` when checking a nondefault destination. A successful
GEI import does not mean LFS content is available yet.

## 6. Upload LFS And Review

```sh
refit lfs-push -work prepared -confirm-staging
```

This reads the saved staging identity, requires GEI state `SUCCEEDED`, checks the
destination is the expected private repository, verifies local objects, and
uploads the explicit SHA-256 object list, including objects found only in hidden
history. It never runs `git push`. Destination authentication is supplied through
the subprocess environment, not arguments or archived credential helpers. A
successful upload writes `prepared/lfs-uploaded.json`.

Review before sign-off:

1. Read the destination Migration Log issue and resolve importer warnings/errors.
2. Compare branches, tags, default branch, and available hidden-ref evidence with
   the report. Importer handling of hidden refs requires live validation.
3. Check PR/issue/review counts, authors, comments, open and closed PRs, diffs,
   deleted branches, and review anchors on rewritten commits.
4. Clone the **destination for validation only**, run `git fsck --full`, fetch
   historical LFS payloads using `git lfs fetch --all`, and run LFS checks. Check
   representative historical revisions, not only the default branch. Hidden-only
   content may require explicit PR/ref access or GitHub-assisted validation.
5. Confirm both existing and newly converted LFS payloads can be downloaded and
   their contents match the SHA-256 pointer hashes in the preparation report.
6. Review permissions and disabled automation. Record staging approval; do not
   promote or redirect users as part of this tool.

## Recovery And Limitations

A failed preparation may leave a diagnostic extraction but no successful report.
Correct the layout/policy/cache issue and run preparation into a **new** work
directory from the originals. Never reuse a half-rewritten extraction.

`staging-attempt.json` is created before remote writes. A second stage attempt
against that work directory is blocked. If a request times out, inspect migration
status and destination activity before retrying: a lost response may follow a
successful enqueue. Do not blindly remove the attempt file. Uploaded archives and
unused migration sources may need operator cleanup. Export, enqueue, and upload
operations are not automatically retried. LFS upload itself can be rerun.

Supported today: separate single-stream tar.gz archives, regular files/directories,
one extracted SHA-1 files-backend bare Git repository, and reviewed JSON metadata.
Unsupported layouts fail closed rather than dropping refs or guessing:

- Git bundles, nested compressed archives (including compressed attachments),
  combined Git/metadata input, multiple repositories/wikis, worktrees and gitfiles.
- Archive links/special files, duplicate/case-colliding paths, shallow/partial
  repositories, alternates, grafts, replace/notes refs, detached HEAD, noncommit
  refs, symbolic non-HEAD refs, and unrecognized archived Git config.
- Signed commits/tags or embedded merge tags pending an explicit signature policy;
  SHA-256 Git repositories; commit/tag bodies above 1 MiB.
- Extended/noncanonical LFS pointers; JSONL/NDJSON, duplicate JSON keys, JSON files
  above 64 MiB, and unknown metadata transformations.

These restrictions may require an archive-specific adapter before the customer
export is usable. Do not relabel an unsupported layout as reviewed to bypass it.
GEI's normal metadata exclusions and follow-up tasks still apply; this tool cannot
promise preservation of data that GEI itself does not migrate.

## Verification And References

Tests use real Git/Git LFS with small byte thresholds and local HTTP servers.
They cover hidden/custom/remote refs, exact-threshold behavior, annotated tags,
source immutability, metadata maps, malicious archives, artifact tampering,
API redirects/upload protocol, and LFS batch/payload delivery. They do not prove
customer archive compatibility, real 1 GB resource usage, or live GEI acceptance.

- [REST organization migration API](https://docs.github.com/en/rest/migrations/orgs)
- [Archive upload and GEI API workflow](https://docs.github.com/en/migrations/using-github-enterprise-importer/migrating-between-github-products/migrating-repositories-from-github-enterprise-server-to-github-enterprise-cloud?tool=api)
- [GEI migration scope and limitations](https://docs.github.com/en/migrations/using-github-enterprise-importer/migrating-between-github-products/about-migrations-between-github-products)
