#!/usr/bin/env bash
#
# Decides the next version and writes its release notes.
#
# Reads the commits since the most recent v* tag and applies conventional-commit
# rules: a breaking change bumps major, a feat bumps minor, anything else bumps
# patch. Below 1.0.0 a breaking change bumps minor instead — a stray `!` should
# not declare the API stable. Pass patch|minor|major to override the rules.
#
# Preview what a push to main would publish, without touching anything:
#
#   NOTES_FILE=/tmp/notes.md .github/scripts/release-plan.sh && cat /tmp/notes.md
#
# Prints KEY=value lines (previous, version, bump, has_changes) on stdout, which
# is the shape GitHub Actions wants appended to $GITHUB_OUTPUT.

set -euo pipefail

forced_bump="${1:-auto}"
notes_file="${NOTES_FILE:-release-notes.md}"
repo_url="${REPO_URL:-$(git remote get-url origin | sed -e 's/\.git$//' -e 's#^git@github.com:#https://github.com/#')}"

case "$forced_bump" in
  auto | patch | minor | major) ;;
  *) echo "usage: $0 [auto|patch|minor|major]" >&2; exit 2 ;;
esac

# --sort=-v:refname orders tags by version rather than lexically, so v0.10.0
# sorts above v0.9.0 instead of below it.
previous="$(git tag --list 'v*' --sort=-v:refname | head -n1)"

log_args=(--no-merges)
if [ -n "$previous" ]; then
  log_args+=("${previous}..HEAD")
fi
# With no tags yet, the range is left open and the whole history is release one.

subjects="$(git log --format='%s' "${log_args[@]}")"
entries="$(git log --format='%h%x09%s' "${log_args[@]}")"

if [ -z "$subjects" ]; then
  # Nothing new since the last tag. The caller skips the release and the deploy
  # rather than publishing an empty version.
  echo "previous=${previous}"
  echo "version=${previous}"
  echo "bump=none"
  echo "has_changes=false"
  : >"$notes_file"
  exit 0
fi

# --- which part of the version moves ------------------------------------------

breaking=false
# Breaking changes announce themselves two ways: `feat!:` in the subject, or a
# BREAKING CHANGE trailer in the body. Both count.
if printf '%s\n' "$subjects" | grep -qE '^[a-zA-Z]+(\([^)]*\))?!:'; then
  breaking=true
elif git log --format='%B' "${log_args[@]}" | grep -qE '^BREAKING[ -]CHANGE'; then
  breaking=true
fi

if [ "$forced_bump" != auto ]; then
  bump="$forced_bump"
elif [ "$breaking" = true ]; then
  bump=major
elif printf '%s\n' "$subjects" | grep -qE '^feat(\([^)]*\))?!?:'; then
  bump=minor
else
  bump=patch
fi

base="${previous#v}"
[ -n "$base" ] || base="0.0.0"
IFS=. read -r major minor patch <<<"$base"

# Pre-1.0 the major number is not a stability promise, so an automatic major
# bump lands on minor. An explicit `major` override still cuts 1.0.0.
if [ "$major" -eq 0 ] && [ "$bump" = major ] && [ "$forced_bump" = auto ]; then
  bump=minor
fi

case "$bump" in
  major) major=$((major + 1)); minor=0; patch=0 ;;
  minor) minor=$((minor + 1)); patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac
version="v${major}.${minor}.${patch}"

# --- the notes ----------------------------------------------------------------

# Each section pulls the subjects matching (or, with -v, not matching) a type
# prefix. Commits that follow no convention still land in "Other changes", so
# nothing published goes unmentioned.
section() {
  local title="$1" pattern="$2" invert="${3:-}" body re
  # awk processes string escapes in a -v assignment before the regex engine sees
  # the value, which would eat the backslash in \( and turn a literal paren into
  # a group. Doubling them here keeps the patterns above readable.
  re="${pattern//\\/\\\\}"
  body="$(printf '%s\n' "$entries" |
    awk -F'\t' -v re="$re" -v inv="$invert" \
      '(inv == "" && $2 ~ re) || (inv != "" && $2 !~ re) {printf "- %s (%s)\n", $2, $1}')"
  [ -n "$body" ] || return 0
  printf '### %s\n\n%s\n\n' "$title" "$body"
}

{
  section 'Breaking changes' '^[a-zA-Z]+(\([^)]*\))?!:'
  section 'Features' '^feat(\([^)]*\))?!?:'
  section 'Fixes' '^fix(\([^)]*\))?!?:'
  section 'Performance' '^perf(\([^)]*\))?!?:'
  section 'Other changes' '^(feat|fix|perf)(\([^)]*\))?!?:' invert

  if [ -n "$previous" ]; then
    printf '**Full changelog**: %s/compare/%s...%s\n' "$repo_url" "$previous" "$version"
  else
    printf '**Full changelog**: %s/commits/%s\n' "$repo_url" "$version"
  fi
} >"$notes_file"

echo "previous=${previous}"
echo "version=${version}"
echo "bump=${bump}"
echo "has_changes=true"
