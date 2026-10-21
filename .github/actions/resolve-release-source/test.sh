#!/usr/bin/env bash
set -euo pipefail

ACTION_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/resolve-release-source.XXXXXX")
trap 'rm -rf -- "${TEST_ROOT:?}"' EXIT

export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL="${TEST_ROOT}/gitconfig"
export GIT_TERMINAL_PROMPT=0
: > "$GIT_CONFIG_GLOBAL"
TEST_COUNT=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  if [ -f "${LOG:-}" ]; then
    cat "$LOG" >&2
  fi
  exit 1
}

pass() {
  TEST_COUNT=$((TEST_COUNT + 1))
  printf 'PASS: %s\n' "$1"
}

new_fixture() {
  local name="$1"
  BRANCH="${2:-release}"
  PREFIX="${3:-release-pending}"
  WORK="${TEST_ROOT}/${name}/work"
  ORIGIN="${TEST_ROOT}/${name}/origin.git"
  OUTPUT="${TEST_ROOT}/${name}/output"
  LOG="${TEST_ROOT}/${name}/log"
  CANDIDATE_REF="refs/heads/${PREFIX}/v1.2.3"

  mkdir -p "${TEST_ROOT}/${name}"
  git init --quiet --bare "$ORIGIN"
  git init --quiet --initial-branch="$BRANCH" "$WORK"
  git -C "$WORK" config user.name "Release resolver test"
  git -C "$WORK" config user.email "resolver@example.invalid"
  git -C "$WORK" config core.autocrlf false
  git -C "$WORK" remote add origin "$ORIGIN"
  printf 'Source\n' > "${WORK}/source.txt"
  git -C "$WORK" add source.txt
  git -C "$WORK" commit --quiet -m "Initial source"
  SOURCE=$(git -C "$WORK" rev-parse HEAD)
  git -C "$WORK" push --quiet origin "HEAD:refs/heads/${BRANCH}"
}

make_candidate() {
  local stored_source="${1:-$SOURCE}"
  git -C "$WORK" checkout --quiet -b "${PREFIX}/v1.2.3" "$SOURCE"
  mkdir -p "${WORK}/.semrelease/pending"
  printf '%s\n' "$stored_source" > "${WORK}/.semrelease/pending/target-sha"
  printf '1.2.3\n' > "${WORK}/.semrelease/pending/version"
  printf 'Saved release notes\n' > "${WORK}/.semrelease/pending/release-notes.md"
  git -C "$WORK" add .semrelease/pending
  git -C "$WORK" commit --quiet -m "Record pending release"
  CANDIDATE=$(git -C "$WORK" rev-parse HEAD)
  git -C "$WORK" push --quiet origin "HEAD:${CANDIDATE_REF}"
  git -C "$WORK" checkout --quiet "$BRANCH"
}

advance_release() {
  printf 'Newer work\n' >> "${WORK}/source.txt"
  git -C "$WORK" commit --quiet -am "Newer release work"
  LATEST=$(git -C "$WORK" rev-parse HEAD)
  git -C "$WORK" push --quiet origin "HEAD:refs/heads/${BRANCH}"
}

run_resolver() {
  : > "$OUTPUT"
  if (
    cd "$WORK"
    BRANCH_TO_RELEASE="$BRANCH" \
      PENDING_BRANCH_PREFIX="$PREFIX" \
      GITHUB_OUTPUT="$OUTPUT" \
      bash "${ACTION_DIR}/resolve.sh"
  ) > "$LOG" 2>&1; then
    STATUS=0
  else
    STATUS=$?
  fi
}

expect_success() {
  [ "$STATUS" -eq 0 ] || fail "Resolver exited with status ${STATUS}"
  mapfile -t OUTPUT_LINES < "$OUTPUT"
  [ "${#OUTPUT_LINES[@]}" -eq 4 ] || fail "Expected exactly four resolver outputs"
}

expect_failure() {
  [ "$STATUS" -ne 0 ] || fail "Resolver unexpectedly succeeded"
  [ ! -s "$OUTPUT" ] || fail "Failed resolution emitted outputs"
  grep -Fq -- "$1" "$LOG" || fail "Expected failure message: $1"
}

expect_output() {
  grep -Fxq -- "$1" "$OUTPUT" || fail "Expected output: $1"
}

new_fixture new
advance_release
git -C "$WORK" checkout --quiet -b workflow "$SOURCE"
run_resolver
expect_success
expect_output "mode=new"
expect_output "source_sha=${LATEST}"
expect_output "candidate_ref="
expect_output "candidate_sha="
[ "$(git -C "$WORK" rev-parse HEAD)" = "$SOURCE" ] ||
  fail "Resolution changed the caller's checkout"
pass "New release resolves the remote branch head without changing the checkout"

new_fixture resume
make_candidate
advance_release
run_resolver
expect_success
expect_output "mode=resume"
expect_output "source_sha=${SOURCE}"
expect_output "candidate_ref=${CANDIDATE_REF}"
expect_output "candidate_sha=${CANDIDATE}"
[ "$(git -C "$WORK" rev-parse HEAD)" = "$LATEST" ] ||
  fail "Resume changed the caller's checkout"
pass "Pending candidate takes priority over newer release commits"

new_fixture custom stable pending/stable
make_candidate
run_resolver
expect_success
expect_output "mode=resume"
expect_output "source_sha=${SOURCE}"
expect_output "candidate_ref=${CANDIDATE_REF}"
expect_output "candidate_sha=${CANDIDATE}"
pass "Custom release branch and pending prefix are respected"

new_fixture multiple
make_candidate
git -C "$WORK" push --quiet origin "${CANDIDATE}:refs/heads/${PREFIX}/v2.0.0"
run_resolver
expect_failure "More than one pending release candidate exists"
pass "Multiple pending candidates are rejected"

new_fixture inconsistent
make_candidate "not-the-parent"
run_resolver
expect_failure "The pending release candidate target is inconsistent"
pass "Stored target must match the candidate parent"

new_fixture missing-target
CANDIDATE=$(printf 'Missing metadata\n' |
  git -C "$WORK" commit-tree "${SOURCE}^{tree}" -p "$SOURCE")
git -C "$WORK" push --quiet origin "${CANDIDATE}:${CANDIDATE_REF}"
run_resolver
expect_failure ".semrelease/pending/target-sha"
pass "Missing target metadata is rejected"

for shape in root merge; do
  new_fixture "$shape"
  PARENTS=()
  if [ "$shape" = merge ]; then
    advance_release
    PARENTS=(-p "$SOURCE" -p "$LATEST")
  fi
  CANDIDATE=$(printf '%s candidate\n' "$shape" |
    git -C "$WORK" commit-tree "${SOURCE}^{tree}" "${PARENTS[@]}")
  git -C "$WORK" push --quiet origin "${CANDIDATE}:${CANDIDATE_REF}"
  run_resolver
  expect_failure "The pending release candidate must have exactly one parent"
  pass "${shape} candidate with an invalid parent count is rejected"
done

new_fixture orphaned-source
make_candidate
UNRELATED=$(printf 'Unrelated history\n' |
  git -C "$WORK" commit-tree "${SOURCE}^{tree}")
git -C "$WORK" push --quiet origin "${UNRELATED}:refs/heads/unrelated"
git --git-dir="$ORIGIN" update-ref "refs/heads/${BRANCH}" "$UNRELATED"
run_resolver
expect_failure "The pending release target is no longer on ${BRANCH}"
pass "Pending source must remain in the release branch history"

new_fixture moved
make_candidate
REPLACEMENT=$(printf 'Moved candidate\n' |
  git -C "$WORK" commit-tree "${CANDIDATE}^{tree}" -p "$CANDIDATE")
git -C "$WORK" push --quiet origin "${REPLACEMENT}:refs/heads/replacement"
export TEST_ORIGIN="$ORIGIN"
export TEST_CANDIDATE_REF="$CANDIDATE_REF"
export TEST_REPLACEMENT="$REPLACEMENT"
git() {
  if [ "${1:-}" = fetch ] &&
     [ "${4:-}" = "${TEST_CANDIDATE_REF}:refs/remotes/origin/release-candidate" ]; then
    command git --git-dir="$TEST_ORIGIN" update-ref \
      "$TEST_CANDIDATE_REF" "$TEST_REPLACEMENT"
  fi
  command git "$@"
}
export -f git
run_resolver
unset -f git
expect_failure "The pending release candidate changed while it was being resolved"
pass "Candidate changes between listing and fetching are rejected"

new_fixture failed-lookup
make_candidate
git() {
  if [ "${1:-}" = ls-remote ]; then
    printf 'Simulated candidate lookup failure\n' >&2
    return 128
  fi
  command git "$@"
}
export -f git
run_resolver
unset -f git
expect_failure "Simulated candidate lookup failure"
pass "A failed lookup cannot fall back to a new release"

new_fixture failed-fetch
git() {
  if [ "${1:-}" = fetch ]; then
    printf 'Simulated release fetch failure\n' >&2
    return 128
  fi
  command git "$@"
}
export -f git
run_resolver
unset -f git
expect_failure "Simulated release fetch failure"
pass "A failed release fetch stops resolution"

new_fixture required-input
BRANCH=""
run_resolver
expect_failure "release-branch must not be empty"
pass "Release branch must be explicitly provided"
BRANCH="release"
PREFIX=""
run_resolver
expect_failure "pending-branch-prefix must not be empty"
pass "Pending branch prefix must be explicitly provided"

new_fixture invalid-input
PREFIX="*"
run_resolver
expect_failure "Invalid pending-branch-prefix"
BRANCH="invalid branch"
PREFIX="release-pending"
run_resolver
expect_failure "Invalid release-branch"
pass "Invalid branch and prefix inputs are rejected"

printf 'Passed %s release source resolver tests.\n' "$TEST_COUNT"
