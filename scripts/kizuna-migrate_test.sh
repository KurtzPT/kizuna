#!/usr/bin/env bash
# shellcheck disable=SC2154 # the variables checked are set by the sourced script
# Behaviour tests for kizuna-migrate.sh: the file name parsing, reading and
# writing .plexmatch files, HAMA .id files, whole runs without Kizuna, and
# whole runs against a fake Kizuna (anidb, tvdb and auto rules).
#
#   bash scripts/kizuna-migrate_test.sh       (JQ=gojq to use gojq)
#
# Offline: the real Kizuna is never asked (-u points at a closed port, or at a
# fake curl answering from canned JSON). The fake-Kizuna checks need jq (or
# $JQ) and are skipped without it; the symlink checks are skipped where the
# filesystem can't make symlinks. Each run works in a new folder under $TMPDIR
# (or /tmp), deleted at the end when every check passed and kept (its path
# printed) when one failed.

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=$HERE/kizuna-migrate.sh
# shellcheck source=kizuna-migrate.sh
source "$SCRIPT"
set +e # the script sets -e: a failing check must not end the run

NO_KIZUNA=http://127.0.0.1:9 # nothing listens there
WORK=$(mktemp -d "${TMPDIR:-/tmp}/kizuna-migrate-test.XXXXXX") || exit 1
CHECKS=0 FAILS=0

# cleanup runs on any exit: a failed run keeps its folder to look at
cleanup() {
	if ((FAILS == 0)); then
		rm -rf -- "$WORK"
	else
		echo "test folder kept: $WORK"
	fi
}
trap cleanup EXIT

# check NAME GOT WANT
check() {
	CHECKS=$((CHECKS + 1))
	if [[ $2 != "$3" ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL %s\n  got:  %q\n  want: %q\n' "$1" "$2" "$3"
	fi
}

# check_contains NAME TEXT PART
check_contains() {
	CHECKS=$((CHECKS + 1))
	if [[ $2 != *"$3"* ]]; then
		FAILS=$((FAILS + 1))
		printf 'FAIL %s\n  missing: %q\n  in: %s\n' "$1" "$3" "$2"
	fi
}

# bytes prints a file's bytes the way od shows them (\r, \n), spaces dropped:
# command substitution would drop a trailing newline (and on Windows a \r).
bytes() { od -An -c "$1" | tr -d ' \n'; }

# --- name parsing --------------------------------------------------------------

while IFS='|' read -r name want; do
	check "absolute_number \"$name\"" "$(absolute_number "$name")" "$want"
done <<'EOF'
Show - 05|5
[Group] Show - 033v2 [1080p]|33
Show E12|12
Show Episode 7 (BD)|7
Show 12|12
Show - S01E05|0
Show|0
EOF

while IFS='|' read -r name want; do
	check "credit_of \"$name\"" "$(credit_of "$name")" "$want"
done <<'EOF'
Show - NCOP1|op 1 -
Show - OP1b|op 1 b
Show - ED|ed 1 -
Show - Ending 2a|ed 2 a
Show - 05|
Show - S01E01|
EOF

while IFS='|' read -r name want; do
	check "credit_title \"$name\"" "$(credit_title "$name")" "$want"
done <<'EOF'
Opening 1a|1 a
Opening 1 (b)|1 b
Ending|1 -
Ending 2: Song|2 -
Episode 1|
EOF

while IFS='|' read -r name want; do
	check "special_number \"$name\"" "$(special_number "$name")" "$want"
done <<'EOF'
Show - SP03|3
Show - OVA|1
Show - PV|201
Show - Trailer 2|202
Show - 05|0
Show - S01E05|0
EOF

while IFS='|' read -r name want; do
	check "plex_extra \"$name\"" "$(plex_extra "$name")" "$want"
done <<'EOF'
Show - NCOP1.mkv|NCOP1
Show - OP1b.mkv|OP1b
Show - Creditless Ending.mkv|Creditless Ending
Show - 05.mkv|
Show - Operation.mkv|
EOF

while IFS='|' read -r value season want; do
	check "norm_ep $value $season" "$(norm_ep "$value" "$season")" "$want"
done <<'EOF'
SP04|1|0 4
S03E04|1|3 4
E02|5|5 2
02-03|1|1 2
x1|1|
EOF

# --- reading .plexmatch files --------------------------------------------------

show=$WORK/read/Show
mkdir -p "$show/Season 03" "$show/extras"
printf 'title: Other Name\r\nyear: 2009\r\nseason: 2\r\nep: SP04: a.mkv\r\n# ep: 9: commented.mkv\r\nep: 7: Season 03\\c.mkv' >"$show/.plexmatch"
printf 'season: 5\nep: 03: b.mkv\n' >"$show/Season 03/.plexmatch"
printf 'pattern: x\n' >"$show/extras/.plexmatch" # extras folders aren't read
read_plexmatches "$show"
check "title: hint" "$SHOW_TITLE" "Other Name"
check "year: hint" "$SHOW_YEAR" "2009"
check "season: hint in the show folder" "$ROOT_SEASON" "2"
check "ep: hint, CRLF line" "${USER_EP[a.mkv]:-}" "SP04"
check "ep: hint with a backslash, last line without newline" "${USER_EP[Season 03/c.mkv]:-}" "7"
check "a comment is no hint" "${USER_EP[commented.mkv]:-}" ""
check "ep: hint in a subfolder's .plexmatch" "${USER_EP[Season 03/b.mkv]:-}" "03"
check "season: hint in a subfolder" "${DIR_SEASON[Season 03]:-}" "5"
check "extras/ is not read" "$HAS_PATTERN" "0"
check "dir_season of the subfolder" "$(dir_season "Season 03")" "5"

printf 'pattern: {show} - {episode}\n' >"$show/.plexmatch"
read_plexmatches "$show"
check "pattern: hint" "$HAS_PATTERN" "1"

# --- writing .plexmatch files --------------------------------------------------

dir=$WORK/write
mkdir -p "$dir/new" "$dir/lf" "$dir/crlf" "$dir/fail"

add_hints "$dir/new/.plexmatch" $'ep: S00E01: a.mkv\nep: S00E02: b.mkv\n'
check "new file" "$(cat "$dir/new/.plexmatch")" $'ep: S00E01: a.mkv\nep: S00E02: b.mkv'

printf 'title: Show\nep: 1: x.mkv' >"$dir/lf/.plexmatch" # no newline at the end
chmod 600 "$dir/lf/.plexmatch"
mode=$(ls -l "$dir/lf/.plexmatch" | cut -c1-10)
add_hints "$dir/lf/.plexmatch" $'ep: S00E01: a.mkv\n'
check "existing file: lines kept, hint appended" "$(cat "$dir/lf/.plexmatch")" $'title: Show\nep: 1: x.mkv\nep: S00E01: a.mkv'
check "existing file: permissions kept" "$(ls -l "$dir/lf/.plexmatch" | cut -c1-10)" "$mode"
check "no temp file left" "$(ls -A "$dir/lf")" ".plexmatch"

printf 'title: Show\r\n' >"$dir/crlf/.plexmatch"
add_hints "$dir/crlf/.plexmatch" $'ep: S00E01: a.mkv\nep: S00E02: b.mkv\n'
check "Windows file keeps its line endings" "$(bytes "$dir/crlf/.plexmatch")" \
	'title:Show\r\nep:S00E01:a.mkv\r\nep:S00E02:b.mkv\r\n'

# A write that fails leaves the old file whole (here the temp file's name is
# taken by a folder, which no one can write a file over, root included).
printf 'title: Keep Me\nep: 1: x.mkv\n' >"$dir/fail/.plexmatch"
mkdir "$dir/fail/.plexmatch.kizuna-tmp"
err=$( (add_hints "$dir/fail/.plexmatch" $'ep: S00E01: a.mkv\n') 2>&1)
status=$?
check "failed write: exit status" "$status" "1"
check_contains "failed write: says so" "$err" "can't write $dir/fail/.plexmatch"
check "failed write: old file whole" "$(cat "$dir/fail/.plexmatch")" $'title: Keep Me\nep: 1: x.mkv'

for c in "Show [tvdb4-81797]|[tvdb4-81797]" "Show {anidb2-23}|{anidb2-23}" "Show [tvdb3-81797]|" 	"Show [tvdb-81797]|" "Show [anidb-23] [tvdb4-81797]|" "Show (2004)|"; do
	name=${c%%|*} want=${c#*|}
	check "unsupported_order \"$name\"" "$(unsupported_order "$name")" "$want"
done

# --- whole runs (anidb mode, Kizuna not reachable) -------------------------------

lib=$WORK/library
show="$lib/Show [anidb-1]"
mkdir -p "$show" "$lib/Movie" "$lib/.hidden" "$lib/Other [anidb2-5]" "$lib/First [anidb-2]"
: >"$lib/First [anidb-2]/First - S01E05.mkv" # a show's only episode so far, not a movie
for f in "Show - 01.mkv" "Show - 02.mkv" "Show - SP1.mkv" "Show - SP2.mkv" "Show - SP3.mkv" "Show - PV.mkv" \
	"Show - NCOP1.mkv" "notes.txt"; do
	: >"$show/$f"
done
: >"$lib/Movie/Movie (2010).mkv"
: >"$lib/.hidden/Hidden - SP1.mkv" # hidden folders aren't shows
# SP2 has a hint Plex drops, SP3 one that differs from the plan: both are kept
printf 'title: Show\r\nep: SP02: Show - SP2.mkv\r\nep: S00E05: Show - SP3.mkv\r\n' >"$show/.plexmatch"
original='title:Show\r\nep:SP02:Show-SP2.mkv\r\nep:S00E05:Show-SP3.mkv\r\n'

out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$lib" 2>&1)
check "dry run exits 0" "$?" "0"
check_contains "dry run: special" "$out" "+ ep: S00E01: Show - SP1.mkv"
check_contains "dry run: trailer" "$out" "+ ep: S00E201: Show - PV.mkv"
check_contains "dry run: movie" "$out" "+ ep: S01E01: Movie (2010).mkv"
check_contains "dry run: NCOP stays an extra" "$out" "Plex's scanner makes files with \"NCOP1\" in the name"
check_contains "dry run: SPxx hint warned" "$out" "its hint \"ep: SP02\" makes Plex leave the file out"
check_contains "dry run: other hint kept" "$out" "kept its hint \"ep: S00E05\"; Kizuna's numbering is S00E03"
check_contains "dry run: unsupported HAMA order" "$out" "[anidb2-5] is a HAMA numbering order Kizuna doesn't support"
check_contains "dry run: summary" "$out" "2 .plexmatch files to write, 3 hints in all, 2 files already have a hint"
check "dry run writes nothing" "$(bytes "$show/.plexmatch")" "$original"
check "dry run: no new .plexmatch" "$(ls -A "$lib/Movie")" "Movie (2010).mkv"
check "dry run: a lone SxxEyy file gets no hint" "$(grep -c 'First - S01E05' <<<"$out")" "0"

out=$(bash "$SCRIPT" -u "$NO_KIZUNA" -a "$lib" 2>&1)
check "apply exits 0" "$?" "0"
check_contains "apply: says so" "$out" "wrote 2 .plexmatch files"
check "apply: hints appended in the file's line endings" "$(bytes "$show/.plexmatch")" \
	"${original}"'ep:S00E201:Show-PV.mkv\r\nep:S00E01:Show-SP1.mkv\r\n'
check "apply: new .plexmatch for the movie" "$(bytes "$lib/Movie/.plexmatch")" 'ep:S01E01:Movie(2010).mkv\n'
check "apply: hidden folder untouched" "$(ls -A "$lib/.hidden")" "Hidden - SP1.mkv"

before=$(bytes "$show/.plexmatch")$(bytes "$lib/Movie/.plexmatch")
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" -a "$lib" 2>&1)
check_contains "second run: nothing to write" "$out" "0 .plexmatch files to write"
check "second run changes nothing" "$(bytes "$show/.plexmatch")$(bytes "$lib/Movie/.plexmatch")" "$before"

out=$(bash "$SCRIPT" -m nope "$lib" 2>&1)
check "unknown mode exits 1" "$?" "1"
check_contains "unknown mode says so" "$out" "unknown mode nope"

# a relative LIBRARY that starts with "-" is no option to find or basename
mkdir -p "$WORK/dash/-lib/Show [anidb-1]"
: >"$WORK/dash/-lib/Show [anidb-1]/Show - SP1.mkv"
out=$(cd "$WORK/dash" && bash "$SCRIPT" -u "$NO_KIZUNA" -- -lib/ 2>&1)
check "LIBRARY -lib: exit" "$?" "0"
check_contains "LIBRARY -lib: planned" "$out" "+ ep: S00E01: Show - SP1.mkv"

# a show folder named Extras: its .plexmatch is read like any other show's
mkdir -p "$WORK/extras/Extras"
: >"$WORK/extras/Extras/Extras - SP1.mkv"
printf 'ep: S00E09: Extras - SP1.mkv\n' >"$WORK/extras/Extras/.plexmatch"
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$WORK/extras" 2>&1)
check_contains "show folder Extras: its hint kept" "$out" "kept its hint \"ep: S00E09\"; Kizuna's numbering is S00E01"
check_contains "show folder Extras: no new hint" "$out" "0 .plexmatch files to write"

# nothing deeper than MAX_DEPTH below the show folder is looked at
check "depth limit: the MAX_DEPTH the folders below assume" "$MAX_DEPTH" "6"
deep="$WORK/deep/Deep [anidb-1]"
mkdir -p "$deep/1/2/3/4/5/6"
: >"$deep/1/2/3/4/5/Deep - SP1.mkv"    # at MAX_DEPTH (6)
: >"$deep/1/2/3/4/5/6/Deep - SP2.mkv"  # one deeper
printf 'pattern: x\n' >"$deep/1/2/3/4/5/6/.plexmatch"
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$WORK/deep" 2>&1)
check "depth limit: exit" "$?" "0"
check_contains "depth limit: a video at the limit" "$out" "+ ep: S00E01: 1/2/3/4/5/Deep - SP1.mkv"
check "depth limit: a video below it" "$(grep -c 'Deep - SP2' <<<"$out")" "0"
check_contains "depth limit: a .plexmatch below it isn't read" "$out" "1 .plexmatch files to write, 1 hints in all"

# --- HAMA .id files --------------------------------------------------------------

ids=$WORK/ids
mkdir -p "$ids/Bebop" "$ids/Both" "$ids/Empty" "$ids/Tagged [anidb-5]" "$ids/Imdb"
printf '23\r\n' >"$ids/Bebop/anidb.id"
printf '100\n' >"$ids/Both/tvdb.id"
printf '7\n' >"$ids/Both/AniDB.id"
: >"$ids/Empty/tvdb.id"
printf '9\n' >"$ids/Tagged [anidb-5]/tvdb.id"
printf '0133093\n' >"$ids/Imdb/imdb.id"
# id_file DIR: ID_FILE_TAG and the warnings, one per line
id_file() {
	WARNINGS=()
	read_id_file "$1"
	printf '%s\n' "$ID_FILE_TAG" "${WARNINGS[@]}"
}
out=$(id_file "$ids/Bebop")
check ".id: anidb.id with CRLF" "$(head -n 1 <<<"$out")" "[anidb-23]"
check_contains ".id: says to rename" "$out" "Rename the folder to \"Bebop [anidb-23]\""
out=$(id_file "$ids/Both")
check ".id: anidb wins over tvdb, any case" "$(head -n 1 <<<"$out")" "[anidb-7]"
check_contains ".id: names the other kinds" "$out" "$ids/Both: also has tvdb.id; used AniDB.id"
out=$(id_file "$ids/Empty")
check ".id: empty file gives no tag" "$(head -n 1 <<<"$out")" ""
check_contains ".id: empty file warned" "$out" "$ids/Empty/tvdb.id: no id in it; ignored"
check ".id: a tag in the folder name wins" "$(id_file "$ids/Tagged [anidb-5]")" ""
check ".id: imdb.id" "$(head -n 1 <<<"$(id_file "$ids/Imdb")")" "[imdb-tt0133093]"

# --- whole runs (auto mode, Kizuna not reachable) ---------------------------------

lib=$WORK/auto-offline
mkdir -p "$lib/A [anidb-1]" "$lib/T [tvdb-100]" "$lib/Untagged"
: >"$lib/A [anidb-1]/A - SP1.mkv"
: >"$lib/T [tvdb-100]/T - S02E01.mkv"
: >"$lib/Untagged/Untagged - SP1.mkv"
out=$(bash "$SCRIPT" -m auto -u "$NO_KIZUNA" "$lib" 2>&1)
check "auto offline: exit" "$?" "0"
check_contains "auto offline: [anidb-N] planned" "$out" "+ ep: S00E01: A - SP1.mkv"
check_contains "auto offline: [tvdb-N] skipped" "$out" "$lib/T [tvdb-100]: tvdb rules need Kizuna for the seasons; Kizuna not reachable, skipped"
check_contains "auto offline: untagged skipped" "$out" "$lib/Untagged: its first id tag (if any) isn't [anidb-N] or [tvdb-N], so Kizuna's AUTO_DEFAULT decides; Kizuna not reachable, skipped"

# --- symlinks (followed, as Plex does) ---------------------------------------------

# symlink TARGET LINK: a real symlink, or fails (Git Bash on Windows copies
# unless told not to, and makes real ones only with Developer Mode)
symlink() { MSYS=winsymlinks:nativestrict ln -s "$1" "$2" 2>/dev/null && [[ -L $2 ]]; }
links=$WORK/links
mkdir -p "$links/lib/Plain [anidb-2]" "$links/lib/Loop [anidb-3]" "$links/store/Real Show" "$links/pmlib/Pm [anidb-4]"
: >"$links/store/Real Show/Real - SP1.mkv"
: >"$links/store/video.mkv"
: >"$links/lib/Loop [anidb-3]/Loop - SP1.mkv"
: >"$links/pmlib/Pm [anidb-4]/Pm - SP1.mkv"
printf 'title: Pm\n' >"$links/store/pm-plexmatch"
if symlink "$links/store/Real Show" "$links/lib/Linked [anidb-1]" &&
	symlink "$links/store/video.mkv" "$links/lib/Plain [anidb-2]/Plain - SP2.mkv" &&
	symlink . "$links/lib/Loop [anidb-3]/back" &&
	symlink "$links/store/pm-plexmatch" "$links/pmlib/Pm [anidb-4]/.plexmatch"; then
	out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$links/lib" 2>&1)
	check "symlinks: exit, a loop included" "$?" "0"
	check_contains "symlinks: symlinked show folder" "$out" "$links/lib/Linked [anidb-1]/.plexmatch: new file"
	check_contains "symlinks: its video" "$out" "+ ep: S00E01: Real - SP1.mkv"
	check_contains "symlinks: symlinked video" "$out" "+ ep: S00E02: Plain - SP2.mkv"
	check "symlinks: a loop is followed once" "$(grep -c 'ep: S00E01: .*Loop - SP1.mkv' <<<"$out")" "1"

	# a symlinked .plexmatch is read, but never written: its hints are listed
	pm="$links/pmlib/Pm [anidb-4]/.plexmatch"
	warned="WARNING: $pm is a symlink, so the script doesn't write it"
	summary="0 .plexmatch files to write, 0 hints in all, 0 files already have a hint, 1 warnings"
	out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$links/pmlib" 2>&1)
	check "symlinked .plexmatch, dry run: exit" "$?" "0"
	check_contains "symlinked .plexmatch, dry run: hint listed" "$out" "$pm: a symlink, not written: add these hints by hand"$'\n'"  + ep: S00E01: Pm - SP1.mkv"
	check_contains "symlinked .plexmatch, dry run: warned" "$out" "$warned"
	check_contains "symlinked .plexmatch, dry run: summary" "$out" "$summary"
	out=$(bash "$SCRIPT" -u "$NO_KIZUNA" -a "$links/pmlib" 2>&1)
	check "symlinked .plexmatch, apply: exit" "$?" "0"
	check_contains "symlinked .plexmatch, apply: warned" "$out" "$warned"
	check_contains "symlinked .plexmatch, apply: summary" "$out" "$summary"
	check "symlinked .plexmatch, apply: still a symlink" "$([[ -L $pm ]] && echo link)" "link"
	check "symlinked .plexmatch, apply: its target untouched" "$(bytes "$links/store/pm-plexmatch")" 'title:Pm\n'
	check "symlinked .plexmatch, apply: no temp file left" "$([[ -e $pm.kizuna-tmp ]] && echo left)" ""
else
	echo "SKIP symlink checks: this filesystem can't make symlinks here (Git Bash on Windows: needs Developer Mode)"
fi

# --- whole runs against a fake Kizuna ---------------------------------------------

# The script's curl calls answered from canned JSON, the way Kizuna answers
# Plex: a match (POST .../library/metadata/matches, the show picked by the tag
# or title in the request), a show's episodes (.../grandchildren: index is the
# absolute number, parentIndex the season) and AniDB credits (.../anidb-1-s0/children).
FAKE=http://kizuna.test
# episodes SEASON:FIRST-LAST...: grandchildren with absolute numbers
episodes() {
	local s e first last items=""
	for s in "$@"; do
		first=${s#*:} last=${first#*-} first=${first%-*} s=${s%%:*}
		for ((e = first; e <= last; e++)); do items+="{\"type\":\"episode\",\"index\":$e,\"parentIndex\":$s},"; done
	done
	printf '{"MediaContainer":{"Metadata":[%s]}}' "${items%,}"
}
curl() {
	local url=${!#} body="" prev="" a key=""
	for a in "$@"; do
		if [[ $prev == -d ]]; then body=$a; fi
		prev=$a
	done
	body=${body//[[:space:]]/}
	case $url in
	"$FAKE/health") ;;
	"$FAKE"/*/library/metadata/matches)
		case $body in
		*'[tvdb-100]'*) key=tvdb-100 ;;
		*'[tvdb3-200]'*) key=tvdb-200 ;;
		*'"title":"Untagged"'*) key=tvdb-300 ;;
		*'[tmdb-5]'*) key=anidb-7 ;;
		*'[anidb-1]'*) key=anidb-1 ;;
		esac
		if [[ -n $key ]]; then
			printf '{"MediaContainer":{"size":1,"Metadata":[{"type":"show","ratingKey":"%s"}]}}' "$key"
		else
			printf '{"MediaContainer":{"size":0}}'
		fi
		;;
	# season 0 listed too: the script leaves specials out
	"$FAKE"/*/library/metadata/tvdb-[123]00/grandchildren) episodes 0:1-2 2:4-6 1:1-3 ;;
	"$FAKE"/anidb/library/metadata/anidb-1-s0/children)
		printf '{"MediaContainer":{"Metadata":[%s]}}' \
			'{"index":1,"title":"Episode S1"},{"index":101,"title":"Opening 1a"},{"index":102,"title":"Opening 1b"},{"index":103,"title":"Opening 2"},{"index":151,"title":"Ending 1"}'
		;;
	*) return 7 ;; # "couldn't connect"
	esac
}
export -f curl episodes
export FAKE

have_jq=1
if ! command -v jq >/dev/null; then
	if [[ -n ${JQ:-} ]] && command -v "$JQ" >/dev/null; then
		jq() { "$JQ" "$@"; }
		export -f jq
		export JQ
	else
		have_jq=0
	fi
fi

if ((have_jq)); then
	# tvdb mode: plain [tvdb-N] (per-season numbers), [tvdb3-N] (absolute), an
	# .id file, a show Kizuna doesn't match
	lib=$WORK/tvdb
	mkdir -p "$lib/Show [tvdb-100]/Season 02" "$lib/Abs [tvdb3-200]" "$lib/Ided" "$lib/Nomatch [tvdb3-999]"
	for f in "Show - S01E02.mkv" "Show - S02E01.mkv" "Show - S02E05.mkv" "Show - S02E09.mkv" "Show - SP1.mkv" \
		"Season 02/Show - 02.mkv"; do
		: >"$lib/Show [tvdb-100]/$f"
	done
	for f in "Abs - 01.mkv" "Abs - 05.mkv" "Abs - 99.mkv"; do : >"$lib/Abs [tvdb3-200]/$f"; done
	printf '100\n' >"$lib/Ided/tvdb.id"
	: >"$lib/Ided/Ided - S02E01.mkv"
	: >"$lib/Nomatch [tvdb3-999]/Nomatch - 05.mkv"
	out=$(bash "$SCRIPT" -m tvdb -u "$FAKE" "$lib" 2>&1)
	check "tvdb: exit" "$?" "0"
	check_contains "tvdb: S02E01 renumbered to absolute" "$out" "+ ep: S02E04: Show - S02E01.mkv"
	check_contains "tvdb: Season 02/ - 02 renumbered" "$out" "+ ep: S02E05: Season 02/Show - 02.mkv"
	check "tvdb: season 1 and already absolute files left alone" \
		"$(grep -cE '\+ ep: .*(S01E02|S02E05)\.mkv' <<<"$out")" "0"
	check_contains "tvdb: a number the season doesn't have" "$out" \
		"Show - S02E09.mkv: S02E09: TheTVDB's season 2 of tvdb-100 has no episode 9 (and 9 isn't one of its absolute numbers)"
	check_contains "tvdb: special only reported" "$out" "special file - add its TVDB number by hand (ep: S00Exx: Show - SP1.mkv)"
	check_contains "tvdb: absolute season 2 episode" "$out" "+ ep: S02E05: Abs - 05.mkv"
	check "tvdb: absolute season 1 episode left alone" "$(grep -c 'ep: .*Abs - 01.mkv' <<<"$out")" "0"
	check_contains "tvdb: absolute episode Kizuna doesn't have" "$out" "Abs - 99.mkv: absolute episode 99 not in tvdb-200"
	check_contains "tvdb: .id file's tag used for the match" "$out" "+ ep: S02E04: Ided - S02E01.mkv"
	check_contains "tvdb: no match" "$out" "$lib/Nomatch [tvdb3-999]: no match from Kizuna at $FAKE; skipped"
	check_contains "tvdb: summary" "$out" "3 .plexmatch files to write, 4 hints in all"

	out=$(bash "$SCRIPT" -m tvdb -u "$NO_KIZUNA" "$lib" 2>&1)
	check "tvdb, Kizuna unreachable: exit" "$?" "1"
	check_contains "tvdb, Kizuna unreachable: says so" "$out" "error: Kizuna is not ready at $NO_KIZUNA"

	# auto mode: rules per folder, by tag or from Kizuna's /auto match
	lib=$WORK/auto
	mkdir -p "$lib/A [anidb-1]" "$lib/T [tvdb-100]" "$lib/Untagged" "$lib/Other [tmdb-5]" "$lib/Nomatch"
	: >"$lib/A [anidb-1]/A - SP1.mkv"
	: >"$lib/T [tvdb-100]/T - S02E01.mkv"
	: >"$lib/Untagged/Untagged - SP1.mkv"
	: >"$lib/Untagged/Untagged - 05.mkv"
	: >"$lib/Other [tmdb-5]/Other - SP1.mkv"
	: >"$lib/Nomatch/Nomatch - 01.mkv"
	out=$(bash "$SCRIPT" -m auto -u "$FAKE" "$lib" 2>&1)
	check "auto: exit" "$?" "0"
	check_contains "auto: [anidb-N] gets anidb rules" "$out" "+ ep: S00E01: A - SP1.mkv"
	check_contains "auto: [tvdb-N] gets tvdb rules" "$out" "+ ep: S02E04: T - S02E01.mkv"
	check_contains "auto: untagged, Kizuna says tvdb: special reported" "$out" "special file - add its TVDB number by hand (ep: S00Exx: Untagged - SP1.mkv)"
	check_contains "auto: untagged, Kizuna says tvdb: season" "$out" "+ ep: S02E05: Untagged - 05.mkv"
	check_contains "auto: other tag, Kizuna says anidb" "$out" "+ ep: S00E01: Other - SP1.mkv"
	check_contains "auto: no match" "$out" "$lib/Nomatch: no match from Kizuna at $FAKE/auto; skipped"

	# anidb mode with Kizuna: openings and endings get AniDB's own numbers
	# (lowercase names, which Plex's scanner doesn't make extras)
	lib=$WORK/credits
	mkdir -p "$lib/Cred [anidb-1]"
	for f in op1b op2 ed1 op1 op3 OP1a; do : >"$lib/Cred [anidb-1]/Cred - $f.mkv"; done
	out=$(bash "$SCRIPT" -u "$FAKE" "$lib" 2>&1)
	check "credits: exit" "$?" "0"
	check_contains "credits: variant" "$out" "+ ep: S00E102: Cred - op1b.mkv"
	check_contains "credits: shifted by a variant" "$out" "+ ep: S00E103: Cred - op2.mkv"
	check_contains "credits: ending" "$out" "+ ep: S00E151: Cred - ed1.mkv"
	check_contains "credits: no letter when AniDB has variants" "$out" "+ ep: S00E101: Cred - op1.mkv"
	check_contains "credits: ... warned" "$out" "AniDB has variants of this one; used 1a (S00E101)"
	check_contains "credits: one AniDB doesn't list" "$out" "Cred - op3.mkv: OP3: not among this show's AniDB openings/endings; no hint"
	check_contains "credits: an extra gets the name to rename it to" "$out" "To get it as Kizuna's S00E101, rename it, e.g. to \"Cred - S00E101.mkv\""
else
	echo "SKIP fake-Kizuna checks (tvdb, auto and anidb with Kizuna): jq not found (install jq, or set JQ=gojq)"
fi

echo "$CHECKS checks, $FAILS failed"
((FAILS == 0))
