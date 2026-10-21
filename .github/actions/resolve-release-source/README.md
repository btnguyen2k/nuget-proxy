# Resolve release source

Local composite action that chooses a new release source or resumes a pending
release. It reads Git refs and metadata; it does not publish anything, change the
working tree, or create or delete remote branches.

Check out the repository with full history before invoking the action. The
checkout must include this action and have read access to the `origin` remote.
Checking out the workflow's SHA keeps the local action aligned with the workflow
revision; the release source is resolved independently from the remote branch.

```yaml
steps:
  - uses: actions/checkout@v7
    with:
      ref: ${{ github.workflow_sha }}
      fetch-depth: 0
  - name: Resolve release candidate
    id: resolve
    uses: ./.github/actions/resolve-release-source
    with:
      release-branch: release
      pending-branch-prefix: release-pending
```

Both inputs are required, have no defaults, and must not be empty.

| Input | Required | Meaning |
|---|---|---|
| `release-branch` | Yes | Branch whose remote head supplies a new release source. |
| `pending-branch-prefix` | Yes | Literal prefix for pending branches, without a trailing slash. |

| Output | Meaning |
|---|---|
| `mode` | `new` if no pending branch exists, otherwise `resume`. |
| `source_sha` | Latest release-branch commit for `new`; candidate parent for `resume`. |
| `candidate_ref` | Full pending branch ref; empty for `new`. |
| `candidate_sha` | Pending metadata commit SHA; empty for `new`. |

A pending candidate takes priority over newer release commits. Resolution fails
if multiple candidates exist, the candidate moves during resolution, it does not
have exactly one parent, its `.semrelease/pending/target-sha` disagrees with that
parent, or the source is no longer an ancestor of the release branch head.
Failed Git lookups and fetches also fail resolution instead of selecting a new
release.

Run the integration tests with Bash 4.4+, Git, and standard Unix tools:

```sh
bash .github/actions/resolve-release-source/test.sh
```

The tests use temporary local repositories and never contact GitHub or modify
the caller's repository.
