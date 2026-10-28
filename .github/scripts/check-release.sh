#!/usr/bin/env bash
set -euo pipefail

: "${RELEASE_TAG:?}" "${RELEASE_COMMIT:?}" "${GH_REPO:?}"
actual=$(gh api "repos/$GH_REPO/git/ref/tags/$RELEASE_TAG" --jq '.object.sha')
if [[ "$actual" != "$RELEASE_COMMIT" ]]; then
  echo "::error::发布标签已移动；拒绝构建或发布与已通过 CI 的提交不一致的版本"
  exit 1
fi
release_file=$(mktemp)
error_file=$(mktemp)
trap 'rm -f "$release_file" "$error_file"' EXIT
if gh release view "$RELEASE_TAG" --json isDraft,targetCommitish > "$release_file" 2> "$error_file"; then
  if ! jq -e --arg commit "$RELEASE_COMMIT" '.isDraft == true and .targetCommitish == $commit' "$release_file" > /dev/null; then
    echo "::error::正式版本不可覆盖，或草稿属于其他提交；请新发起 Run workflow 分配下一版本"
    exit 1
  fi
elif ! grep -qi 'release not found\|HTTP 404' "$error_file"; then
  cat "$error_file" >&2
  exit 1
fi
