#!/usr/bin/env bash
# Cut an aiscan client release: suggest the next version for a train, decide whether it should
# move the "latest" download pointer, and push the tag that triggers the release workflow.
#
# Usage:
#   scripts/release.sh <desktop|extension> [version] [--latest|--not-latest] [--yes]
#
# Each client is its own release train with its own tag prefix (desktop-v*, extension-v*); no
# release uses a bare vX.Y.Z tag. With no version this suggests the next patch. Pass a version
# (e.g. 0.3.0) for a minor/major bump.
#
# The "latest" decision drives the download pointer. Each train has a `<train>-latest` GitHub
# release whose body names the versioned tag it points at; the download server (pulse
# aiscan/downloads.py) reads ONLY that pointer - it never scans releases. So:
#   - latest  -> the release workflow moves the pointer to this version; it's the download you get.
#   - not-latest -> the workflow leaves the pointer alone. The release still ships (people can grab
#       it by tag) but new installs keep getting the current latest. Two cases want this:
#       * a back-port hotfix on an old line (desktop-0.1.x while 0.2.x is current);
#       * yanking: cutting a version you do NOT want to become the default download yet.
#
# On a maintenance branch (e.g. desktop-0.1.x / extension-1.2.x) this suggests the next patch on
# THAT line and defaults to --not-latest whenever a newer minor already exists - a back-port must
# not silently become the version new users download. Override the guess with --latest/--not-latest.
#
# The decision is carried to CI in the annotated tag message ("latest: false"); the release workflow
# reads it and skips the pointer move. A lightweight (non-annotated) tag reads as latest.
set -euo pipefail

die() { echo "release: error: $*" >&2; exit 1; }

TRAIN="${1:-}"
[ -n "$TRAIN" ] && shift || true
case "$TRAIN" in
  desktop | extension) ;;
  *) die "first arg must be 'desktop' or 'extension' (got '${TRAIN}')" ;;
esac
PREFIX="${TRAIN}-v"

VERSION=""
LATEST_OVERRIDE="" # "", "true", or "false"
ASSUME_YES=0
for arg in "$@"; do
  case "$arg" in
    --latest) LATEST_OVERRIDE=true ;;
    --not-latest) LATEST_OVERRIDE=false ;;
    --yes | -y) ASSUME_YES=1 ;;
    -*) die "unknown flag '$arg'" ;;
    *) [ -z "$VERSION" ] || die "version given twice ('$VERSION' and '$arg')"; VERSION="$arg" ;;
  esac
done

git fetch --tags --quiet 2>/dev/null || echo "release: warning: could not fetch tags; suggestions use local tags only" >&2

# Strip the prefix and version-sort; tail is the newest. Empty when the train has no releases yet.
train_versions() { git tag -l "${PREFIX}${1:-}*" | sed "s|^${PREFIX}||" | sort -V; }
highest="$(train_versions | tail -1)"

# Detect a maintenance branch like desktop-0.1.x / extension-1.2.x -> we're cutting on a hotfix line.
branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "")"
line="" # e.g. "0.1" when on a maintenance branch, else empty
if [[ "$branch" =~ ^${TRAIN}-([0-9]+\.[0-9]+)\.x$ ]]; then
  line="${BASH_REMATCH[1]}"
fi

bump_patch() {
  local IFS=. major minor patch
  read -r major minor patch <<<"$1"
  echo "${major}.${minor}.$((patch + 1))"
}

if [ -z "$VERSION" ]; then
  if [ -n "$line" ]; then
    line_highest="$(train_versions "${line}." | tail -1)"
    VERSION="$([ -n "$line_highest" ] && bump_patch "$line_highest" || echo "${line}.0")"
  else
    [ -n "$highest" ] || die "no ${PREFIX}* tags yet - pass an explicit version, e.g. $TRAIN 0.1.0"
    VERSION="$(bump_patch "$highest")"
  fi
fi

[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version must be X.Y.Z (got '$VERSION')"
TAG="${PREFIX}${VERSION}"
git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null 2>&1 && die "tag ${TAG} already exists"

# Guess latest: this version is the newest the train has ever seen. `sort -V | tail` picks the max;
# if that's our version (and it isn't an existing tag, checked above) it should be latest.
if [ -z "$highest" ] || [ "$(printf '%s\n%s\n' "$VERSION" "$highest" | sort -V | tail -1)" = "$VERSION" ]; then
  latest_guess=true
else
  latest_guess=false # a back-port below the current highest
fi
latest="${LATEST_OVERRIDE:-$latest_guess}"

echo "release: train      ${TRAIN}"
echo "release: version     ${VERSION}   (current latest: ${highest:-none})"
[ -n "$line" ] && echo "release: branch      ${branch}  (maintenance line ${line}.x)"
if [ "$latest" = true ]; then
  echo "release: latest      yes - moves the ${TRAIN}-latest download pointer to ${VERSION}"
else
  echo "release: latest      no  - ${TRAIN}-latest pointer stays at ${highest:-none}"
fi
[ -n "$LATEST_OVERRIDE" ] && echo "release: (latest overridden on the command line; guess was ${latest_guess})"

if [ "$ASSUME_YES" != 1 ]; then
  printf 'release: tag and push %s? [y/N] ' "$TAG"
  read -r reply
  case "$reply" in [yY] | [yY][eE][sS]) ;; *) die "aborted" ;; esac
fi

msg="aiscan ${TRAIN} ${VERSION}"
[ "$latest" = false ] && msg="${msg}"$'\n\n'"latest: false"
git tag -a "$TAG" -m "$msg"
git push origin "$TAG"
echo "release: pushed ${TAG} - watch the release workflow in GitHub Actions"
