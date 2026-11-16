#!/usr/bin/env bash
# Behaviour tests for ghcr-cleanup.sh: which versions a run keeps and deletes,
# on a made-up package (no GitHub API, no registry: gh and curl are faked).
#
#   bash scripts/ghcr-cleanup_test.sh       (JQ=gojq to use gojq)

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=$HERE/ghcr-cleanup.sh
CHECKS=0 FAILS=0

# check NAME GOT WANT
check() {
	CHECKS=$((CHECKS + 1))
	if [[ $2 != "$3" ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL %s\n  got:  %q\n  want: %q\n' "$1" "$2" "$3"
	fi
}

# version ID DIGEST CREATED TAG...: one entry of the versions API, comma after
version() {
	local id=$1 name=$2 created=$3 tags=
	shift 3
	(($#)) && tags=$(printf '"%s",' "$@")
	printf '{"id":%s,"name":"%s","created_at":"%s","metadata":{"container":{"tags":[%s]}}},' \
		"$id" "$name" "$created" "${tags%,}"
}

# list ENTRIES: the version() entries as a JSON array
list() { printf '[%s]' "${1%,}"; }

# NOW is 2026-11-09T00:00:00Z, as a number: date -d is GNU-only
run() {
	OWNER=Someone PACKAGE=kizuna KEEP_COUNT=2 KEEP_DAYS=30 MIN_AGE_HOURS=24 \
		NOW=1794182400 \
		VERSIONS_JSON="$1" MANIFESTS_JSON="$2" bash "$SCRIPT" 2>&1
}

# dev (an index with one image, plus its signed attestation, itself an index),
# a release, three sha-* builds (the oldest beyond KEEP_COUNT and KEEP_DAYS,
# with an image and an attestation of its own), a fresh untagged upload and an
# old untagged leftover
versions=$(list "$(
	version 1 sha256:dev 2026-09-01T00:00:00Z dev sha-aaa
	version 2 sha256:devimg 2026-09-01T00:00:00Z
	version 3 sha256:devatt 2026-09-01T00:00:00Z sha256-dev
	version 4 sha256:devattimg 2026-09-01T00:00:00Z
	version 5 sha256:rel 2026-08-01T00:00:00Z 1.0.0 1.0 latest sha-rrr
	version 6 sha256:old 2026-09-02T00:00:00Z sha-bbb
	version 7 sha256:oldimg 2026-09-02T00:00:00Z
	version 8 sha256:oldatt 2026-09-02T00:00:00Z sha256-old
	version 9 sha256:mid 2026-09-03T00:00:00Z sha-ccc
	version 10 sha256:new 2026-11-08T12:00:00Z sha-ddd
	version 11 sha256:fresh 2026-11-08T20:00:00Z
	version 12 sha256:leftover 2026-10-01T00:00:00Z
)")
manifests='{
  "sha256:dev": {"manifests": [{"digest": "sha256:devimg"}]},
  "sha256:devatt": {"manifests": [{"digest": "sha256:devattimg"}]},
  "sha256:old": {"manifests": [{"digest": "sha256:oldimg"}]}
}'

out=$(run "$versions" "$manifests")
check "dry run: exit" "$?" 0
check "dry run: counts" "$(grep -c '^delete ' <<< "$out")" 4
check "dry run: deleted, tagged first" \
	"$(sed -nE 's/^delete ([0-9]+) .*/\1/p' <<< "$out" | tr '\n' ' ')" "6 8 7 12 "
check "dry run: summary" "$(head -n 1 <<< "$out")" \
	"someone/kizuna: 12 versions, keeping 8, deleting 4"
check "dry run: kept tags" "$(sed -n 's/^kept tags: //p' <<< "$out")" \
	"1.0 1.0.0 dev latest sha-aaa sha-ccc sha-ddd sha-rrr"
check "dry run: nothing deleted" "$(tail -n 1 <<< "$out")" \
	"dry run: nothing deleted (DELETE=true deletes)"

# with KEEP_COUNT=2 and recent builds, only the old leftover goes
out=$(run "$(list "$(version 1 sha256:a 2026-11-01T00:00:00Z sha-a)$(version 2 sha256:b 2026-10-01T00:00:00Z)")" '{}')
check "untagged leftover only" "$(sed -nE 's/^delete ([0-9]+) .*/\1/p' <<< "$out")" 2

# nothing to keep: stop rather than delete everything
out=$(run "$(list "$(version 1 sha256:a 2026-01-01T00:00:00Z sha256-x)")" '{}')
check "nothing to keep: exit" "$?" 1
check "nothing to keep: message" "$(tail -n 1 <<< "$out")" \
	"::error::nothing to keep was found (no dev, release or sha-* tags): stopping"

out=$(run '[]' '{}')
check "empty package" "$out" "no versions found for someone/kizuna: nothing to do"

# without PACKAGE: the repository name, lower-cased, else kizuna
out=$(env -u PACKAGE GITHUB_REPOSITORY=Someone/My-Kizuna OWNER=Someone \
	VERSIONS_JSON='[]' MANIFESTS_JSON='{}' bash "$SCRIPT" 2>&1)
check "package from repository" "$out" "no versions found for someone/my-kizuna: nothing to do"
out=$(env -u PACKAGE -u GITHUB_REPOSITORY OWNER=Someone \
	VERSIONS_JSON='[]' MANIFESTS_JSON='{}' bash "$SCRIPT" 2>&1)
check "package default" "$out" "no versions found for someone/kizuna: nothing to do"

# created_at with fractional seconds is read like a whole second: the
# oldest sha-* build beyond KEEP_COUNT and KEEP_DAYS and the old leftover go,
# the fresh upload stays
out=$(run "$(list "$(
	version 1 sha256:dev 2026-09-01T00:00:00.5Z dev
	version 2 sha256:a 2026-11-01T00:00:00.123456Z sha-a
	version 3 sha256:b 2026-08-01T00:00:00.9Z sha-b
	version 4 sha256:c 2026-07-01T00:00:00.1Z sha-c
	version 5 sha256:fresh 2026-11-08T20:00:00.75Z
	version 6 sha256:old 2026-10-01T00:00:00.000Z
)")" '{}')
check "fractional seconds: exit" "$?" 0
check "fractional seconds: deleted" "$(sed -nE 's/^delete ([0-9]+) .*/\1/p' <<< "$out" | tr '\n' ' ')" "4 6 "

# The real API and registry paths, with curl and gh faked by exported
# functions: curl answers from FAKE_MANIFESTS, needs the token, and fails for
# the token when FAKE_FAIL=token (answers without one when FAKE_FAIL=notoken)
# or for the manifest FAKE_FAIL names, and logs its arguments to CURL_LOG; gh
# lists the versions in two pages, FAKE_PAGE1 and FAKE_PAGE2 (failing after
# the first when FAKE_FAIL=list), and prints and logs every other call to
# GH_LOG (the script sends its output to /dev/null).
GH_LOG=$(mktemp "${TMPDIR:-/tmp}/ghcr-cleanup-test.XXXXXX") || exit 1
CURL_LOG=$(mktemp "${TMPDIR:-/tmp}/ghcr-cleanup-test.XXXXXX") || exit 1
trap 'rm -f -- "$GH_LOG" "$CURL_LOG"' EXIT
curl() {
	local url=${!#}
	echo "$*" >>"$CURL_LOG"
	case $url in
	*/token?*)
		[[ ${FAKE_FAIL:-} == token ]] && return 22
		[[ ${FAKE_FAIL:-} == notoken ]] && echo '{}' && return
		echo '{"token":"T"}'
		;;
	*/manifests/*)
		[[ $* == *"Bearer T"* && ${url##*/} != "${FAKE_FAIL:-}" ]] || return 22
		printf '%s' "$FAKE_MANIFESTS" | "${JQ:-jq}" -c --arg d "${url##*/}" '.[$d] // {}'
		;;
	*) return 22 ;;
	esac
}
gh() {
	if [[ $1 == api && $2 == --paginate ]]; then
		[[ $3 == "/users/Someone/packages/container/kizuna/versions?per_page=100" ]] || return 1
		printf '%s\n' "$FAKE_PAGE1"
		[[ ${FAKE_FAIL:-} == list ]] && return 1
		printf '%s\n' "$FAKE_PAGE2"
		return
	fi
	echo "gh $*"
	echo "gh $*" >>"$GH_LOG"
}
export -f curl gh
export FAKE_MANIFESTS=$manifests GH_LOG CURL_LOG
FAKE_PAGE1=$(printf '%s' "$versions" | "${JQ:-jq}" -c '.[:7]')
FAKE_PAGE2=$(printf '%s' "$versions" | "${JQ:-jq}" -c '.[7:]')
export FAKE_PAGE1 FAKE_PAGE2

out=$(run "$versions" '')
check "registry: deleted" "$(sed -nE 's/^delete ([0-9]+) .*/\1/p' <<< "$out" | tr '\n' ' ')" "6 8 7 12 "
# every registry call has a time limit, to connect and in all
check "registry: curl called" "$(($(wc -l <"$CURL_LOG") > 0))" 1
check "registry: every curl call has time limits" \
	"$(($(grep -cv -- '--connect-timeout [0-9]' "$CURL_LOG") + $(grep -cv -- '--max-time [0-9]' "$CURL_LOG")))" 0

# the versions from the API, two pages joined, plan the same
out=$(run '' '')
check "listing: exit" "$?" 0
check "listing: pages joined" "$(head -n 1 <<< "$out")" "someone/kizuna: 12 versions, keeping 8, deleting 4"
check "listing: deleted" "$(sed -nE 's/^delete ([0-9]+) .*/\1/p' <<< "$out" | tr '\n' ' ')" "6 8 7 12 "

# a listing that fails after its first page stops the run: the versions on
# the missing page can't be followed as kept
out=$(FAKE_FAIL=list DELETE=true run '' '')
check "listing fails: stopped" "$(($? != 0))" 1
check "listing fails: nothing planned or deleted" "$(grep -cE '^(delete|deleted|gh) ' <<< "$out")" 0
check "listing fails: says why" "$(tail -n 1 <<< "$out")" \
	"::error::listing the versions of someone/kizuna failed: stopping (nothing deleted)"
check "listing fails: gh never deletes" "$(cat "$GH_LOG")" ""

# a failed lookup stops the run before anything is planned or deleted
for fail in token notoken sha256:dev sha256:devatt; do
	out=$(FAKE_FAIL=$fail DELETE=true run "$versions" '')
	check "registry fails ($fail): stopped" "$(($? != 0))" 1
	check "registry fails ($fail): nothing deleted" "$(grep -cE '^(delete|deleted|gh) ' <<< "$out")" 0
	case $fail in
	*token) want="::error::no registry token for someone/kizuna: stopping (nothing deleted)" ;;
	*) want="::error::manifest $fail: lookup failed (a kept version the registry can't serve?): stopping (nothing deleted)" ;;
	esac
	check "registry fails ($fail): says why" "$(tail -n 1 <<< "$out")" "$want"
done
check "registry fails: gh never called" "$(cat "$GH_LOG")" ""

# DELETE=true: exactly the planned versions reach gh, tagged first
out=$(DELETE=true run "$versions" '')
check "delete: exit" "$?" 0
check "delete: gh calls" "$(cat "$GH_LOG")" \
	"$(printf 'gh api -X DELETE /users/Someone/packages/container/kizuna/versions/%s\n' 6 8 7 12)"
check "delete: reported" "$(sed -nE 's/^deleted ([0-9]+)$/\1/p' <<< "$out" | tr '\n' ' ')" "6 8 7 12 "

echo "$CHECKS checks, $FAILS failed"
((FAILS == 0))
