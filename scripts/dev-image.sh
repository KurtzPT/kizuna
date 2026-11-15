#!/usr/bin/env bash
# What a ci run on dev does with the :dev image. Run from the repository root
# by ci.yml (job gate) and docker.yml (job publish).
#
#   dev-image.sh revision  the commit :dev was built from: the revision label
#                          of the published image (docker.yml's metadata step
#                          writes it), read without a login (the package is
#                          public). Prints nothing when there is no :dev or no
#                          such label; fails when the registry can't be read
#   dev-image.sh publish   (gate) does this commit get published? Writes
#                          publish=true|false to $GITHUB_OUTPUT. Whenever the
#                          answer isn't certain: true
#   dev-image.sh move      (publish) may :dev move to this commit? Writes
#                          dev=true|false to $GITHUB_OUTPUT: false when :dev is
#                          built from a newer commit, so a re-run of an old run
#                          can't move it back. Fails when the registry can't be
#                          read twice: nothing may be pushed then
#   dev-image.sh paths     what changes the image, as path filters (docker.yml's
#                          push filter repeats them; dev-image_test.sh compares)
#
# Env: GITHUB_REPOSITORY, GITHUB_SHA, GITHUB_OUTPUT (as on a runner), JQ
# (another jq, e.g. gojq). Uses curl, git and jq. Tested by dev-image_test.sh.
set -euo pipefail

JQ=${JQ:-jq}

# Besides what .dockerignore lets into the build context, these define the
# build: a change to one builds a different image too.
BUILD_FILES=(Dockerfile .dockerignore .github/workflows/docker.yml .github/actions/build-image)

# One line per file or folder that changes the image: .dockerignore's "!"
# lines, then BUILD_FILES. Anything but "*" and then "!<name>" lines is an
# error: a pattern here would need a matcher this script doesn't have.
image_entries() {
  local line first=true
  while IFS= read -r line || [ -n "$line" ]; do
    line=${line%$'\r'}
    case $line in '' | '#'*) continue ;; esac
    if $first; then
      if [ "$line" != '*' ]; then
        echo "::error file=.dockerignore::it must be an allowlist: \"*\" first, not \"$line\"" >&2
        return 1
      fi
      first=false
    elif [[ $line =~ ^![A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$ ]]; then
      echo "${line#!}"
    else
      echo "::error file=.dockerignore::only \"!<file or folder>\" lines after \"*\" (scripts/dev-image.sh reads them), not \"$line\"" >&2
      return 1
    fi
  done < .dockerignore
  if $first; then
    echo "::error file=.dockerignore::empty: it must be an allowlist (\"*\", then \"!<file or folder>\" lines)" >&2
    return 1
  fi
  printf '%s\n' "${BUILD_FILES[@]}"
}

# touches_image FILE...: does any of them change the image? Needs $entries.
touches_image() {
  local f e
  for f; do
    for e in "${entries[@]}"; do
      if [ "$f" = "$e" ] || [[ $f == "$e"/* ]]; then return 0; fi
    done
  done
  return 1
}

dev_revision() {
  local image=${GITHUB_REPOSITORY,,} accept out token auth digest rev
  accept='Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'
  out=$(curl -fsS --max-time 30 "https://ghcr.io/token?scope=repository:$image:pull") || return 1
  token=$("$JQ" -er .token <<< "$out") || return 1
  auth="Authorization: Bearer $token"
  # the tag's index: one image per platform. 404: no :dev yet
  out=$(curl -sS --max-time 30 -w '\n%{http_code}' -H "$auth" -H "$accept" "https://ghcr.io/v2/$image/manifests/dev") || return 1
  case ${out##*$'\n'} in
    200) ;;
    404) return 0 ;;
    *) echo "ghcr.io answered ${out##*$'\n'} for :dev" >&2; return 1 ;;
  esac
  # every platform is built from the same commit: one is enough
  digest=$("$JQ" -er 'first(.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64")).digest' <<< "${out%$'\n'*}") || return 1
  out=$(curl -fsS --max-time 30 -H "$auth" -H "$accept" "https://ghcr.io/v2/$image/manifests/$digest") || return 1
  digest=$("$JQ" -er .config.digest <<< "$out") || return 1
  # the labels are in the image's config
  out=$(curl -fsSL --max-time 30 -H "$auth" "https://ghcr.io/v2/$image/blobs/$digest") || return 1
  rev=$("$JQ" -r '.config.Labels["org.opencontainers.image.revision"] // ""' <<< "$out") || return 1
  if [[ $rev =~ ^[0-9a-f]{40}$ ]]; then echo "$rev"; fi
}

cmd_publish() {
  local list built publish=false changed=() f
  # read first: a broken .dockerignore fails the run whatever :dev is
  list=$(image_entries)
  mapfile -t entries <<< "$list"
  # Compared with the commit :dev was built from, not the one before this
  # push: a waiting run that a newer push replaced never published, and its
  # changes must still reach :dev.
  if ! built=$(dev_revision); then
    echo "::notice::couldn't read which commit :dev was built from (see above): publishing"
    publish=true
  elif [ -z "$built" ]; then
    echo "::notice::there is no :dev image that names its commit: publishing"
    publish=true
  elif [ "$built" = "$GITHUB_SHA" ]; then
    echo "::notice:::dev is already built from this commit: not publishing"
  # a force push, or a re-run of an older run
  elif ! git merge-base --is-ancestor "$built" "$GITHUB_SHA" 2> /dev/null; then
    echo "::notice:::dev was built from $built, which isn't in this commit's history: publishing"
    publish=true
  else
    # -z: names exactly as they are (no quoting of unusual characters);
    # --no-renames: a file moved out of the image counts where it was
    mapfile -d '' -t changed < <(git diff --name-only --no-renames -z "$built" "$GITHUB_SHA")
    wait "$!"
    echo "changed files since $built (:dev):"
    for f in "${changed[@]}"; do echo "  $f"; done
    # an empty diff (only empty commits) publishes rather than guess
    if ((${#changed[@]} == 0)) || touches_image "${changed[@]}"; then
      publish=true
    else
      echo "::notice::nothing that goes into the image changed since :dev was built: not publishing"
    fi
  fi
  echo "publish: $publish"
  echo "publish=$publish" >> "$GITHUB_OUTPUT"
}

cmd_move() {
  local built dev=true
  # Not "is this commit still dev's head": a docs-only push after this one
  # starts no ci run, and :dev would stay behind.
  # once more after a pause: one bad answer shouldn't fail the run
  if ! built=$(dev_revision) && { sleep 10; ! built=$(dev_revision); }; then
    echo "::error::couldn't read which commit :dev was built from (see above; the package must be public), so this could move it back: nothing pushed. Run the failed jobs again"
    return 1
  fi
  # newer: this run's commit is in its history
  if [ -n "$built" ] && [ "$built" != "$GITHUB_SHA" ] &&
    git merge-base --is-ancestor "$GITHUB_SHA" "$built" 2> /dev/null; then
    echo "::notice:::dev is built from $built, a newer commit than this run's: pushing only sha-${GITHUB_SHA::7}, :dev stays"
    dev=false
  fi
  echo "dev: $dev"
  echo "dev=$dev" >> "$GITHUB_OUTPUT"
}

cmd_paths() {
  local list e
  list=$(image_entries)
  while IFS= read -r e; do
    if [ -d "$e" ]; then echo "$e/**"; else echo "$e"; fi
  done <<< "$list"
}

case ${1:-} in
  revision)
    : "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
    dev_revision
    ;;
  publish | move)
    : "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
    : "${GITHUB_SHA:?GITHUB_SHA is required}"
    : "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"
    "cmd_$1"
    ;;
  paths) cmd_paths ;;
  *)
    echo "usage: dev-image.sh revision|publish|move|paths" >&2
    exit 2
    ;;
esac
