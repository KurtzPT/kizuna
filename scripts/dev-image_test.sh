#!/usr/bin/env bash
# Behaviour tests for dev-image.sh: what a ci run on dev decides about :dev,
# on a made-up history and registry (git, curl and sleep are faked: no
# network, no repository written). Also checks that the workflows' path
# filters agree with .dockerignore's allowlist.
#
#   bash scripts/dev-image_test.sh       (JQ=gojq to use gojq)

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
SCRIPT=$HERE/dev-image.sh
CHECKS=0 FAILS=0

# check NAME GOT WANT
check() {
	CHECKS=$((CHECKS + 1))
	if [[ $2 != "$3" ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL %s\n  got:  %q\n  want: %q\n' "$1" "$2" "$3"
	fi
}

FAKE_DIR=$(mktemp -d "${TMPDIR:-/tmp}/dev-image-test.XXXXXX") || exit 1
trap 'rm -rf -- "$FAKE_DIR"' EXIT
repo=$FAKE_DIR/repo
mkdir "$repo" || exit 1
cp "$ROOT/.dockerignore" "$repo/"

# The history, one line per commit, oldest first: "<sha> <file>...", the
# files it changes (a move lists both names, as git diff --no-renames does).
: >"$FAKE_DIR/history"

# commit FILE...: add a commit that changes them, print its sha
commit() {
	local sha
	sha=$(printf '%040d' $(($(wc -l <"$FAKE_DIR/history") + 1)))
	echo "$sha $*" >>"$FAKE_DIR/history"
	echo "$sha"
}

# git, faked for the two questions the script asks: is A an ancestor of B
# (no later in the history), and which files changed from A to B (only with
# the exact flags the script must use)
git() {
	local h=$FAKE_DIR/history a b
	case $1 in
	merge-base)
		[ "$2" = --is-ancestor ] || return 129
		a=$(awk -v s="$3" '$1 == s { print NR }' "$h")
		b=$(awk -v s="$4" '$1 == s { print NR }' "$h")
		[ -n "$a" ] && [ -n "$b" ] || return 128
		[ "$a" -le "$b" ]
		;;
	diff)
		[ "$*" = "diff --name-only --no-renames -z $5 $6" ] || return 129
		awk -v a="$5" -v b="$6" '$1 == a { on = 1; next }
			on { for (i = 2; i <= NF; i++) print $i }
			$1 == b { exit }' "$h" | sort -u | tr '\n' '\0'
		;;
	*) return 129 ;;
	esac
}

# run CMD SHA: dev-image.sh CMD in the scratch folder, for commit SHA. Its
# output goes to $FAKE_DIR/log, what it writes to GITHUB_OUTPUT to
# $FAKE_DIR/output; returns its exit status
run() {
	: >"$FAKE_DIR/output"
	: >"$FAKE_DIR/sleeps"
	(cd "$repo" && GITHUB_REPOSITORY=Someone/Kizuna GITHUB_SHA=$2 \
		GITHUB_OUTPUT=$FAKE_DIR/output bash "$SCRIPT" "$1") >"$FAKE_DIR/log" 2>&1
}
output() { cat "$FAKE_DIR/output"; }
logged() { grep -cF -- "$1" "$FAKE_DIR/log"; }

# The registry, faked: the token needs the lower-cased image name, the
# manifests and blob need the token. FAKE_GHCR: unset (:dev built from
# FAKE_REV), none (404 for :dev), error (500 for :dev), down (nothing
# answers), flaky (the first request fails, then as unset), nolabel.
curl() {
	local url=${!#} index
	index='{"manifests":[{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},{"digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}}]}'
	case ${FAKE_GHCR:-} in
	down) return 7 ;;
	flaky)
		if [ ! -e "$FAKE_DIR/flaked" ]; then
			: >"$FAKE_DIR/flaked"
			return 7
		fi
		;;
	esac
	case $url in
	https://ghcr.io/token\?scope=repository:someone/kizuna:pull) echo '{"token":"T"}' ;;
	https://ghcr.io/v2/someone/kizuna/manifests/dev)
		[[ $* == *"Bearer T"* ]] || return 22
		case ${FAKE_GHCR:-} in
		none) printf '%s\n404' '{"errors":[{"code":"MANIFEST_UNKNOWN"}]}' ;;
		error) printf '%s\n500' 'oops' ;;
		*) printf '%s\n200' "$index" ;;
		esac
		;;
	https://ghcr.io/v2/someone/kizuna/manifests/sha256:amd)
		[[ $* == *"Bearer T"* ]] || return 22
		echo '{"config":{"digest":"sha256:cfg"}}'
		;;
	https://ghcr.io/v2/someone/kizuna/blobs/sha256:cfg)
		[[ $* == *"Bearer T"* ]] || return 22
		if [ "${FAKE_GHCR:-}" = nolabel ]; then
			echo '{"config":{"Labels":{}}}'
		else
			printf '{"config":{"Labels":{"org.opencontainers.image.revision":"%s"}}}\n' "$FAKE_REV"
		fi
		;;
	*) return 22 ;;
	esac
}
sleep() { echo "sleep $*" >>"$FAKE_DIR/sleeps"; }
export -f curl sleep git
export FAKE_DIR

base=$(commit internal/a.go cmd/kizuna/main.go README.md)

# --- revision ---------------------------------------------------------------
FAKE_REV=$base run revision x
check "revision: exit" "$?" 0
check "revision: the label" "$(cat "$FAKE_DIR/log")" "$base"
FAKE_GHCR=none run revision x
check "revision, no :dev: exit" "$?" 0
check "revision, no :dev: nothing" "$(cat "$FAKE_DIR/log")" ""
FAKE_GHCR=nolabel run revision x
check "revision, no label: nothing" "$(cat "$FAKE_DIR/log")" ""
FAKE_REV=not-a-commit run revision x
check "revision, not a commit: nothing" "$(cat "$FAKE_DIR/log")" ""
FAKE_GHCR=error run revision x
check "revision, 500: fails" "$?" 1
check "revision, 500: says so" "$(cat "$FAKE_DIR/log")" "ghcr.io answered 500 for :dev"
FAKE_GHCR=down run revision x
check "revision, unreachable: fails" "$?" 1

# --- publish (the gate) -----------------------------------------------------
# first publish: no :dev yet
FAKE_GHCR=none run publish "$base"
check "first publish: exit" "$?" 0
check "first publish" "$(output)" publish=true
check "first publish: says why" "$(logged 'there is no :dev image')" 1
FAKE_GHCR=nolabel run publish "$base"
check "no label: publish" "$(output)" publish=true

# a failed read publishes: missing a change is worse than one more build
FAKE_GHCR=down run publish "$base"
check "unreadable: exit" "$?" 0
check "unreadable: publish" "$(output)" publish=true
check "unreadable: says why" "$(logged "couldn't read which commit")" 1

FAKE_REV=$base run publish "$base"
check "same commit: not published" "$(output)" publish=false
check "same commit: says why" "$(logged 'already built from this commit')" 1

# image unchanged: only files the image doesn't contain
docs=$(commit README.md docs/notes.txt LICENSE scripts/x.sh assets/logo.png unraid/kizuna.xml \
	compose.yml .gitignore .gitattributes .github/workflows/ci.yml .github/labeler.yml \
	cmdline.txt internal.md go.mod.orig)
FAKE_REV=$base run publish "$docs"
check "image unchanged: exit" "$?" 0
check "image unchanged: not published" "$(output)" publish=false
check "image unchanged: says why" "$(logged 'nothing that goes into the image changed')" 1
check "image unchanged: lists the files" "$(logged '  docs/notes.txt')" 1

# image changed: each file or folder in the allowlist, and what defines the build
prev=$docs
for f in internal/a.go internal/x/README.md cmd/kizuna/main.go go.mod .env.sample Dockerfile \
	.dockerignore .github/workflows/docker.yml .github/actions/build-image/action.yml; do
	c=$(commit "$f")
	FAKE_REV=$prev run publish "$c"
	check "image changed ($f): published" "$(output)" publish=true
	prev=$c
done
# several pushes since :dev, one of them to the image
c=$(commit README.md)
FAKE_REV=$docs run publish "$c"
check "image changed further back: published" "$(output)" publish=true
latest=$c

# a file moved out of the image changes it (the fake git insists on
# --no-renames, which lists the old name too)
moved=$(commit internal/a.go docs/a.go)
FAKE_REV=$latest run publish "$moved"
check "moved out of the image: published" "$(output)" publish=true

# only empty commits since :dev: publish rather than guess
empty=$(commit)
FAKE_REV=$moved run publish "$empty"
check "empty diff: published" "$(output)" publish=true

# a re-run of an older run: :dev isn't in its history. The gate publishes,
# and move keeps :dev where it is (below)
FAKE_REV=$empty run publish "$docs"
check "older run: published" "$(output)" publish=true
check "older run: says why" "$(logged "isn't in this commit's history")" 1

# a force push: :dev's commit is gone
gone=$(printf 'a%.0s' {1..40})
FAKE_REV=$gone run publish "$empty"
check "force push: published" "$(output)" publish=true

# --- move (the publish job) -------------------------------------------------
FAKE_GHCR=none run move "$base"
check "move, first publish: exit" "$?" 0
check "move, first publish" "$(output)" dev=true
FAKE_REV=$docs run move "$empty"
check "move, newer commit" "$(output)" dev=true
FAKE_REV=$empty run move "$empty"
check "move, same commit" "$(output)" dev=true
FAKE_REV=$gone run move "$empty"
check "move, force push" "$(output)" dev=true

# never back: :dev is built from a newer commit than this run's
FAKE_REV=$empty run move "$docs"
check "move, older commit: exit" "$?" 0
check "move, older commit: :dev stays" "$(output)" dev=false
check "move, older commit: says why" "$(logged 'a newer commit than this run')" 1

# one failed read is tried again after a pause
FAKE_GHCR=flaky FAKE_REV=$empty run move "$docs"
check "move, flaky: exit" "$?" 0
check "move, flaky: read again" "$(output)" dev=false
check "move, flaky: one pause" "$(cat "$FAKE_DIR/sleeps")" "sleep 10"

# two failed reads: stop, nothing pushed
FAKE_GHCR=down run move "$docs"
check "move, unreadable: fails" "$?" 1
check "move, unreadable: no output" "$(output)" ""
check "move, unreadable: says why" "$(logged '::error::couldn'"'"'t read which commit :dev was built from')" 1
check "move, unreadable: one pause" "$(cat "$FAKE_DIR/sleeps")" "sleep 10"

# --- .dockerignore must stay an allowlist the script can read ---------------
for bad in 'docs' $'*\n!cmd\n**/*.md' $'*\n!internal/**' ''; do
	printf '%s\n' "$bad" >"$repo/.dockerignore"
	FAKE_REV=$base run publish "$base"
	check "unreadable .dockerignore ($bad): fails" "$(($? != 0))" 1
	check "unreadable .dockerignore ($bad): no output" "$(output)" ""
	check "unreadable .dockerignore ($bad): says why" "$(logged '::error file=.dockerignore::')" 1
done
cp "$ROOT/.dockerignore" "$repo/"

run nonsense x
check "usage" "$?" 2

# --- the real repository: one list ------------------------------------------
want=$(cd "$ROOT" && bash "$SCRIPT" paths) || want=
check "paths: read" "$(($(wc -l <<<"$want") > 4))" 1

# every entry is a file or folder that exists: a typo would match nothing
while IFS= read -r p; do
	check "paths: $p exists" "$(test -e "$ROOT/${p%/\*\*}" && echo yes)" yes
done <<<"$want"

# docker.yml's push filter (GitHub reads no other file) is the same list
got=$(awk '/^    paths:$/ { on = 1; next }
	on && /^      - / { sub(/^      - /, ""); gsub(/"/, ""); print; next }
	on { exit }' "$ROOT/.github/workflows/docker.yml")
check "docker.yml push paths = .dockerignore + build files" "$(sort <<<"$got")" "$(sort <<<"$want")"

# ci's and codeql's docs-only filters skip whole pushes: none may cover a file
# that goes into the image, or a change to it would never publish :dev
mapfile -t entries <<<"${want//\/\*\*/}"
mapfile -t files < <(command git -C "$ROOT" ls-files --cached --others --exclude-standard -- "${entries[@]}")
check "image files listed" "$((${#files[@]} > 10))" 1
for wf in ci codeql; do
	ignore=$(sed -n 's/^    paths-ignore: \[\(.*\)\].*/\1/p' "$ROOT/.github/workflows/$wf.yml")
	check "$wf.yml: paths-ignore read" "$([ -n "$ignore" ] && echo yes)" yes
	IFS=, read -ra pats <<<"$ignore"
	hit=
	for pat in "${pats[@]}"; do
		pat=${pat//\"/}
		pat=${pat// /}
		# ** and * both match across folders here: stricter than GitHub
		for f in "${files[@]}"; do
			# shellcheck disable=SC2053 # $pat is a pattern
			[[ $f == $pat ]] && hit+="$f ($pat) "
		done
	done
	check "$wf.yml: paths-ignore skips no image file" "$hit" ""
done

echo "$CHECKS checks, $FAILS failed"
((FAILS == 0))
