# Project conventions

- Go 1.26+, using the latest patch of an upstream-supported release; standard library; Git and Git LFS are runtime dependencies.
- Work from migration archives, never substitute a network clone for hidden refs.
- The customer's feature flag permits 1 GB Git blobs. Default to 1,000,000,000 bytes; allow an explicit byte threshold for the flag's exact boundary. Do not enforce the public 400 MiB limit for this workflow.
- Keep source archives immutable. Only rewrite disposable extracted copies.
- Preserve all archived refs and use an explicit commit map for metadata references.
- Fail closed on unsupported archive layouts and unreviewed metadata transformations.
- Never log PATs or signed archive URLs. Never push rewritten Git refs to the source.
- Imports target a new private staging repository. Production promotion is out of scope.
- Keep the README short and detailed procedures under docs/.
- Validate changes with go test ./..., go test -race ./..., and go vet ./....
