#!/usr/bin/env bash
# Cut an aiscan client release: pick the next version for a train, decide whether it should move the
# "latest" download pointer, and push the tag that triggers the release workflow.
#
# Interactive by default: anything you don't pass on the command line is prompted for, with a smart
# default pre-filled - press Enter to accept it or type an override.
#
#   scripts/release.sh                         # prompts for everything
#   scripts/release.sh desktop                 # prompts for version (suggests next patch) + latest
#   scripts/release.sh desktop 0.3.0           # only prompts to confirm
#   scripts/release.sh desktop --not-latest    # prompts for version, forces not-latest
#   scripts/release.sh desktop 0.3.0 --yes     # fully non-interactive (CI / scripting)
#
# Each client is its own release train with its own tag prefix (desktop-v*, extension-v*); no
# release uses a bare vX.Y.Z tag.
#
# The "latest" decision drives the download pointer. Each train has a `<train>-latest` GitHub release
# whose body names the versioned tag it points at; the download server (pulse aiscan/downloads.py)
# reads ONLY that pointer - it never scans releases. So:
#   - latest  -> the release workflow moves the pointer to this version; it's the download you get.
#   - not-latest -> the workflow leaves the pointer alone. The release still ships (people can grab
#       it by tag) but new installs keep getting the current latest. Two cases want this:
#       * a back-port hotfix on an old line (desktop-0.1.x while 0.2.x is current);
#       * yanking: cutting a version you do NOT want to become the default download yet.
#
# On a maintenance branch (e.g. desktop-0.1.x / extension-1.2.x) the suggested version is the next
# patch on THAT line, defaulting to not-latest whenever a newer minor already exists - a back-port
# must not silently become the version new users download.
#
# The decision is carried to CI in the annotated tag message ("latest: false"); the release workflow
# reads it and skips the pointer move. A lightweight (non-annotated) tag reads as latest.
set -euo pipefail

die() { echo "release: error: $*" >&2; exit 1; }

# Prompt with a default; Enter accepts it, anything typed overrides. Prompt goes to stderr so the
# chosen value is the only thing on stdout (safe inside $(...)).
ask() {
  local reply
  printf '%s [%s]: ' "$1" "${2:-}" >&2
  read -r reply || true
  echo "${reply:-${2:-}}"
}

# Yes/no with a default (y or n); returns 0 for yes. Enter accepts the default.
ask_yesno() {
  local reply hint
  [ "$2" = y ] && hint="[Y/n]" || hint="[y/N]"
  printf '%s %s: ' "$1" "$hint" >&2
  read -r reply || true
  case "${reply:-$2}" in [yY] | [yY][eE][sS]) return 0 ;; *) return 1 ;; esac
}

# --- parse args: [train] [version] plus flags, all optional ---
TRAIN=""
VERSION=""
LATEST_OVERRIDE="" # "", "true", or "false"
ASSUME_YES=0
positional=()
for arg in "$@"; do
  case "$arg" in
    --latest) LATEST_OVERRIDE=true ;;
    --not-latest) LATEST_OVERRIDE=false ;;
    --yes | -y) ASSUME_YES=1 ;;
    -*) die "unknown flag '$arg'" ;;
    *) positional+=("$arg") ;;
  esac
done
[ "${#positional[@]}" -ge 1 ] && TRAIN="${positional[0]}"
[ "${#positional[@]}" -ge 2 ] && VERSION="${positional[1]}"
[ "${#positional[@]}" -ge 3 ] && die "too many arguments: ${positional[*]}"

interactive=1
[ "$ASSUME_YES" = 1 ] && interactive=0

branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "")"

# --- resolve train (arg, else inferred-from-branch default, else prompt) ---
if [ -z "$TRAIN" ]; then
  default_train=""
  [[ "$branch" =~ ^(desktop|extension)- ]] && default_train="${BASH_REMATCH[1]}"
  if [ "$interactive" = 1 ]; then
    TRAIN="$(ask "Which train? (desktop/extension)" "$default_train")"
  else
    TRAIN="$default_train"
  fi
fi
case "$TRAIN" in
  desktop | extension) ;;
  *) die "train must be 'desktop' or 'extension' (got '${TRAIN:-<empty>}')" ;;
esac
PREFIX="${TRAIN}-v"

git fetch --tags --quiet 2>/dev/null || echo "release: warning: could not fetch tags; suggestions use local tags only" >&2

# Strip the prefix and version-sort; tail is the newest. Empty when the train has no releases yet.
train_versions() { git tag -l "${PREFIX}${1:-}*" | sed "s|^${PREFIX}||" | sort -V; }
highest="$(train_versions | tail -1)"

# Detect a maintenance branch like desktop-0.1.x / extension-1.2.x -> we're cutting on a hotfix line.
line="" # e.g. "0.1" when on a maintenance branch, else empty
[[ "$branch" =~ ^${TRAIN}-([0-9]+\.[0-9]+)\.x$ ]] && line="${BASH_REMATCH[1]}"

bump_patch() {
  local IFS=. major minor patch
  read -r major minor patch <<<"$1"
  echo "${major}.${minor}.$((patch + 1))"
}

# Suggested next version: next patch on the maintenance line, else next patch of the train's highest.
if [ -n "$line" ]; then
  line_highest="$(train_versions "${line}." | tail -1)"
  suggested="$([ -n "$line_highest" ] && bump_patch "$line_highest" || echo "${line}.0")"
elif [ -n "$highest" ]; then
  suggested="$(bump_patch "$highest")"
else
  suggested=""
fi

# --- resolve version (arg, else prompt with the suggestion as default) ---
if [ -z "$VERSION" ]; then
  if [ "$interactive" = 1 ]; then
    VERSION="$(ask "Version" "$suggested")"
  else
    VERSION="$suggested"
  fi
fi
[ -n "$VERSION" ] || die "no ${PREFIX}* tags yet - pass an explicit version, e.g. $TRAIN 0.1.0"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version must be X.Y.Z (got '$VERSION')"
TAG="${PREFIX}${VERSION}"
git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null 2>&1 && die "tag ${TAG} already exists"

# Guess latest: this version is the newest the train has ever seen (`sort -V | tail` picks the max).
if [ -z "$highest" ] || [ "$(printf '%s\n%s\n' "$VERSION" "$highest" | sort -V | tail -1)" = "$VERSION" ]; then
  latest_guess=true
else
  latest_guess=false # a back-port below the current highest
fi

# --- resolve latest (flag override, else prompt with the guess as default, else the guess) ---
if [ -n "$LATEST_OVERRIDE" ]; then
  latest="$LATEST_OVERRIDE"
elif [ "$interactive" = 1 ]; then
  if ask_yesno "Move the ${TRAIN}-latest pointer to ${VERSION}?" "$([ "$latest_guess" = true ] && echo y || echo n)"; then
    latest=true
  else
    latest=false
  fi
else
  latest="$latest_guess"
fi

echo "release: train      ${TRAIN}"
echo "release: version    ${VERSION}   (current latest: ${highest:-none})"
[ -n "$line" ] && echo "release: branch     ${branch}  (maintenance line ${line}.x)"
if [ "$latest" = true ]; then
  echo "release: latest     yes - moves the ${TRAIN}-latest download pointer to ${VERSION}"
else
  echo "release: latest     no  - ${TRAIN}-latest pointer stays at ${highest:-none}"
fi

if [ "$ASSUME_YES" != 1 ]; then
  ask_yesno "Tag and push ${TAG}?" y || die "aborted"
fi

msg="aiscan ${TRAIN} ${VERSION}"
[ "$latest" = false ] && msg="${msg}"$'\n\n'"latest: false"
git tag -a "$TAG" -m "$msg"
git push origin "$TAG"
echo "release: pushed ${TAG} - watch the release workflow in GitHub Actions"
