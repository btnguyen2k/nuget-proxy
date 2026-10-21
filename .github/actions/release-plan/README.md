# Release plan

Local composite action that calculates a new release plan or reloads the saved
plan for an unfinished release. It never publishes packages, tags, or GitHub
releases.

Use a Linux runner with Bash and Git. Check out the workflow revision before
calling the local action. The action then checks out `source-sha` with full
history, installs Node LTS only for a new plan, and returns normalized outputs.

```yaml
steps:
  - uses: actions/checkout@v7
    with:
      ref: ${{ github.workflow_sha }}
      fetch-depth: 0
  - name: Plan release
    id: plan
    uses: ./.github/actions/release-plan
    with:
      mode: ${{ needs.ResolveReleaseSource.outputs.MODE }}
      source-sha: ${{ needs.ResolveReleaseSource.outputs.SOURCE_SHA }}
      release-branch: ${{ env.BRANCH_TO_RELEASE }}
      github-token: ${{ secrets.GITHUB_TOKEN }}
      tag-prefix: ${{ env.TAG_PREFIX }}
      candidate-ref: ${{ needs.ResolveReleaseSource.outputs.CANDIDATE_REF }}
      candidate-sha: ${{ needs.ResolveReleaseSource.outputs.CANDIDATE_SHA }}
```

| Input | Required | Meaning |
|---|---|---|
| `mode` | Yes | `new` calculates a plan; `resume` loads an existing one. |
| `source-sha` | Yes | Full 40-character source commit SHA from `resolve-release-source`. |
| `release-branch` | Yes | Release branch for semantic-release calculation. |
| `github-token` | Yes | Token with `contents: read` access for checkout and planning. |
| `tag-prefix` | No | Release tag prefix; defaults to `v`. |
| `candidate-ref` | On resume | Full pending branch ref from `resolve-release-source`. |
| `candidate-sha` | On resume | Expected pending metadata commit SHA. |

Required inputs have no defaults. In resume mode, the action verifies the pending
ref still points to `candidate-sha`, then loads the saved version and release
notes without recalculating them. Candidate structure and source ancestry are
validated by `resolve-release-source` before this action.

| Output | Meaning |
|---|---|
| `result` | `SUCCESS` permits publication; other results do not. |
| `version` | Calculated or saved version. |
| `release_notes` | Calculated or saved multiline release notes. |

Keep CI dependencies, event conditions, and source/candidate passthrough outputs
on the calling job. The `ReleasePlan` job in `release-full.yaml` demonstrates this.

Shell steps intentionally remain inline. Checking out an older source may remove
the action directory, but GitHub has already loaded its composite steps and
output mappings. Do not replace these steps with scripts read from the action
directory after checkout.

Run the metadata and shell integration tests from the repository root:

```sh
go test ./.github/actions/release-plan
```

Tests require Go, Bash, and Git, use temporary local repositories, and do not
contact GitHub. On Windows, the tests use Git Bash and substitute the Linux UUID
source used for multiline output delimiters.
