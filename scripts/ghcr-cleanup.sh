#!/usr/bin/env bash
# Deletes old sha-<short> builds from the GitHub Container Registry package,
# with everything that belongs to them. Run by the ghcr-cleanup workflow.
#
# Kept:
#   - every version with a tag other than sha-*: dev, latest, 1.2.3, 1.2, ...
#   - the newest KEEP_COUNT sha-* versions, and any sha-* newer than KEEP_DAYS
#   - everything a kept version needs: the per-platform images and BuildKit
#     attestations in its index, and its signed attestation (the version
#     tagged sha256-<its digest>), followed recursively
#   - an untagged version whose manifest names a kept version as its subject
#     (an attestation stored the OCI referrers way, with no sha256-* tag), and
#     everything it needs in turn (one the registry answers 404 for has no
#     subject: it goes, with a warning)
#   - anything younger than MIN_AGE_HOURS: a build may be pushing it right now
# Deleted: every other version, including untagged leftovers.
#
# Dry run unless DELETE=true: it prints what it would delete and keep.
#
# Env: OWNER, PACKAGE (default: the repository name from GITHUB_REPOSITORY,
# else kizuna; lower-cased like the image name), KEEP_COUNT (10), KEEP_DAYS (30),
# MIN_AGE_HOURS (24), DELETE (false), GH_TOKEN for the gh CLI.
# For tests: VERSIONS_JSON (what the versions API returns) instead of the API,
# MANIFESTS_JSON ({"<digest>": <manifest>, ...}) instead of the registry, NOW
# (epoch seconds) and JQ (another jq, e.g. gojq).
set -euo pipefail

OWNER=${OWNER:?OWNER is required}
repo=${GITHUB_REPOSITORY:-}
repo=${repo#*/}
PACKAGE=${PACKAGE:-${repo:-kizuna}}
PACKAGE=${PACKAGE,,}
KEEP_COUNT=${KEEP_COUNT:-10}
KEEP_DAYS=${KEEP_DAYS:-30}
MIN_AGE_HOURS=${MIN_AGE_HOURS:-24}
DELETE=${DELETE:-false}
NOW=${NOW:-$(date +%s)}
JQ=${JQ:-jq}

api="/users/$OWNER/packages/container/$PACKAGE/versions"
image="${OWNER,,}/$PACKAGE"

# all versions as one JSON array
list_versions() {
  if [ -n "${VERSIONS_JSON:-}" ]; then
    printf '%s' "$VERSIONS_JSON"
  else
    gh api --paginate "$api?per_page=100" | "$JQ" -s 'add // []'
  fi
}

# Each registry call gives up after a minute (10 s to connect), like a failed
# one: the run stops with nothing deleted, and the next scheduled run retries.
# (A manifest read tries three times, so at most about 3 minutes.)
CURL_TIMEOUTS=(--connect-timeout 10 --max-time 60)

registry_token=
ACCEPT='Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'
# a manifest's JSON from the registry (the package is public: anonymous pull).
# Call it in the main shell (m=$(manifest ...)), never in a pipe or <(...):
# a failed lookup must stop the script, or a kept index looks childless and
# its images get deleted. Two retries: one passing 5xx or timeout in a few
# hundred reads shouldn't fail the month's run (curl retries timeouts, 408,
# 429 and 5xx, never a 404; --max-time is per attempt).
manifest() {
  local digest=$1
  if [ -n "${MANIFESTS_JSON:-}" ]; then
    printf '%s' "$MANIFESTS_JSON" | "$JQ" -c --arg d "$digest" '.[$d] // {}'
    return
  fi
  curl -fsS --retry 2 "${CURL_TIMEOUTS[@]}" -H "Authorization: Bearer $registry_token" -H "$ACCEPT" \
    "https://ghcr.io/v2/$image/manifests/$digest"
}

# the HTTP status the registry answers for a manifest, once, after a failed
# manifest call (curl -f fails the same way for a 404 and a 5xx)
manifest_status() {
  curl -sS -o /dev/null -w '%{http_code}' "${CURL_TIMEOUTS[@]}" \
    -H "Authorization: Bearer $registry_token" -H "$ACCEPT" \
    "https://ghcr.io/v2/$image/manifests/$1"
}

# a page that fails ends the listing (pipefail): what came before it is no
# whole list, and versions missing from it can't be followed as kept
versions=$(list_versions) || {
  echo "::error::listing the versions of $image failed: stopping (nothing deleted)" >&2
  exit 1
}
total=$(printf '%s' "$versions" | "$JQ" length)
if [ "$total" -eq 0 ]; then
  echo "no versions found for $image: nothing to do"
  exit 0
fi

# the roots to keep: release/dev tags, then the newest or recent sha-* builds
keep_days_ago=$((NOW - KEEP_DAYS * 86400))
roots=$(printf '%s' "$versions" | "$JQ" -r --argjson n "$KEEP_COUNT" --argjson since "$keep_days_ago" '
  def tags: .metadata.container.tags // [];
  def epoch: .created_at | sub("\\.[0-9]+"; "") | fromdateiso8601;
  ( map(select(any(tags[]; test("^sha(256)?-") | not))) )
  + ( map(select(any(tags[]; startswith("sha-")) and all(tags[]; test("^sha(256)?-"))))
      | sort_by(.created_at) | reverse
      | to_entries | map(select(.key < $n or (.value | epoch) >= $since)) | map(.value) )
  | map(.name) | unique | .[]')

if [ -z "$roots" ]; then
  echo "::error::nothing to keep was found (no dev, release or sha-* tags): stopping" >&2
  exit 1
fi

# digest -> version id, and sha256-<hex> attestation tag -> its digest
by_tag=$(printf '%s' "$versions" | "$JQ" -c '
  [ .[] | .name as $d | (.metadata.container.tags // [])[] | select(startswith("sha256-")) | {key: ., value: $d} ]
  | from_entries')

# one registry token for every manifest lookup (-e: no token stops here)
if [ -z "${MANIFESTS_JSON:-}" ]; then
  registry_token=$(curl -fsS "${CURL_TIMEOUTS[@]}" "https://ghcr.io/token?scope=repository:$image:pull" | "$JQ" -er .token) || {
    echo "::error::no registry token for $image: stopping (nothing deleted)" >&2
    exit 1
  }
fi

# digest -> version id, for the messages (a deletion by hand needs the id)
declare -A id_of=()
while read -r d id; do
  if [ -n "$d" ]; then id_of[$d]=$id; fi
done <<< "$(printf '%s' "$versions" | "$JQ" -r '.[] | "\(.name) \(.id)"')"
# "sha256:... (version 12)", or "(not listed)" for a part the package doesn't list
version_of() { echo "$1 (version ${id_of[$1]:-not listed})"; }

# every manifest read so far, by digest: each one is fetched once a run
declare -A manifests=()
# read_manifest DIGEST WHY [missing-ok]: sets M to the manifest, a JSON
# object. A lookup that fails stops the run (WHY says what the version is): a
# manifest nobody could read must not turn into a delete. The one exception,
# with missing-ok: when the registry answers exactly 404, it returns 1 and
# the caller decides.
read_manifest() {
  local m code
  if [ -z "${manifests[$1]+x}" ]; then
    if ! m=$(manifest "$1"); then
      if [ "${3:-}" = missing-ok ]; then
        code=$(manifest_status "$1") || code=
        if [ "$code" = 404 ]; then return 1; fi
      fi
      echo "::error::manifest $(version_of "$1"): lookup failed ($2): stopping (nothing deleted)" >&2
      exit 1
    fi
    if ! printf '%s' "$m" | "$JQ" -e 'type == "object"' > /dev/null 2>&1; then
      echo "::error::manifest $(version_of "$1"): the registry's answer is not a manifest ($2): stopping (nothing deleted)" >&2
      exit 1
    fi
    manifests[$1]=$m
  fi
  M=${manifests[$1]}
}

# follow every digest in the queue to what it needs, keeping it all
declare -A keep=()
queue=()
follow() {
  local d children child att
  while [ ${#queue[@]} -gt 0 ]; do
    d=${queue[0]}
    queue=("${queue[@]:1}")
    if [ -n "${keep[$d]:-}" ]; then continue; fi
    keep[$d]=1
    # the per-platform images and BuildKit attestations of an index
    read_manifest "$d" "a kept version the registry can't serve?"
    children=$(printf '%s' "$M" | "$JQ" -r '.manifests[]?.digest')
    while read -r child; do
      if [ -n "$child" ]; then queue+=("$child"); fi
    done <<< "$children"
    # the signed attestation stored under the sha256-<hex> tag
    att=$(printf '%s' "$by_tag" | "$JQ" -r --arg t "sha256-${d#sha256:}" '.[$t] // empty')
    if [ -n "$att" ]; then queue+=("$att"); fi
  done
}
while read -r d; do queue+=("$d"); done <<< "$roots"
follow

# An attestation stored the OCI referrers way has no tag: only its manifest's
# subject says which image it belongs to. So every untagged version old enough
# to go, and not kept yet, is read once for its subject (a younger one stays
# anyway). One whose subject is kept is kept too, with what it needs, until
# nothing more is: an attestation of a kept attestation stays as well.
min_age_before=$((NOW - MIN_AGE_HOURS * 3600))
candidates=$(printf '%s' "$versions" | "$JQ" -r --argjson before "$min_age_before" '
  def epoch: .created_at | sub("\\.[0-9]+"; "") | fromdateiso8601;
  .[] | select((.metadata.container.tags // []) == [] and epoch <= $before) | .name')
declare -A subject_of=()
while read -r d; do
  if [ -z "$d" ] || [ -n "${keep[$d]:-}" ]; then continue; fi
  # A version the registry answers 404 for has no manifest, so no subject:
  # it goes, as untagged leftovers did before subjects were read. That is
  # safe: with no manifest it can't be an attestation anyone can fetch, and
  # the kept roots were read above with the same token, so the 404 is no
  # missing permission. Any other failure still stops the run, and so does a
  # 404 for a part of a kept version (in follow).
  if ! read_manifest "$d" "an untagged version, read for its subject" missing-ok; then
    echo "::warning::manifest $(version_of "$d"): the registry has none (404): no subject to keep it by, so it is deleted like any untagged leftover" >&2
    continue
  fi
  s=$(printf '%s' "$M" | "$JQ" -r '.subject.digest? // empty')
  if [ -n "$s" ]; then subject_of[$d]=$s; fi
done <<< "$candidates"
grew=true
while [ "$grew" = true ]; do
  grew=false
  for d in "${!subject_of[@]}"; do
    if [ -z "${keep[$d]:-}" ] && [ -n "${keep[${subject_of[$d]}]:-}" ]; then
      queue+=("$d")
      grew=true
    fi
  done
  follow
done

# everything else that's old enough goes
keep_json=$(printf '%s\n' "${!keep[@]}" | "$JQ" -R . | "$JQ" -s 'map({key: ., value: true}) | from_entries')
plan=$(printf '%s' "$versions" | "$JQ" -c --argjson keep "$keep_json" --argjson before "$min_age_before" '
  def epoch: .created_at | sub("\\.[0-9]+"; "") | fromdateiso8601;
  map({id, name, created_at, tags: (.metadata.container.tags // []),
       keep: ((.name as $n | $keep | has($n)) or epoch > $before)})')

kept=$(printf '%s' "$plan" | "$JQ" '[.[] | select(.keep)] | length')
gone=$(printf '%s' "$plan" | "$JQ" '[.[] | select(.keep | not)] | length')

echo "$image: $total versions, keeping $kept, deleting $gone"
echo "kept tags: $(printf '%s' "$plan" | "$JQ" -r '[.[] | select(.keep) | .tags[] | select(startswith("sha256-") | not)] | sort | join(" ")')"
echo "manifests read: ${#manifests[@]}"
printf '%s' "$plan" | "$JQ" -r '[.[] | select(.keep | not)] | sort_by(.tags == []) | .[]
  | "delete \(.id) \(.name[0:19]) \(.created_at) \(if .tags == [] then "(untagged)" else .tags | join(",") end)"'

if [ "$gone" -eq "$total" ]; then
  echo "::error::this would delete every version: stopping" >&2
  exit 1
fi
if [ "$DELETE" != true ]; then
  echo "dry run: nothing deleted (DELETE=true deletes)"
  exit 0
fi

# tagged versions first: if a run stops halfway, what's left is untagged
# leftovers the next run deletes
printf '%s' "$plan" | "$JQ" -r '[.[] | select(.keep | not)] | sort_by(.tags == []) | .[].id' |
  while read -r id; do
    gh api -X DELETE "$api/$id" > /dev/null
    echo "deleted $id"
  done
