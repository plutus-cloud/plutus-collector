#!/usr/bin/env bash
# Fails when text that will be published (the PR title and body, commit messages and added lines)
# matches a pattern that does not belong in a public repository.
#
# Two pattern sources:
#   - built-in: tool-attribution trailers and footers, below.
#   - PUBLIC_TEXT_DENYLIST: newline-separated extended regexes, supplied as a repository secret so
#     the list itself is not published. Only the file, line and pattern number are reported.
#
# Usage: check-public-text.sh <base-ref> <head-ref>   (PR_TITLE and PR_BODY come from the environment)
set -euo pipefail

base=${1:?base ref}
head=${2:?head ref}

builtin=(
  '^Co-authored-by:.*(claude|anthropic|copilot|openai|chatgpt|gemini)'
  'Generated with \[?(Claude|Copilot|ChatGPT)'
)

patterns=()
for p in "${builtin[@]}"; do patterns+=("$p"); done
if [ -n "${PUBLIC_TEXT_DENYLIST:-}" ]; then
  while IFS= read -r line; do
    [ -n "$line" ] && patterns+=("$line")
  done <<<"$PUBLIC_TEXT_DENYLIST"
else
  echo "::notice::PUBLIC_TEXT_DENYLIST is not available (fork pull request or unset); only built-in patterns are checked."
fi

fail=0
check() { # label, text
  local label=$1 text=$2 i=0
  for p in "${patterns[@]}"; do
    i=$((i + 1))
    if grep -qiE -- "$p" <<<"$text"; then
      echo "::error::${label} matches public-text pattern #${i}"
      fail=1
    fi
  done
}

check "PR title" "${PR_TITLE:-}"
check "PR body" "${PR_BODY:-}"
check "commit messages" "$(git log --format=%B "${base}..${head}")"

# Added lines only, with file and line numbers, so existing text is not re-flagged.
while IFS= read -r hit; do
  file=${hit%%:*}; rest=${hit#*:}; line=${rest%%:*}; text=${rest#*:}
  i=0
  for p in "${patterns[@]}"; do
    i=$((i + 1))
    if grep -qiE -- "$p" <<<"$text"; then
      echo "::error file=${file},line=${line}::added line matches public-text pattern #${i}"
      fail=1
    fi
  done
done < <(git diff -U0 --no-color "${base}...${head}" -- . ':!scripts/check-public-text.sh' \
  | awk '/^\+\+\+ b\//{f=substr($0,7)} /^@@/{split($3,a,","); n=substr(a[1],2)-1} /^\+[^+]/{n++; print f":"n":"substr($0,2)}')

exit "$fail"
