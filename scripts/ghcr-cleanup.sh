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
#   - anything younger than MIN_AGE_HOURS: a build may be pushing it right now
# Deleted: every other version, including untagged leftovers.
#
# Dry run unless DELETE=true: it prints what it would delete and keep.
#
# Env: OWNER, PACKAGE (default kizuna), KEEP_COUNT (10), KEEP_DAYS (30),
# MIN_AGE_HOURS (24), DELETE (false), GH_TOKEN for the gh CLI.
# For tests: VERSIONS_JSON (what the versions API returns) instead of the API,
# MANIFESTS_JSON ({"<digest>": <manifest>, ...}) instead of the registry, NOW
# (epoch seconds) and JQ (another jq, e.g. gojq).
set -euo pipefail

OWNER=${OWNER:?OWNER is required}
PACKAGE=${PACKAGE:-kizuna}
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

registry_token=
# a manifest's JSON from the registry (the package is public: anonymous pull).
# Call it in the main shell (m=$(manifest ...)), never in a pipe or <(...):
# a failed lookup must stop the script, or a kept index looks childless and
# its images get deleted.
manifest() {
  local digest=$1
  if [ -n "${MANIFESTS_JSON:-}" ]; then
    printf '%s' "$MANIFESTS_JSON" | "$JQ" -c --arg d "$digest" '.[$d] // {}'
    return
  fi
  curl -fsS -H "Authorization: Bearer $registry_token" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
    "https://ghcr.io/v2/$image/manifests/$digest"
}

versions=$(list_versions)
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
  registry_token=$(curl -fsS "https://ghcr.io/token?scope=repository:$image:pull" | "$JQ" -er .token) || {
    echo "::error::no registry token for $image: stopping (nothing deleted)" >&2
    exit 1
  }
fi

# follow every kept root to what it needs
declare -A keep=()
queue=()
while read -r d; do queue+=("$d"); done <<< "$roots"
while [ ${#queue[@]} -gt 0 ]; do
  d=${queue[0]}
  queue=("${queue[@]:1}")
  [ -n "${keep[$d]:-}" ] && continue
  keep[$d]=1
  # the per-platform images and BuildKit attestations of an index
  m=$(manifest "$d") || {
    echo "::error::manifest $d: lookup failed (a kept version the registry can't serve?): stopping (nothing deleted)" >&2
    exit 1
  }
  children=$(printf '%s' "$m" | "$JQ" -r '.manifests[]?.digest')
  while read -r child; do
    [ -n "$child" ] && queue+=("$child")
  done <<< "$children"
  # the signed attestation stored under the sha256-<hex> tag
  att=$(printf '%s' "$by_tag" | "$JQ" -r --arg t "sha256-${d#sha256:}" '.[$t] // empty')
  [ -n "$att" ] && queue+=("$att")
done

# everything else that's old enough goes
min_age_before=$((NOW - MIN_AGE_HOURS * 3600))
keep_json=$(printf '%s\n' "${!keep[@]}" | "$JQ" -R . | "$JQ" -s 'map({key: ., value: true}) | from_entries')
plan=$(printf '%s' "$versions" | "$JQ" -c --argjson keep "$keep_json" --argjson before "$min_age_before" '
  def epoch: .created_at | sub("\\.[0-9]+"; "") | fromdateiso8601;
  map({id, name, created_at, tags: (.metadata.container.tags // []),
       keep: ((.name as $n | $keep | has($n)) or epoch > $before)})')

kept=$(printf '%s' "$plan" | "$JQ" '[.[] | select(.keep)] | length')
gone=$(printf '%s' "$plan" | "$JQ" '[.[] | select(.keep | not)] | length')

echo "$image: $total versions, keeping $kept, deleting $gone"
echo "kept tags: $(printf '%s' "$plan" | "$JQ" -r '[.[] | select(.keep) | .tags[] | select(startswith("sha256-") | not)] | sort | join(" ")')"
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
