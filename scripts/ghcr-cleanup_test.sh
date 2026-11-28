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

# run_defaults VERSIONS: run with the script's own KEEP_DAYS (7) and KEEP_COUNT (0)
run_defaults() {
	env -u KEEP_COUNT -u KEEP_DAYS OWNER=Someone PACKAGE=kizuna MIN_AGE_HOURS=24 \
		NOW=1794182400 VERSIONS_JSON="$1" MANIFESTS_JSON='{}' bash "$SCRIPT" 2>&1
}

# the defaults: a sha-* build older than a week goes unless a release or dev
# tag uses it; a release, however old, stays, and so does a build from the
# last week
out=$(run_defaults "$(list "$(
	version 1 sha256:rel 2026-01-01T00:00:00Z 1.0.0 latest sha-r
	version 2 sha256:dev 2026-10-01T00:00:00Z dev sha-d
	version 3 sha256:week 2026-11-02T01:00:00Z sha-w
	version 4 sha256:eight 2026-11-01T00:00:00Z sha-e
)")")
check "defaults: exit" "$?" 0
check "defaults: deleted" "$(sed -nE 's/^delete ([0-9]+) .*/\1/p' <<< "$out" | tr '\n' ' ')" "4 "
check "defaults: kept tags" "$(sed -n 's/^kept tags: //p' <<< "$out")" "1.0.0 dev latest sha-d sha-r sha-w"

# no minimum count: the newest sha-* build goes too once it is over a week old
out=$(run_defaults "$(list "$(
	version 1 sha256:rel 2026-01-01T00:00:00Z 1.0.0 latest sha-r
	version 2 sha256:dev 2026-10-01T00:00:00Z dev sha-d
	version 4 sha256:eight 2026-11-01T00:00:00Z sha-e
)")")
check "defaults: newest old build" "$(sed -nE 's/^delete ([0-9]+) .*/\1/p' <<< "$out" | tr '\n' ' ')" "4 "

# The real API and registry paths, with curl and gh faked by exported
# functions: curl answers from FAKE_MANIFESTS, needs the token, and fails for
# the token when FAKE_FAIL=token (answers without one when FAKE_FAIL=notoken,
# fails a second token fetch when FAKE_FAIL=refresh, or always answers 401 when
# FAKE_FAIL=401), expires T1 at FAKE_EXPIRE_AT (requiring T2 from then on), or
# fails for the manifest FAKE_FAIL names (a 500) or FAKE_404 lists (a 404, as
# the status probe says), and logs its arguments to CURL_LOG; gh
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
		local n
		n=$(grep -c '/token?' "$CURL_LOG")
		[[ ${FAKE_FAIL:-} == token ]] && return 22
		[[ ${FAKE_FAIL:-} == notoken ]] && echo '{}' && return
		[[ ${FAKE_FAIL:-} == refresh && $n -gt 1 ]] && return 22
		if [[ -n ${FAKE_EXPIRE_AT:-} ]]; then
			printf '{"token":"T%s"}\n' "$n"
		else
			echo '{"token":"T"}'
		fi
		;;
	*/manifests/*)
		local d=${url##*/} code=200
		[[ $d == "${FAKE_FAIL:-}" ]] && code=500
		[[ " ${FAKE_404:-} " == *" $d "* ]] && code=404
		if [[ -n ${FAKE_EXPIRE_AT:-} ]]; then
			if grep -qF "/manifests/$FAKE_EXPIRE_AT" "$CURL_LOG"; then
				[[ $* == *"Bearer T2"* ]] || code=401
			else
				[[ $* == *"Bearer T1"* ]] || code=401
			fi
		else
			[[ $* == *"Bearer T"* ]] || code=401
		fi
		[[ ${FAKE_FAIL:-} == 401 && $d == "${FAKE_EXPIRE_AT:-}" ]] && code=401
		# the status probe (-w '%{http_code}', no -f) prints the status
		if [[ $* == *"%{http_code}"* ]]; then
			echo "$code"
			return
		fi
		[[ $code == 200 ]] || return 22
		# a string in FAKE_MANIFESTS is a raw body (not JSON, say)
		printf '%s' "$FAKE_MANIFESTS" |
			"${JQ:-jq}" -r --arg d "$d" '.[$d] // {} | if type == "string" then . else tojson end'
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
# a manifest read retries twice (the token call and the status probe don't)
check "registry: manifest reads retry" \
	"$(grep -F '/manifests/' "$CURL_LOG" | grep -Fv '%{http_code}' | grep -cv -- '--retry 2')" 0

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
	sha256:dev) want="::error::manifest sha256:dev (version 1): lookup failed (a kept version the registry can't serve?): stopping (nothing deleted)" ;;
	sha256:devatt) want="::error::manifest sha256:devatt (version 3): lookup failed (a kept version the registry can't serve?): stopping (nothing deleted)" ;;
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

# Attestations stored the OCI referrers way: untagged, tied to their image
# only by the subject in their manifest. Shaped like what GHCR holds: k is an
# index (dev) with an image and a BuildKit attestation, ktag the index tagged
# sha256-k that lists the signed attestation ksig (whose subject is k too).
# kref is a signed attestation with no tag; kref2 attests kref (a chain);
# krefidx is an untagged index that attests k and lists a part of its own.
# d is the build beyond KEEP_COUNT and KEEP_DAYS, with the same around it;
# lost attests a digest the package doesn't list.
ref_versions=$(list "$(
	version 1 sha256:k 2026-09-01T00:00:00Z dev sha-k
	version 2 sha256:kimg 2026-09-01T00:00:00Z
	version 3 sha256:katt 2026-09-01T00:00:00Z
	version 4 sha256:ktag 2026-09-01T00:00:00Z sha256-k
	version 5 sha256:ksig 2026-09-01T00:00:00Z
	version 6 sha256:kref2 2026-09-02T00:00:00Z
	version 7 sha256:kref 2026-09-01T00:00:00Z
	version 8 sha256:krefidx 2026-09-01T00:00:00Z
	version 9 sha256:krefpart 2026-09-01T00:00:00Z
	version 10 sha256:x 2026-09-05T00:00:00Z sha-x
	version 11 sha256:y 2026-09-06T00:00:00Z sha-y
	version 12 sha256:d 2026-08-01T00:00:00Z sha-d
	version 13 sha256:dimg 2026-08-01T00:00:00Z
	version 14 sha256:dtag 2026-08-01T00:00:00Z sha256-d
	version 15 sha256:dsig 2026-08-01T00:00:00Z
	version 16 sha256:dref 2026-08-01T00:00:00Z
	version 17 sha256:lost 2026-08-01T00:00:00Z
	version 18 sha256:dref2 2026-08-02T00:00:00Z
)")
ref_manifests='{
  "sha256:k": {"mediaType": "application/vnd.oci.image.index.v1+json",
    "manifests": [{"digest": "sha256:kimg"}, {"digest": "sha256:katt"}]},
  "sha256:katt": {"artifactType": "application/vnd.docker.attestation.manifest.v1+json",
    "subject": {"digest": "sha256:kimg"}},
  "sha256:ktag": {"manifests": [{"digest": "sha256:ksig",
    "artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json"}]},
  "sha256:ksig": {"artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json",
    "subject": {"digest": "sha256:k"}},
  "sha256:kref": {"artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json",
    "subject": {"digest": "sha256:k"}},
  "sha256:kref2": {"subject": {"digest": "sha256:kref"}},
  "sha256:krefidx": {"manifests": [{"digest": "sha256:krefpart"}], "subject": {"digest": "sha256:k"}},
  "sha256:d": {"manifests": [{"digest": "sha256:dimg"}]},
  "sha256:dtag": {"manifests": [{"digest": "sha256:dsig"}]},
  "sha256:dsig": {"subject": {"digest": "sha256:d"}},
  "sha256:dref": {"subject": {"digest": "sha256:d"}},
  "sha256:lost": {"subject": {"digest": "sha256:gone"}},
  "sha256:dref2": {"subject": {"digest": "sha256:dref"}}
}'
# kept: what k needs, kref, kref2, krefidx and krefpart (by subject), x, y
deleted_of() { sed -nE 's/^delete ([0-9]+) .*/\1/p' <<< "$1" | tr '\n' ' '; }
out=$(run "$ref_versions" "$ref_manifests")
check "subject: exit" "$?" 0
check "subject: summary" "$(head -n 1 <<< "$out")" "someone/kizuna: 18 versions, keeping 11, deleting 7"
check "subject: deleted" "$(deleted_of "$out")" "12 14 13 15 16 17 18 "
for id in 6 7 8 9; do
	check "subject: version $id kept" "$(grep -c "^delete $id " <<< "$out")" 0
done
# read once each: the 7 versions the tags lead to (k's tree, x, y), then the
# 9 untagged ones left, for their subject; d and dtag never
check "subject: manifests read" "$(sed -n 's/^manifests read: //p' <<< "$out")" 16

# the same through the registry: no manifest fetched twice
: >"$CURL_LOG"
: >"$GH_LOG"
out=$(FAKE_MANIFESTS=$ref_manifests DELETE=true run "$ref_versions" '')
check "subject, registry: exit" "$?" 0
check "subject, registry: gh calls" "$(cat "$GH_LOG")" \
	"$(printf 'gh api -X DELETE /users/Someone/packages/container/kizuna/versions/%s\n' 12 14 13 15 16 17 18)"
check "subject, registry: manifest calls" "$(grep -c '/manifests/' "$CURL_LOG")" 16
check "subject, registry: each manifest once" "$(sed -nE 's|.*/manifests/||p' "$CURL_LOG" | sort | uniq -d)" ""

# an untagged version that can't be read for its subject stops the run: it
# may be the attestation of a kept image
: >"$GH_LOG"
out=$(FAKE_MANIFESTS=$ref_manifests FAKE_FAIL=sha256:kref DELETE=true run "$ref_versions" '')
check "subject unreadable: stopped" "$(($? != 0))" 1
check "subject unreadable: nothing deleted" "$(grep -cE '^(delete|deleted|gh) ' <<< "$out")" 0
check "subject unreadable: says why" "$(tail -n 1 <<< "$out")" \
	"::error::manifest sha256:kref (version 7): lookup failed (an untagged version, read for its subject): stopping (nothing deleted)"
check "subject unreadable: gh never called" "$(cat "$GH_LOG")" ""

# an untagged version younger than MIN_AGE_HOURS stays without being read
out=$(run "$(list "$(
	version 1 sha256:a 2026-11-01T00:00:00Z sha-a
	version 2 sha256:fresh 2026-11-08T20:00:00Z
)")" '{"sha256:a": {}}')
check "fresh untagged: kept" "$(grep -c '^delete ' <<< "$out")" 0
check "fresh untagged: not read" "$(sed -n 's/^manifests read: //p' <<< "$out")" 1

# Read failures around the subject pass, through the registry. r is kept by
# its subject (a, :dev) and lists two parts nothing read before the repeat
# loop: rpart is tagged (sha256-zz, no root), ryoung is under a day old. gone
# is a leftover the registry has no manifest for (404 when FAKE_404 says so),
# other a plain leftover.
loop_versions=$(list "$(
	version 1 sha256:a 2026-09-01T00:00:00Z dev
	version 2 sha256:r 2026-09-01T00:00:00Z
	version 3 sha256:rpart 2026-09-01T00:00:00Z sha256-zz
	version 4 sha256:ryoung 2026-11-08T20:00:00Z
	version 5 sha256:gone 2026-09-01T00:00:00Z
	version 6 sha256:other 2026-09-01T00:00:00Z
)")
loop_manifests='{
  "sha256:a": {},
  "sha256:r": {"subject": {"digest": "sha256:a"},
    "manifests": [{"digest": "sha256:rpart"}, {"digest": "sha256:ryoung"}]},
  "sha256:rpart": {},
  "sha256:ryoung": {},
  "sha256:other": {}
}'
loop_run() { FAKE_MANIFESTS=${LOOP_MANIFESTS:-$loop_manifests} DELETE=true run "$loop_versions" ''; }
gone_warning="::warning::manifest sha256:gone (version 5): the registry has none (404): no subject to keep it by, so it is deleted like any untagged leftover"

# a 404 in the subject pass: that leftover goes, with a warning naming it,
# and the parts of the version kept by subject are read and kept
: >"$CURL_LOG"
: >"$GH_LOG"
out=$(FAKE_404=sha256:gone loop_run)
check "404 leftover: exit" "$?" 0
check "404 leftover: warned" "$(grep -cxF "$gone_warning" <<< "$out")" 1
check "404 leftover: deleted" "$(cat "$GH_LOG")" \
	"$(printf 'gh api -X DELETE /users/Someone/packages/container/kizuna/versions/%s\n' 5 6)"
check "repeat loop: the tagged and the young part read" \
	"$(grep -cE '/manifests/sha256:(rpart|ryoung)$' "$CURL_LOG")" 2
check "repeat loop: manifests read" "$(sed -n 's/^manifests read: //p' <<< "$out")" 5

# any other failure in the subject pass stops the run, naming the version
for fail in "FAKE_FAIL=sha256:gone" "LOOP_MANIFESTS={\"sha256:gone\":\"<html>\"}"; do
	: >"$GH_LOG"
	out=$(export "${fail?}" && loop_run)
	check "subject pass fails ($fail): stopped" "$(($? != 0))" 1
	check "subject pass fails ($fail): nothing deleted" "$(grep -cE '^(delete|deleted|gh) ' <<< "$out")$(cat "$GH_LOG")" 0
	case $fail in
	FAKE_FAIL=*) want="::error::manifest sha256:gone (version 5): lookup failed (an untagged version, read for its subject): stopping (nothing deleted)" ;;
	*) want="::error::manifest sha256:gone (version 5): the registry's answer is not a manifest (an untagged version, read for its subject): stopping (nothing deleted)" ;;
	esac
	check "subject pass fails ($fail): says why" "$(tail -n 1 <<< "$out")" "$want"
done

# a failure while following a version kept by subject (a 404 included)
# stops the run: that part is needed
for fail in "FAKE_FAIL=sha256:rpart" "FAKE_404=sha256:rpart"; do
	: >"$GH_LOG"
	out=$(export "${fail?}" && loop_run)
	check "repeat loop fails ($fail): stopped" "$(($? != 0))" 1
	check "repeat loop fails ($fail): nothing deleted" "$(grep -cE '^(delete|deleted|gh) ' <<< "$out")$(cat "$GH_LOG")" 0
	check "repeat loop fails ($fail): says why" "$(tail -n 1 <<< "$out")" \
		"::error::manifest sha256:rpart (version 3): lookup failed (a kept version the registry can't serve?): stopping (nothing deleted)"
done

# Token expiry mid-run: a 401 refreshes the token once and retries, and
# subsequent lookups keep using the new token
: >"$CURL_LOG"
: >"$GH_LOG"
out=$(FAKE_EXPIRE_AT=sha256:devatt DELETE=true run "$versions" '')
check "token refresh: exit" "$?" 0
check "token refresh: token fetched twice" "$(grep -c '/token?' "$CURL_LOG")" 2
check "token refresh: deleted" "$(cat "$GH_LOG")" \
	"$(printf 'gh api -X DELETE /users/Someone/packages/container/kizuna/versions/%s\n' 6 8 7 12)"
check "token refresh: later lookup uses new token" \
	"$(grep -c 'Bearer T2.*/manifests/sha256:devattimg$' "$CURL_LOG")" 1

# A 401 on a 404 untagged leftover refreshes first, then sees the real 404
: >"$CURL_LOG"
: >"$GH_LOG"
out=$(FAKE_EXPIRE_AT=sha256:gone FAKE_404=sha256:gone loop_run)
check "token refresh on 404: exit" "$?" 0
check "token refresh on 404: token fetched twice" "$(grep -c '/token?' "$CURL_LOG")" 2
check "token refresh on 404: warned" "$(grep -cxF "$gone_warning" <<< "$out")" 1
check "token refresh on 404: deleted" "$(cat "$GH_LOG")" \
	"$(printf 'gh api -X DELETE /users/Someone/packages/container/kizuna/versions/%s\n' 5 6)"

# If refreshing the token fails, stop with nothing deleted
: >"$CURL_LOG"
: >"$GH_LOG"
out=$(FAKE_EXPIRE_AT=sha256:devatt FAKE_FAIL=refresh DELETE=true run "$versions" '')
check "token refresh fails: stopped" "$(($? != 0))" 1
check "token refresh fails: nothing deleted" "$(grep -cE '^(delete|deleted|gh) ' <<< "$out")$(cat "$GH_LOG")" 0
check "token refresh fails: says why" "$(tail -n 1 <<< "$out")" \
	"::error::no registry token for someone/kizuna: stopping (nothing deleted)"

# If the fresh token still gets 401, stop after one refresh with nothing deleted
: >"$CURL_LOG"
: >"$GH_LOG"
out=$(FAKE_EXPIRE_AT=sha256:devatt FAKE_FAIL=401 DELETE=true run "$versions" '')
check "token still 401: stopped" "$(($? != 0))" 1
check "token still 401: only one refresh" "$(grep -c '/token?' "$CURL_LOG")" 2
check "token still 401: nothing deleted" "$(grep -cE '^(delete|deleted|gh) ' <<< "$out")$(cat "$GH_LOG")" 0
check "token still 401: says why" "$(tail -n 1 <<< "$out")" \
	"::error::manifest sha256:devatt (version 3): lookup failed (a kept version the registry can't serve?): stopping (nothing deleted)"

# Proactive refresh: when the token reaches TOKEN_MAX_AGE, a new token is
# fetched before the next manifest lookup without waiting for a 401
: >"$CURL_LOG"
: >"$GH_LOG"
out=$(TOKEN_MAX_AGE=0 DELETE=true run "$versions" '')
check "proactive token refresh: exit" "$?" 0
check "proactive token refresh: refreshed before lookups" "$(($(grep -c '/token?' "$CURL_LOG") > 1))" 1

echo "$CHECKS checks, $FAILS failed"
((FAILS == 0))
