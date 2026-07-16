#!/usr/bin/env bash
#
# Pre-commit hook: the repository's quality gates, run before a commit exists
# rather than after CI has already rejected it.
#
# Install with `make install-hooks`. Bypass a single commit with
# `git commit --no-verify` when you know what you are doing (a WIP commit on a
# scratch branch); CI runs these anyway, so bypassing only defers the answer.
#
# Design notes, since they are easy to get wrong:
#
#   * These checks REPORT, they do not FIX. A hook that runs `go fmt` rewrites
#     files after git has already snapshotted the index, so the commit records
#     the unformatted version and the fix dangles in the working tree. Telling
#     you to run `make fmt` is slower to type and impossible to get wrong.
#
#   * Tests are deliberately NOT here. `make test` takes minutes, and a hook
#     that takes minutes is a hook everyone passes --no-verify to. These checks
#     are the fast ones; CI owns the slow ones.

set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 1

readonly BOLD=$'\033[1m'
readonly RED=$'\033[0;31m'
readonly GREEN=$'\033[0;32m'
readonly YELLOW=$'\033[0;33m'
readonly RESET=$'\033[0m'

failed=0

step() {
    printf '%s==>%s %s\n' "$BOLD" "$RESET" "$1"
}

fail() {
    printf '%s  FAIL%s %s\n' "$RED" "$RESET" "$1"
    failed=1
}

pass() {
    printf '%s  ok%s   %s\n' "$GREEN" "$RESET" "$1"
}

# Nothing staged means nothing to check -- e.g. `git commit` during a merge
# resolution with an empty index.
if git diff --cached --quiet; then
    printf '%s==>%s nothing staged, skipping checks\n' "$BOLD" "$RESET"
    exit 0
fi

# Only look at Go files that are actually part of this commit. Added, copied,
# modified, renamed -- not deleted, which have nothing left to format. Read into
# an array (-z / NUL-delimited) so a path containing a space survives.
staged_go=()
while IFS= read -r -d '' f; do
    case "$f" in
    *.go) staged_go+=("$f") ;;
    esac
done < <(git diff --cached --name-only --diff-filter=ACMR -z)

# ---------------------------------------------------------------------------
step "gofmt"
if [ "${#staged_go[@]}" -eq 0 ]; then
    pass "no Go files staged"
else
    unformatted="$(gofmt -s -l "${staged_go[@]}" 2>/dev/null || true)"
    if [ -n "$unformatted" ]; then
        fail "these staged files are not gofmt'd:"
        printf '%s\n' "$unformatted" | sed 's/^/           /'
        printf '         run: %smake fmt%s, then stage the result\n' "$BOLD" "$RESET"
    else
        pass "staged Go files are formatted"
    fi
fi

# ---------------------------------------------------------------------------
step "go build"
if build_out="$(go build ./... 2>&1)"; then
    pass "builds"
else
    fail "the tree does not build:"
    printf '%s\n' "$build_out" | head -20 | sed 's/^/           /'
fi

# ---------------------------------------------------------------------------
step "go vet"
if vet_out="$(go vet ./... 2>&1)"; then
    pass "vet clean"
else
    fail "go vet:"
    printf '%s\n' "$vet_out" | head -20 | sed 's/^/           /'
fi

# ---------------------------------------------------------------------------
# Local-only files: planning notes, IDE config, operator data, build output.
# .gitignore cannot enforce this on a path git already tracks, which is exactly
# how .planning/ and .idea/ ended up in the repository. Guarded by existence so
# the hook still works on branches predating the script.
step "tracked files"
if [ -x ./scripts/check-tracked-files.sh ]; then
    if tracked_out="$(./scripts/check-tracked-files.sh 2>&1)"; then
        pass "no local-only files tracked"
    else
        fail "local-only files are tracked:"
        printf '%s\n' "$tracked_out" | sed 's/^/           /'
    fi
else
    printf '%s  skip%s scripts/check-tracked-files.sh not present on this branch\n' "$YELLOW" "$RESET"
fi

# ---------------------------------------------------------------------------
# Last, because it is by far the slowest.
step "golangci-lint"
if ! command -v golangci-lint >/dev/null 2>&1; then
    printf '%s  skip%s golangci-lint not installed -- CI will run it\n' "$YELLOW" "$RESET"
elif lint_out="$(golangci-lint run 2>&1)"; then
    pass "lint clean"
else
    fail "golangci-lint:"
    printf '%s\n' "$lint_out" | head -30 | sed 's/^/           /'
fi

# ---------------------------------------------------------------------------
echo
if [ "$failed" -ne 0 ]; then
    printf '%sCommit refused.%s Fix the above, or use %sgit commit --no-verify%s to bypass.\n' \
        "$RED" "$RESET" "$BOLD" "$RESET"
    exit 1
fi

printf '%sAll checks passed.%s\n' "$GREEN" "$RESET"
exit 0
