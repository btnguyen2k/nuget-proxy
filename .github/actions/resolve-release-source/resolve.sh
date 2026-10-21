#!/usr/bin/env bash
set -euo pipefail

: "${BRANCH_TO_RELEASE:?release-branch must not be empty}"
: "${PENDING_BRANCH_PREFIX:?pending-branch-prefix must not be empty}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT must be set}"

if ! git check-ref-format "refs/heads/${BRANCH_TO_RELEASE}"; then
  echo "❌Invalid release-branch: ${BRANCH_TO_RELEASE}" >&2
  exit 1
fi
if ! git check-ref-format "refs/heads/${PENDING_BRANCH_PREFIX}/candidate"; then
  echo "❌Invalid pending-branch-prefix: ${PENDING_BRANCH_PREFIX}" >&2
  exit 1
fi

# A pending candidate always wins over newer branch commits so a
# partially published version keeps one immutable target and plan.
# Capture the pipeline status so a failed lookup cannot look like an empty list.
CANDIDATE_LIST=$(
  git ls-remote --heads origin \
    "refs/heads/${PENDING_BRANCH_PREFIX}/*" |
    sort -k2
)
CANDIDATES=()
if [ -n "${CANDIDATE_LIST}" ]; then
  mapfile -t CANDIDATES <<< "${CANDIDATE_LIST}"
fi

if [ "${#CANDIDATES[@]}" -gt 1 ]; then
  echo "❌More than one pending release candidate exists:"
  printf '  %s\n' "${CANDIDATES[@]}"
  exit 1
fi

if [ "${#CANDIDATES[@]}" -eq 1 ]; then
  CANDIDATE_SHA=$(awk '{print $1}' <<< "${CANDIDATES[0]}")
  CANDIDATE_REF=$(awk '{print $2}' <<< "${CANDIDATES[0]}")

  git fetch --no-tags origin \
    "${CANDIDATE_REF}:refs/remotes/origin/release-candidate"

  FETCHED_CANDIDATE=$(git rev-parse refs/remotes/origin/release-candidate)
  if [ "${FETCHED_CANDIDATE}" != "${CANDIDATE_SHA}" ]; then
    echo "❌The pending release candidate changed while it was being resolved."
    exit 1
  fi

  PARENT_COUNT=$(
    git rev-list --parents -n 1 "${CANDIDATE_SHA}" |
      awk '{ print NF - 1 }'
  )
  if [ "${PARENT_COUNT}" -ne 1 ]; then
    echo "❌The pending release candidate must have exactly one parent."
    exit 1
  fi

  SOURCE_SHA=$(git rev-parse "${CANDIDATE_SHA}^")
  STORED_SOURCE=$(
    git show \
      "${CANDIDATE_SHA}:.semrelease/pending/target-sha" |
      tr -d '\r\n'
  )

  if [ "${STORED_SOURCE}" != "${SOURCE_SHA}" ]; then
    echo "❌The pending release candidate target is inconsistent."
    exit 1
  fi

  git fetch --no-tags origin "refs/heads/${BRANCH_TO_RELEASE}"
  RELEASE_HEAD=$(git rev-parse FETCH_HEAD)

  if ! git merge-base --is-ancestor \
       "${SOURCE_SHA}" "${RELEASE_HEAD}"; then
    echo "❌The pending release target is no longer on ${BRANCH_TO_RELEASE}."
    exit 1
  fi

  echo "mode=resume" >> "$GITHUB_OUTPUT"
  echo "source_sha=${SOURCE_SHA}" >> "$GITHUB_OUTPUT"
  echo "candidate_ref=${CANDIDATE_REF}" >> "$GITHUB_OUTPUT"
  echo "candidate_sha=${CANDIDATE_SHA}" >> "$GITHUB_OUTPUT"
  echo "ℹ️Resuming pending release candidate ${CANDIDATE_REF}."
else
  git fetch --no-tags origin "refs/heads/${BRANCH_TO_RELEASE}"
  SOURCE_SHA=$(git rev-parse FETCH_HEAD)

  echo "mode=new" >> "$GITHUB_OUTPUT"
  echo "source_sha=${SOURCE_SHA}" >> "$GITHUB_OUTPUT"
  echo "candidate_ref=" >> "$GITHUB_OUTPUT"
  echo "candidate_sha=" >> "$GITHUB_OUTPUT"
  echo "✅Preparing a release from ${BRANCH_TO_RELEASE}@${SOURCE_SHA}."
fi
