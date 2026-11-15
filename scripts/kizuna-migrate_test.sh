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

# trim, strip_tags and file_stem set a variable, read here as the script reads it
while IFS='|' read -r text want; do
	trim "$text"
	check "trim \"$text\"" "$TRIM" "$want"
done <<'EOF'
  Show - 05  |Show - 05
Show|Show
   |
|
EOF
trim $'\t Show\r\n'
check "trim: tabs and line ends" "$TRIM" "Show"

while IFS='|' read -r name want; do
	strip_tags "$name"
	check "strip_tags \"$name\"" "$STRIP_TAGS" "$want"
done <<'EOF'
[Grp] Show - 05 [1080p]|Show - 05
[Grp]_Show_-_SP1_[1080p]|Show - SP1
[Grp] Show_-_SP1 [1080p]|Show - SP1
Show (2010) {tvdb-5} - 01|Show   - 01
_Show_|Show
[only tags] (here)|
Show [unclosed|Show [unclosed
Show [a [b] c]|Show  c]
EOF

# a file named only ".mkv" gets its folder's name, as basename gave it
while IFS='|' read -r path want; do
	file_stem "$path"
	check "file_stem \"$path\"" "$FILE_STEM" "$want"
done <<'EOF'
lib/Show/Show - 01.mkv|Show - 01
lib/Show/Season 1/a.b.mkv|a.b
lib/Show/.mkv|Show
lib/Show/..mkv|.
lib/Show/noext|noext
-x/y.mkv|y
EOF

# Each setter clears its variable on every path: called in one shell, as the
# script calls it, an answer is followed by none. Each line: the call for an
# answer, the call for none, the variable shown with declare -p.
# shellcheck disable=SC2034 # read by the sourced script's season_of_dir
DIR_SEASON=() ROOT_SEASON=""
while IFS='|' read -r fn hit miss var want; do
	"$fn" "$hit"
	"$fn" "$miss"
	got=$(declare -p "$var")
	check "$fn \"$hit\" then \"$miss\": $var" "${got#declare -* "$var"=}" "$want"
done <<'EOF'
core_number|Show - 05|Show|CORE_NUMBER|"0"
core_credit|Show - NCOP1|Show - 05|CORE_CREDIT|()
core_special|Show - SP2|Show - 05|CORE_SPECIAL|"0"
plex_credit_of|Show - NCOP1|Show - 05|PLEX_CREDIT_OF|()
season_of_dir|Season 02|Bonus|SEASON_OF_DIR|""
ep_pair|S01E02|x1|EP_PAIR|""
capital_name|op1|Show|CAPITAL_NAME|""
name_tails|Show - SP1|Show - 05|NAME_TAILS|()
EOF
season_episode "Show - S02E01" "" 0
season_episode "Show - 05" "" 5
check "season_episode: an answer, then none" "${#SEASON_EPISODE[@]}" "0"

# A name without spaces once its tags are gone uses underscores for them, and
# is read like the same name with spaces, also when spaces are left around the
# tags.
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
Tails Ep 13 - Christmas Special|13
Tails Episode 7 - Preview|7
Tails E08 Trailer|8
Tails 07 - The Last Special|0
[Grp]_Show_-_05v2_[1080p]|5
[Grp]_Show_Ep_12|12
Show_S01E05|0
Show_Name|0
[Grp] Show_-_02 [1080p]|2
[Grp] Show_-_02v2 [1080p] [ABCD1234]|2
My Show_-_03|0
EOF

while IFS='|' read -r name want; do
	check "credit_of \"$name\"" "$(credit_of "$name")" "$want"
done <<'EOF'
Show - OP1|op 1 - OP1
Show - NCED2|ed 2 - NCED2
Show Ep 05 - ED|
Show E05 NCOP1|
Show - NCOP1|op 1 - NCOP1
Show - OP1b|op 1 b OP1b
Show - ED|ed 1 - ED
Show - Ending 2a|ed 2 a Ending 2a
[Group] Show - ncop2 [1080p]|op 2 - ncop2
Show - 05|
Show - S01E01|
[Grp]_Show_-_NCOP1_[1080p]|op 1 - NCOP1
EOF

# the same answer whatever the machine's locale: Japanese titles are matched
# as bytes, and full-width characters are turned into ASCII first
for loc in C C.UTF-8; do
	while IFS='|' read -r name want; do
		check "credit_title \"$name\" ($loc)" "$(LC_ALL=$loc credit_title "$name")" "$want"
	done <<'EOF'
Opening 1a|1 a
Opening 1 (b)|1 b
Ending|1 -
Ending 2: Song|2 -
Episode 1|
オープニング|1 -
エンディング 2|2 -
オープニング２|2 -
オープニング　３b|3 b
オープニング1（ｂ）|1 b
オープニング１ｂ|1 b
Sobakasu|
EOF
done

while IFS='|' read -r name want; do
	check "special_number \"$name\"" "$(special_number "$name")" "$want"
done <<'EOF'
Show - SP03|3
Show - OVA|1
Show - PV|201
Show - Trailer 2|202
Show - 05|0
Show - S01E05|0
Show - SP1|1
Show - Special 2|2
Show OVA 2|2
Show - Trailer|201
Show - PV1|201
[Group] Show - 05 [1080p]|0
Show - S01E05 - The Special|0
Show - 13 - Christmas Special|0
Tails Ep 13 - Christmas Special|0
Tails Episode 7 - Preview|0
Tails E08 Trailer|0
Tails 07 - The Last Special|1
[Grp]_Show_-_SP1_[1080p]|1
Show_PV2|202
Show_Name|0
Show_S01E05_Special|0
[Grp] Show_-_SP1 [1080p]|1
[Grp] Show_-_PV2 [1080p]|202
EOF
# A numbered episode is no special, whatever its title ends in. "Tails 07 -
# The Last Special" is the form the script can't tell apart: a number before
# the dash is part of many titles ("Show 2 - Special", "Mob Psycho 100 -
# OVA"), so it stays a special, and the user renames it or hints it by hand.

# number_like NAME SEASON: a name with what may be an episode number is never
# reported as having none. In a season folder any digit counts.
while IFS='|' read -r name season want; do
	got=no
	if number_like "$name" "$season"; then got=yes; fi
	check "number_like \"$name\" \"$season\"" "$got" "$want"
done <<'EOF'
Show 01 - Title||yes
Show - 1x01||yes
Show.01.Title.1080p||yes
[Grp]_Show_01_[1080p]||yes
Show Part A||no
Show - Recap||no
Show 2016 Recap||no
Show 1920x1080 Recap||no
Show Recap [01]||no
Show01 Title|1|yes
Show01 Title||no
Making Of|0|no
EOF

while IFS='|' read -r name want; do
	got=no
	if plex_extra "$name"; then got=yes; fi
	check "plex_extra \"$name\"" "$got" "$want"
done <<'EOF'
Movie (2016)-trailer|yes
Movie (2016)-Trailer|yes
Show-behindthescenes|yes
Show-deleted|yes
Show-featurette|yes
Show-interview|yes
Show-scene|yes
Show-short|yes
Show-other|yes
Movie (2016) - Trailer|no
Movie (2016) - Other|no
Show - 05|no
Movie (2016)-trailers|no
EOF

while IFS='|' read -r name want; do
	check "plex_credit \"$name\"" "$(plex_credit "$name")" "$want"
done <<'EOF'
Show - NCOP1|extra NCOP1
Show - OP1b|extra OP1b
Show - ED 2|extra ED 2
Show - OPED|extra OPED
Show - Creditless Opening|extra Creditless Opening
Show - Creditless Ending|extra Creditless Ending
Show - Clean Ending 2|extra Clean Ending 2
Show - 05 [ED fix]|extra ED
Show - Opening 1|dropped Opening 1
Show - Ending|dropped Ending
Show - NCOpening 1|dropped NCOpening 1
Show - 05 [Ending fix]|dropped Ending
Show Ending - NCED1|dropped Ending
Show - S01E05 [ED fix]|
Show - s01e05 - NCOP1|
Show.S01.E05.NCOP1|
Show - S01 05 - Opening 1|
Show - S1 - NCOP1|extra NCOP1
Show - op1|
Show - ncop1|
Show - opening 1|
Show - 05|
Show - Operation|
EOF
# What Plex makes of a lowercase "op1" is not known: its pattern doesn't
# match, so the file is no extra. The script reports it, without a hint.

while IFS='|' read -r name want; do
	check "capital_credit \"$name\"" "$(capital_credit "$name")" "$want"
done <<'EOF'
op1|OP1
ncop2|NCOP2
ending 2A|ED2a
Opening 01|OP1
ed|ED
Show|
EOF

while IFS='|' read -r name want; do
	check "short_credit \"$name\"" "$(short_credit "$name")" "$want"
done <<'EOF'
Opening 1|OP1
Ending|ED
NCOpening 1|NCOP1
Ending_2a|ED2a
Opening a|OP a
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

# One listing of a show's files for its .plexmatch files and its videos
show=$WORK/list/Show
mkdir -p "$show/Season 1" "$show/b" "$show/extras" "$show/.hidden" "$WORK/list/Empty"
for f in "b/x.mkv" "a.mkv" "Season 1/c.MP4" "notes.txt" "extras/e.mkv" ".hidden/h.mkv" ".plexmatch" "b/.plexmatch"; do
	: >"$show/$f"
done
show_files "$show"
check "show_files: every file, hidden and extras folders left out" \
	"$(printf '%s\n' "${SHOW_FILES[@]#"$show"/}" | LC_ALL=C sort | tr '\n' '|')" ".plexmatch|Season 1/c.MP4|a.mkv|b/.plexmatch|b/x.mkv|notes.txt|"
list_videos
check "list_videos: the videos, sorted" "$(printf '%s|' "${LIST_VIDEOS[@]#"$show"/}")" "Season 1/c.MP4|a.mkv|b/x.mkv|"
show_files "$WORK/list/Empty"
list_videos
check "list_videos: none" "${#SHOW_FILES[@]} ${#LIST_VIDEOS[@]}" "0 0"

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

# A hint is known by the same key as the file it names (hint_key), however
# it's written: a byte order mark is no part of the first line, "./" names the
# same folder, the spaces at the start of a name are dropped on both sides.
for c in " a.mkv|a.mkv" "./a.mkv|a.mkv" ".\\ a.mkv|a.mkv" "././a.mkv|a.mkv" "Season 02/ ./b.mkv|Season 02/b.mkv" \
	" Season 02/ b.mkv|Season 02/b.mkv" "a\\b.mkv|a/b.mkv" "Show .mkv|Show .mkv" "Case.MKV|Case.MKV" "./|"; do
	hint_key "${c%%|*}"
	check "hint_key \"${c%%|*}\"" "$HINT_KEY" "${c#*|}"
done
show=$WORK/read-names/Show
mkdir -p "$show/Season 03"
printf '\357\273\277ep: S00E01: bom.mkv\r\nep: S00E02: ./dot.mkv\r\nep: S00E03: .\\Season 03\\back.mkv\r\nep: S00E04:   pad.mkv \t \r\nEP:s00e05:tight.mkv\r\nep: S00E06: Case.MKV\r\n' >"$show/.plexmatch"
printf '\357\273\277title: Ignored Below\nep: 07: ./ sub.mkv\n' >"$show/Season 03/.plexmatch"
read_plexmatches "$show"
check "ep: hint on a first line with a byte order mark" "${USER_EP[bom.mkv]:-}" "S00E01"
check "ep: hint with ./" "${USER_EP[dot.mkv]:-}" "S00E02"
check "ep: hint with .\\ and a backslash" "${USER_EP[Season 03/back.mkv]:-}" "S00E03"
check "ep: hint with spaces around the name" "${USER_EP[pad.mkv]:-}" "S00E04"
check "EP: hint without any space, small letters in the number" "${USER_EP[tight.mkv]:-}" "s00e05"
check "ep: hint keeps its letter case" "${USER_EP[Case.MKV]:-}${USER_EP[case.mkv]:-}" "S00E06"
check "ep: hint with ./ and a space in a subfolder, after a byte order mark" "${USER_EP[Season 03/sub.mkv]:-}" "07"
check "ep: hints read, and no other" "${#USER_EP[@]}" "7"
printf '\357\273\277title: Marked\nyear: 2001\n' >"$show/.plexmatch"
read_plexmatches "$show"
check "title: hint on a first line with a byte order mark" "$SHOW_TITLE" "Marked"

# user_ep finds a hint whose name differs only in letter case, unless another
# video of the show has that name in other letters (two files on Linux), and
# says how the hint writes a name that isn't the file's plain path
show=$WORK/read-user-ep/Show
mkdir -p "$show/Sub"
printf 'ep: S00E06: Case.MKV\nep: S00E01: x.mkv\nep: S00E02: ./dot.mkv\n' >"$show/.plexmatch"
printf 'ep: 3: .\\sub.mkv\nep: 4: plain.mkv\n' >"$show/Sub/.plexmatch"
read_plexmatches "$show"
VIDEO_LC=([case.mkv]=1 [x.mkv]=1)
user_ep "x.mkv"
check "user_ep: exact name" "$HAVE|$HAVE_AS|$HAVE_IN" "S00E01||"
user_ep " x.mkv"
check "user_ep: a name that starts with a space, written plainly" "$HAVE|$HAVE_AS|$HAVE_IN" "S00E01||"
user_ep "case.mkv"
check "user_ep: other letter case" "$HAVE|$HAVE_AS|$HAVE_IN" "S00E06|Case.MKV|"
user_ep "dot.mkv"
check "user_ep: written ./" "$HAVE|$HAVE_AS|$HAVE_IN" "S00E02|./dot.mkv|"
user_ep "Sub/sub.mkv"
check "user_ep: written .\\ in a subfolder's .plexmatch" "$HAVE|$HAVE_AS|$HAVE_IN" "3|.\\sub.mkv|Sub"
user_ep "Sub/plain.mkv"
check "user_ep: written plainly in a subfolder's .plexmatch" "$HAVE|$HAVE_AS|$HAVE_IN" "4||"
VIDEO_LC=([case.mkv]=2)
user_ep "case.mkv"
check "user_ep: other letter case, two such videos" "$HAVE|$HAVE_AS" "|"
user_ep "Case.MKV"
check "user_ep: exact name, two such videos" "$HAVE|$HAVE_AS" "S00E06|"
# shellcheck disable=SC2034 # read by the sourced script
USER_EP=() USER_EP_LC=() USER_EP_AS=() USER_EP_DIR=() USER_EP_SK=() VIDEO_LC=()
WARNINGS=()

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

# openings and endings are reported and never hinted, whether Plex makes them
# extras, leaves them out, or doesn't take the name (small letters)
lib=$WORK/plex-credits
show="$lib/Show [anidb-1]"
mkdir -p "$show"
for f in "Show - 01" "Show - Opening 1" "Show - Ending" "Show - NCOpening 1" "Show - S01E05 [ED fix]" \
	"Show - 05 [ED fix]" "Show - 06 [Ending fix]" "Show - Creditless Opening" "Show - Clean Ending 2" \
	"Show - NCOP1" "Show - S01.E07 - NCOP2" "Show - op1" "Show - ncop2"; do
	: >"$show/$f.mkv"
done
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$lib" 2>&1)
check "plex credits: exit" "$?" "0"
# line_of FILE: the WARNING line of one file
line_of() { grep -F "WARNING: $show/$1.mkv: " <<<"$out"; }
# by_hand TEXT NUMBER: the end of a note, on making the file a special
by_hand() {
	printf '%s' "If you want it as a special, that is yours to do: rename it to a name without \"$1\" or another such word, and add \"ep: $2: <new name>\" to the show's .plexmatch by hand ($2 is its number from the file name, right unless AniDB has variants of it)"
}
stays="opening/ending extras and ignores .plexmatch hints for them, so it stays an extra (it still plays from the show page) and gets no hint. "
left="out of the library, as neither an extra nor an episode, so it gets no hint. To have it as an extra, rename it with "
check "plex credits: Opening 1 is left out" "$(line_of "Show - Opening 1")" \
	"WARNING: $show/Show - Opening 1.mkv: Plex's scanner leaves files with the whole word \"Opening 1\" in the name ${left}OP1 in place of that, e.g. to \"Show - OP1.mkv\". $(by_hand "Opening 1" S00E101)"
check "plex credits: Ending is left out" "$(line_of "Show - Ending")" \
	"WARNING: $show/Show - Ending.mkv: Plex's scanner leaves files with the whole word \"Ending\" in the name ${left}ED in place of that, e.g. to \"Show - ED.mkv\". $(by_hand "Ending" S00E151)"
check "plex credits: NCOpening 1 is left out" "$(line_of "Show - NCOpening 1")" \
	"WARNING: $show/Show - NCOpening 1.mkv: Plex's scanner leaves files with the whole word \"NCOpening 1\" in the name ${left}NCOP1 in place of that, e.g. to \"Show - NCOP1.mkv\". $(by_hand "NCOpening 1" S00E101)"
check "plex credits: Creditless Opening stays an extra" "$(line_of "Show - Creditless Opening")" \
	"WARNING: $show/Show - Creditless Opening.mkv: Plex's scanner makes files with \"Creditless Opening\" in the name $stays$(by_hand "Creditless Opening" S00E101)"
check "plex credits: Clean Ending 2 stays an extra" "$(line_of "Show - Clean Ending 2")" \
	"WARNING: $show/Show - Clean Ending 2.mkv: Plex's scanner makes files with \"Clean Ending 2\" in the name $stays$(by_hand "Clean Ending 2" S00E152)"
check "plex credits: NCOP1 stays an extra" "$(line_of "Show - NCOP1")" \
	"WARNING: $show/Show - NCOP1.mkv: Plex's scanner makes files with \"NCOP1\" in the name $stays$(by_hand "NCOP1" S00E101)"
check "plex credits: an episode with ED in its name" "$(line_of "Show - 05 [ED fix]")" \
	"WARNING: $show/Show - 05 [ED fix].mkv: Plex's scanner makes it an opening/ending extra, not an episode: its name contains \"ED\". Rename it without that"
check "plex credits: an episode with Ending in its name" "$(line_of "Show - 06 [Ending fix]")" \
	"WARNING: $show/Show - 06 [Ending fix].mkv: Plex's scanner leaves it out of the library: its name contains the whole word \"Ending\". Rename it without that"
check "plex credits: SxxEyy is an episode, no warning" "$(grep -c 'S01E05' <<<"$out")" "0"
check "plex credits: Sxx.Eyy is an episode, as Plex reads it" "$(grep -c 'S01\.E07' <<<"$out")" "0"
small="Plex's scanner only takes these words in capitals, so \"%s\" doesn't make it an opening/ending extra, and the script writes no hint for openings and endings. To have it as an extra (Kizuna can then give it a readable title), rename it with %s in place of that, e.g. to \"Show - %s.mkv\". "
# shellcheck disable=SC2059 # the format is the message above
check "plex credits: op1 is no extra, and not hinted" "$(line_of "Show - op1")" \
	"WARNING: $show/Show - op1.mkv: $(printf "$small" op1 OP1 OP1)$(by_hand op1 S00E101)"
# shellcheck disable=SC2059
check "plex credits: ncop2 is no extra, and not hinted" "$(line_of "Show - ncop2")" \
	"WARNING: $show/Show - ncop2.mkv: $(printf "$small" ncop2 NCOP2 NCOP2)$(by_hand ncop2 S00E102)"
check "plex credits: no hint at all" "$(grep -c '^  + ' <<<"$out")" "0"
check_contains "plex credits: summary" "$out" "0 .plexmatch files to write, 0 hints in all, 0 files already have a hint, 10 warnings"
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" -a "$lib" 2>&1)
check "plex credits, apply: exit" "$?" "0"
check "plex credits, apply: no .plexmatch written, no file renamed" "$(ls -A "$show" | wc -l | tr -d ' ')" "13"

# a numbered episode whose title ends in a word for a special, trailer or
# ending is an episode: no hint. Real specials next to it keep theirs.
lib=$WORK/numbered
show="$lib/Tails [anidb-1]"
mkdir -p "$show"
for f in "Tails Ep 13 - Christmas Special" "Tails Episode 7 - Preview" "Tails E08 Trailer" "Tails Ep 09 - OVA" \
	"Tails - 10 - The Special" "Tails - S01E05 - The Special" "[Group] Tails - 05 [1080p]" \
	"Tails 07 - The Last Special" "Tails - SP2" "Tails - Special 3" "Tails OVA 4" "Tails - Trailer" "Tails - PV2" \
	"Tails - OP1" "Tails - NCED2"; do
	: >"$show/$f.mkv"
done
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$lib" 2>&1)
check "numbered: exit" "$?" "0"
check "numbered: the hints, and no other" "$(grep '^  + ' <<<"$out")" \
	"  + ep: S00E202: Tails - PV2.mkv
  + ep: S00E02: Tails - SP2.mkv
  + ep: S00E03: Tails - Special 3.mkv
  + ep: S00E201: Tails - Trailer.mkv
  + ep: S00E01: Tails 07 - The Last Special.mkv
  + ep: S00E04: Tails OVA 4.mkv"
check "numbered: openings and endings are still reported" "$(grep -c '^WARNING: .*opening/ending extras' <<<"$out")" "2"
check_contains "numbered: summary" "$out" "1 .plexmatch files to write, 6 hints in all, 0 files already have a hint, 2 warnings"

# a movie next to its trailer or creditless ending gets the movie's hint: only
# the videos that are no special, trailer, opening or ending count. Two of
# those get none, as the script can't know which is the movie, and are
# reported unless they have a hint of their own.
lib=$WORK/movies
mkdir -p "$lib/Alone [anidb-2]" "$lib/Blu [anidb-3]" "$lib/Eps [anidb-4]" "$lib/Sxe [anidb-5]" "$lib/Two [anidb-6]" \
	"$lib/Under [anidb-7]"
for f in "Alone [anidb-2]/Alone (2016)" "Alone [anidb-2]/Alone (2016) - PV1" "Blu [anidb-3]/Blu (2016) [BD]" \
	"Blu [anidb-3]/Blu (2016) - NCED [BD]" "Eps [anidb-4]/Eps - 01" "Eps [anidb-4]/Eps - Recap" "Eps [anidb-4]/Eps Talk" \
	"Sxe [anidb-5]/Sxe_S01E05" "Two [anidb-6]/Two Part A" "Two [anidb-6]/Two Part B" "Two [anidb-6]/Two - Trailer" \
	"Under [anidb-7]/[Grp]_Under_-_01_[1080p]" "Under [anidb-7]/[Grp]_Under_-_02_[1080p]" \
	"Under [anidb-7]/[Grp]_Under_-_SP1_[1080p]"; do
	: >"$lib/$f.mkv"
done
printf 'ep: S00E09: Eps Talk.mkv\n' >"$lib/Eps [anidb-4]/.plexmatch"
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$lib" 2>&1)
check "movies: exit" "$?" "0"
check "movies: the hints, and no other" "$(grep '^  + ' <<<"$out")" \
	"  + ep: S00E201: Alone (2016) - PV1.mkv
  + ep: S01E01: Alone (2016).mkv
  + ep: S01E01: Blu (2016) [BD].mkv
  + ep: S00E201: Two - Trailer.mkv
  + ep: S00E01: [Grp]_Under_-_SP1_[1080p].mkv"
# no_number DIR FILE: the warning for a video without a number that is no lone movie
no_number() {
	printf '%s' "WARNING: $lib/$1/$2: no episode number found, and the folder has other episodes or movies, so it isn't hinted as a lone movie. If Plex's scanner skips it, rename it with its number, or add its hint by hand (ep: S01E01: $2 for a movie)"
}
check "movies: an unnumbered video next to episodes is reported" "$(grep -F 'Eps - Recap' <<<"$out")" \
	"$(no_number "Eps [anidb-4]" "Eps - Recap.mkv")"
check "movies: two unnumbered videos are reported" "$(grep -F 'Two Part' <<<"$out")" \
	"$(no_number "Two [anidb-6]" "Two Part A.mkv")"$'\n'"$(no_number "Two [anidb-6]" "Two Part B.mkv")"
check "movies: one with a hint of its own is not" "$(grep -c 'Eps Talk' <<<"$out")" "0"
check "movies: a name Plex reads a season and episode in is no movie" "$(grep -c 'Sxe_S01E05' <<<"$out")" "0"
check "movies: underscore episodes are read, not reported" "$(grep -cE 'Under_-_0[12]' <<<"$out")" "0"
check_contains "movies: summary" "$out" "4 .plexmatch files to write, 5 hints in all, 1 files already have a hint, 4 warnings"

# Only a video with nothing that may be its number is reported: shows of
# episodes numbered in a way Plex reads stay silent. Underscore names with
# spaces left around their tags are read too, and a file with Plex's -trailer
# suffix is an extra, so the movie next to it still gets its hint. An episode
# or a lone video with such an ending is reported instead.
lib=$WORK/unnumbered
for f in "Spaced [anidb-8]/[Grp] Spaced_-_01 [1080p]" "Spaced [anidb-8]/[Grp] Spaced_-_02 [1080p]" \
	"Spaced [anidb-8]/[Grp] Spaced_-_SP1 [1080p]" "Titled [anidb-9]/Titled 01 - Title" \
	"Titled [anidb-9]/Titled 02 - Other" "Cross [anidb-10]/Cross - 1x01" "Cross [anidb-10]/Cross - 1x02" \
	"Dots [anidb-11]/Dots.01.Title.1080p" "Dots [anidb-11]/Dots.02.Other.1080p" \
	"Seasons [anidb-12]/Season 01/Seasons 01 Title" "Seasons [anidb-12]/Season 01/Seasons 02 Title" \
	"Seasons [anidb-12]/Specials/Making Of" "Film [anidb-13]/Film (2016)" "Film [anidb-13]/Film (2016)-trailer" \
	"Love [anidb-14]/Love - 01" "Love [anidb-14]/Love - 02 - Love-scene" "Sig [anidb-15]/Sig - S01E02" \
	"Sig [anidb-15]/Sig - S01E03 - Significant-Other" "Lone [anidb-16]/Lone-Short"; do
	mkdir -p "$lib/${f%/*}"
	: >"$lib/$f.mkv"
done
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$lib" 2>&1)
check "unnumbered: exit" "$?" "0"
check "unnumbered: the hints, and no other" "$(grep '^  + ' <<<"$out")" \
	"  + ep: S01E01: Film (2016).mkv
  + ep: S00E01: [Grp] Spaced_-_SP1 [1080p].mkv"
check "unnumbered: only the file without anything like a number is reported, with its season" \
	"$(grep '^WARNING: .*no episode number found' <<<"$out")" \
	"WARNING: $lib/Seasons [anidb-12]/Specials/Making Of.mkv: no episode number found, and the folder has other episodes or movies, so it isn't hinted as a lone movie. If Plex's scanner skips it, rename it with its number, or add its hint by hand (ep: S00Exx: Specials/Making Of.mkv)"
# extra_name DIR FILE SUFFIX: the warning for an episode or lone video Plex makes an extra by its ending
extra_name() {
	printf '%s' "WARNING: $lib/$1/$2: Plex's scanner makes it an extra, not an episode or a movie: its name ends in \"$3\". Rename it without that"
}
check "unnumbered: a numbered episode with an extra's ending is reported" "$(grep -F 'Love-scene' <<<"$out")" \
	"$(extra_name "Love [anidb-14]" "Love - 02 - Love-scene.mkv" -scene)"
check "unnumbered: an SxxEyy episode with an extra's ending is reported" "$(grep -F 'Significant-Other' <<<"$out")" \
	"$(extra_name "Sig [anidb-15]" "Sig - S01E03 - Significant-Other.mkv" -Other)"
check "unnumbered: a lone video with an extra's ending is reported" "$(grep -F 'Lone-Short' <<<"$out")" \
	"$(extra_name "Lone [anidb-16]" "Lone-Short.mkv" -Short)"
check "unnumbered: a trailer next to its movie is not" "$(grep -c 'Film (2016)-trailer' <<<"$out")" "0"
check_contains "unnumbered: summary" "$out" "2 .plexmatch files to write, 2 hints in all, 0 files already have a hint, 4 warnings"

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

# several files of one show on the same number are reported, once per number,
# when a planned hint is one of them, and the hints are planned all the same.
# Not reported: a version of the same file (SP3v2), two files the user's own
# hints put on one number, and a bare number in a season folder's .plexmatch,
# which is that season's.
lib=$WORK/same
mkdir -p "$lib/Coll [anidb-1]" "$lib/Kept [anidb-2]/Season 02"
for f in "Coll - SP1" "Coll - OVA1" "Coll - Special 1" "Coll - SP3" "Coll - SP3v2 [1080p]"; do
	: >"$lib/Coll [anidb-1]/$f.mkv"
done
for f in "Kept - 08" "Kept - 09" "Kept - SP2" "Kept - SP3" "Season 02/Kept - Bonus"; do : >"$lib/Kept [anidb-2]/$f.mkv"; done
printf 'ep: s0e3: Kept - SP2.mkv\nep: S01E09: Kept - 08.mkv\nep: S01E09: Kept - 09.mkv\n' >"$lib/Kept [anidb-2]/.plexmatch"
printf 'ep: 03: Kept - Bonus.mkv\n' >"$lib/Kept [anidb-2]/Season 02/.plexmatch"
# same DIR NUMBER FILES: the warning for files on one number
same() {
	printf '%s' "WARNING: $1: more than one file is on $2, and Plex shows them as one episode: $3. If they are different episodes, give each its own number: rename the file, or set its ep: line in the show's .plexmatch by hand"
}
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$lib" 2>&1)
check "same number: exit" "$?" "0"
check "same number: three specials planned on one number" "$(grep -F 'is on S00E01' <<<"$out")" \
	"$(same "$lib/Coll [anidb-1]" S00E01 '"Coll - OVA1.mkv", "Coll - SP1.mkv", "Coll - Special 1.mkv"')"
check "same number: a planned hint and one already there" "$(grep -F 'is on S00E03' <<<"$out")" \
	"$(same "$lib/Kept [anidb-2]" S00E03 '"Kept - SP2.mkv" (already hinted), "Kept - SP3.mkv"')"
check "same number: no other number is reported" "$(grep -c 'more than one file is on' <<<"$out")" "2"
check "same number: the hints are planned all the same" "$(grep '^  + ' <<<"$out")" \
	"  + ep: S00E01: Coll - OVA1.mkv
  + ep: S00E01: Coll - SP1.mkv
  + ep: S00E03: Coll - SP3.mkv
  + ep: S00E03: Coll - SP3v2 [1080p].mkv
  + ep: S00E01: Coll - Special 1.mkv
  + ep: S00E03: Kept - SP3.mkv"
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" -a "$lib" 2>&1)
check "same number, apply: exit" "$?" "0"
check "same number, apply: reported again" "$(grep -c 'more than one file is on' <<<"$out")" "2"
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$lib" 2>&1)
check "same number, next run: the written hints are the user's now, nothing is reported" \
	"$(grep -c 'more than one file is on' <<<"$out")" "0"

# Two specials in a season folder are told apart by their names, not by the
# folder's season and the number at the end of the name. A file Plex numbers
# by its own name or folder counts too. One episode in two versions stays
# silent, also with an underscore name in a folder with a space. A file Plex
# makes an extra or leaves out isn't counted, whatever its hint says. A lone
# movie, trailers and the first number of a range hint are compared too.
lib=$WORK/same2
mkdir -p "$lib/Sea [anidb-1]/Season 01" "$lib/Sea [anidb-1]/Bonus" "$lib/Nm [anidb-1]/Specials" \
	"$lib/Two [anidb-1]/Season 02" "$lib/Ex [anidb-1]" "$lib/Film [anidb-1]" "$lib/Tr [anidb-1]" "$lib/Rg [anidb-1]"
for f in "Sea [anidb-1]/Season 01/Sea - OVA 2" "Sea [anidb-1]/Season 01/Sea - Special 2" \
	"Sea [anidb-1]/Bonus/Sea - OVA 3" "Sea [anidb-1]/Bonus/Sea - Special 3" \
	"Nm [anidb-1]/Nm - SP1" "Nm [anidb-1]/Nm - S00E01" "Nm [anidb-1]/Specials/Nm - 01" \
	"Two [anidb-1]/Two - SP1 [720p]" "Two [anidb-1]/Two - SP1 [1080p]" \
	"Two [anidb-1]/Season 02/Two_-_SP2" "Two [anidb-1]/Season 02/Two - SP2 [1080p]" \
	"Ex [anidb-1]/Ex - PV1" "Ex [anidb-1]/Ex - PV1v2" "Ex [anidb-1]/Ex-trailer" \
	"Ex [anidb-1]/Ex - SP3" "Ex [anidb-1]/Ex - Special 3" \
	"Film [anidb-1]/Film" "Film [anidb-1]/Film - SP1" \
	"Tr [anidb-1]/Tr - PV1" "Tr [anidb-1]/Tr - Trailer 1" \
	"Rg [anidb-1]/Rg - SP4" "Rg [anidb-1]/Rg - SP5" "Rg [anidb-1]/Rg - OVA"; do
	: >"$lib/$f.mkv"
done
printf 'season: 1\n' >"$lib/Sea [anidb-1]/Bonus/.plexmatch"
printf 'ep: S00E201: Ex-trailer.mkv\nep: sp03: Ex - Special 3.mkv\n' >"$lib/Ex [anidb-1]/.plexmatch"
printf 'ep: S01E01: Film - SP1.mkv\n' >"$lib/Film [anidb-1]/.plexmatch"
printf 'ep: S00E04-E05: Rg - OVA.mkv\n' >"$lib/Rg [anidb-1]/.plexmatch"
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$lib" 2>&1)
check "same number 2: exit" "$?" "0"
check "same number: two specials in a season folder" "$(grep -F 'is on S00E02' <<<"$out")" \
	"$(same "$lib/Sea [anidb-1]" S00E02 '"Season 01/Sea - OVA 2.mkv", "Season 01/Sea - Special 2.mkv"')"
check "same number: two specials in a folder with a season: hint" "$(grep -F 'is on S00E03' <<<"$out")" \
	"$(same "$lib/Sea [anidb-1]" S00E03 '"Bonus/Sea - OVA 3.mkv", "Bonus/Sea - Special 3.mkv"')"
check "same number: files Plex numbers by their name or folder" "$(grep -F "$lib/Nm [anidb-1]: more" <<<"$out")" \
	"$(same "$lib/Nm [anidb-1]" S00E01 '"Nm - S00E01.mkv" (numbered by its name), "Nm - SP1.mkv", "Specials/Nm - 01.mkv" (numbered by its name)')"
check "same number: two versions, also with an underscore name, are one episode" \
	"$(grep -cF "$lib/Two [anidb-1]: more" <<<"$out")" "0"
check "same number: an extra and an SPxx hint aren't counted" "$(grep -cF "$lib/Ex [anidb-1]: more" <<<"$out")" "0"
check "same number: a lone movie" "$(grep -F "$lib/Film [anidb-1]: more" <<<"$out")" \
	"$(same "$lib/Film [anidb-1]" S01E01 '"Film - SP1.mkv" (already hinted), "Film.mkv"')"
check "same number: trailers" "$(grep -F "$lib/Tr [anidb-1]: more" <<<"$out")" \
	"$(same "$lib/Tr [anidb-1]" S00E201 '"Tr - PV1.mkv", "Tr - Trailer 1.mkv"')"
check "same number: a range hint, with its first number" "$(grep -F "$lib/Rg [anidb-1]: more" <<<"$out")" \
	"$(same "$lib/Rg [anidb-1]" S00E04 '"Rg - OVA.mkv" (already hinted), "Rg - SP4.mkv"')"
check "same number 2: no other number is reported" "$(grep -c 'more than one file is on' <<<"$out")" "6"

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

# --- running it again --------------------------------------------------------------

# A hint already there is recognised however it or its file is written, so the
# script adds no line next to it, and a second run changes nothing. Each case
# is a library of one show, "Show [anidb-1]"; the last has them all at once.
# hex: the bytes on stdin, spaces included (bytes drops them)
hex() { od -An -tx1 | tr -d ' \n'; }
# again CASE WANT: runs -a twice on $WORK/again/CASE. The first run must leave
# the show's .plexmatch as WANT, the second must write nothing and leave it
# byte for byte, and a dry run after them must plan nothing. The first run's
# output is left in $first_out, the dry run's in $out.
again() {
	local lib=$WORK/again/$1 pm="$WORK/again/$1/Show [anidb-1]/.plexmatch" first=$WORK/again/$1.first
	# shellcheck disable=SC2034 # read by name, through ${!run}
	first_out=$(bash "$SCRIPT" -u "$NO_KIZUNA" -a "$lib" 2>&1)
	check "again, $1: first run exit" "$?" "0"
	check "again, $1: first run adds only the missing hints" "$(hex <"$pm")" "$(printf '%s' "$2" | hex)"
	cp -p "$pm" "$first"
	out=$(bash "$SCRIPT" -u "$NO_KIZUNA" -a "$lib" 2>&1)
	check_contains "again, $1: second run writes nothing" "$out" "0 .plexmatch files to write, 0 hints in all"
	check "again, $1: second run leaves the file byte for byte" "$(cmp "$first" "$pm" && echo same)" "same"
	out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$lib" 2>&1)
	check "again, $1: a dry run after them plans nothing" "$(grep -c '^  + ' <<<"$out")" "0"
}
# videos CASE NAME...: empty videos in the case's show folder
videos() {
	local s="$WORK/again/$1/Show [anidb-1]" f
	shift
	for f in "$@"; do mkdir -p "$(dirname "$s/$f")"; : >"$s/$f"; done
}
BOM=$'\357\273\277'
# A hint that names its file in a way Plex may not read counts as the file's,
# and the script warns to correct it. The warnings:
# bom_warn PLEXMATCH: one per such file
bom_warn() {
	printf '%s' "WARNING: $1: starts with a byte order mark, and Plex may not read its first line. Save the file without it (UTF-8 without BOM)"
}
# as_warn CASE FILE AS EP FOLDER PLAIN: a hint written differently (./, a
# backslash, other capital letters), in FOLDER's .plexmatch
as_warn() {
	printf '%s' "WARNING: $WORK/again/$1/Show [anidb-1]/$2: kept the hint for \"$3\" (ep: $4) in ${5:+$5/}.plexmatch as this file's, so it gets no other: that name is written differently from the file's (./, a backslash or other capital letters), and Plex may not take the line for this file. Write it as \"$6\" by hand"
}
# lead_warn CASE FILE: a name or folder that starts with a space
lead_warn() {
	printf '%s' "WARNING: $WORK/again/$1/Show [anidb-1]/$2: its name or folder starts with a space, and Plex may not match its ep: hint to it. Rename it without the space"
}
NEW_WARNINGS='byte order mark|written differently|starts with a space'
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$WORK/library" 2>&1)
check "again: an ordinary library gets none of these warnings" "$(grep -cE "$NEW_WARNINGS" <<<"$out")" "0"

videos lead " Show - SP1.mkv" "Specials/ Show - SP2.mkv" "Show - SP3.mkv"
printf 'ep: S00E03: Show - SP3.mkv\n' >"$WORK/again/lead/Show [anidb-1]/.plexmatch"
again lead $'ep: S00E03: Show - SP3.mkv\nep: S00E01:  Show - SP1.mkv\nep: S00E02: Specials/ Show - SP2.mkv\n'
for run in first_out out; do # planned on the first run, recognised after it
	check "again, lead ($run): a name that starts with a space is reported" \
		"$(grep -F 'starts with a space' <<<"${!run}")" "$(lead_warn lead " Show - SP1.mkv")"$'\n'"$(lead_warn lead "Specials/ Show - SP2.mkv")"
done
check "again, lead: no other of these warnings" "$(grep -cE 'byte order mark|written differently' <<<"$out")" "0"

# a name that ends in a space before its extension
videos trail "Show - SP1 .mkv" "Show - SP2.mkv"
if [[ $(ls -A "$WORK/again/trail/Show [anidb-1]") == *"SP1 .mkv"* ]]; then
	again trail $'ep: S00E01: Show - SP1 .mkv\nep: S00E02: Show - SP2.mkv\n'
else
	echo "SKIP check of a name with a space before its extension: this filesystem can't hold one"
fi

videos bom "Show - SP1.mkv" "Show - SP2.mkv"
printf '%sep: S00E01: Show - SP1.mkv\r\ntitle: Show\r\n' "$BOM" >"$WORK/again/bom/Show [anidb-1]/.plexmatch"
again bom "${BOM}"$'ep: S00E01: Show - SP1.mkv\r\ntitle: Show\r\nep: S00E02: Show - SP2.mkv\r\n'
check "again, bom: reported once for the file" "$(grep -F 'byte order mark' <<<"$out")" \
	"$(bom_warn "$WORK/again/bom/Show [anidb-1]/.plexmatch")"
videos bom-title "Show - SP1.mkv"
printf '%stitle: Show\n' "$BOM" >"$WORK/again/bom-title/Show [anidb-1]/.plexmatch"
again bom-title "${BOM}"$'title: Show\nep: S00E01: Show - SP1.mkv\n'

videos dot "Show - SP1.mkv" "Show - SP2.mkv" "Season 01/Show - SP3.mkv" "Show - SP4.mkv"
printf 'ep: S00E01: ./Show - SP1.mkv\nep: S00E02: .\\Show - SP2.mkv\nep: S00E03: ./Season 01/./Show - SP3.mkv\n' \
	>"$WORK/again/dot/Show [anidb-1]/.plexmatch"
again dot $'ep: S00E01: ./Show - SP1.mkv\nep: S00E02: .\\Show - SP2.mkv\nep: S00E03: ./Season 01/./Show - SP3.mkv\nep: S00E04: Show - SP4.mkv\n'
check "again, dot: each hint written differently is reported" "$(grep -F 'written differently' <<<"$out")" \
	"$(as_warn dot "Season 01/Show - SP3.mkv" "./Season 01/./Show - SP3.mkv" S00E03 "" "Season 01/Show - SP3.mkv")
$(as_warn dot "Show - SP1.mkv" "./Show - SP1.mkv" S00E01 "" "Show - SP1.mkv")
$(as_warn dot "Show - SP2.mkv" ".\\Show - SP2.mkv" S00E02 "" "Show - SP2.mkv")"

videos case "Show - SP1.mkv" "Show - SP2.mkv" "Show - SP3.mkv"
printf 'EP: s00e01: Show - SP1.mkv\nEp:S00E02:show - sp2.MKV\n' >"$WORK/again/case/Show [anidb-1]/.plexmatch"
again case $'EP: s00e01: Show - SP1.mkv\nEp:S00E02:show - sp2.MKV\nep: S00E03: Show - SP3.mkv\n'
check "again, case: a hint in other letters is reported" "$(grep -F 'written differently' <<<"$out")" \
	"$(as_warn case "Show - SP2.mkv" "show - sp2.MKV" S00E02 "" "Show - SP2.mkv")"

videos crlf "Show - SP1.mkv" "Show - SP2.mkv" " Show - SP3.mkv"
printf 'title: Show\r\nep: S00E01: Show - SP1.mkv' >"$WORK/again/crlf/Show [anidb-1]/.plexmatch" # no line end at the end
again crlf $'title: Show\r\nep: S00E01: Show - SP1.mkv\r\nep: S00E03:  Show - SP3.mkv\r\nep: S00E02: Show - SP2.mkv\r\n'

# all at once, with a subfolder .plexmatch that has a byte order mark too
videos mixed " Show - SP1.mkv" "Show - SP3.mkv" "Show - SP4.mkv" "Show - SP5.mkv" \
	"Specials/ Show - SP6.mkv" "Specials/Show - SP7.mkv" "Show - SP8.mkv"
printf '%sEP:S00E03: ./Show - SP3.mkv\r\nep: s00e04: show - sp4.mkv\r\n' "$BOM" >"$WORK/again/mixed/Show [anidb-1]/.plexmatch"
printf '%sep: S00E07: .\\Show - SP7.mkv\r\n' "$BOM" >"$WORK/again/mixed/Show [anidb-1]/Specials/.plexmatch"
again mixed "${BOM}"$'EP:S00E03: ./Show - SP3.mkv\r\nep: s00e04: show - sp4.mkv\r\nep: S00E01:  Show - SP1.mkv\r\nep: S00E05: Show - SP5.mkv\r\nep: S00E08: Show - SP8.mkv\r\nep: S00E06: Specials/ Show - SP6.mkv\r\n'
check "again, mixed: the subfolder's .plexmatch untouched" "$(hex <"$WORK/again/mixed/Show [anidb-1]/Specials/.plexmatch")" \
	"$(printf '%sep: S00E07: .\\Show - SP7.mkv\r\n' "$BOM" | hex)"
check "again, mixed: the warnings" "$(grep -E "$NEW_WARNINGS" <<<"$out" | LC_ALL=C sort)" \
	"$(printf '%s\n' "$(bom_warn "$WORK/again/mixed/Show [anidb-1]/.plexmatch")" \
		"$(bom_warn "$WORK/again/mixed/Show [anidb-1]/Specials/.plexmatch")" \
		"$(lead_warn mixed " Show - SP1.mkv")" "$(lead_warn mixed "Specials/ Show - SP6.mkv")" \
		"$(as_warn mixed "Show - SP3.mkv" "./Show - SP3.mkv" S00E03 "" "Show - SP3.mkv")" \
		"$(as_warn mixed "Show - SP4.mkv" "show - sp4.mkv" s00e04 "" "Show - SP4.mkv")" \
		"$(as_warn mixed "Specials/Show - SP7.mkv" ".\\Show - SP7.mkv" S00E07 Specials "Show - SP7.mkv")" |
		LC_ALL=C sort)"

# a hint the script doesn't read as the file's ("../" from a subfolder): it
# adds one plain line, once
videos up "Show - SP1.mkv"
mkdir -p "$WORK/again/up/Show [anidb-1]/Specials"
printf 'ep: S00E01: ../Show - SP1.mkv\n' >"$WORK/again/up/Show [anidb-1]/Specials/.plexmatch"
again up $'ep: S00E01: Show - SP1.mkv\n'

# same_numbers reads a hint the same way: one written "./" after a byte order
# mark is on the number of a planned one
videos numbers "Show - 05.mkv" "Show - SP1.mkv"
printf '%sep: S00E01: ./Show - 05.mkv\n' "$BOM" >"$WORK/again/numbers/Show [anidb-1]/.plexmatch"
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" "$WORK/again/numbers" 2>&1)
check "again, numbers: a hint written ./ counts" "$(grep -F 'is on S00E01' <<<"$out")" \
	"$(same "$WORK/again/numbers/Show [anidb-1]" S00E01 '"Show - 05.mkv" (already hinted), "Show - SP1.mkv"')"

# two files whose names differ only in letter case (Linux): a hint is the one
# file's it names exactly, and the other gets its own
videos two "show - sp1.mkv" "Show - SP1.mkv"
if [[ -e "$WORK/again/two/Show [anidb-1]/show - sp1.mkv" ]] &&
	(($(ls -A "$WORK/again/two/Show [anidb-1]" | wc -l) == 2)); then
	printf 'ep: S00E01: show - sp1.mkv\n' >"$WORK/again/two/Show [anidb-1]/.plexmatch"
	again two $'ep: S00E01: show - sp1.mkv\nep: S00E01: Show - SP1.mkv\n'
	check "again, two: no warning about how the hint is written" "$(grep -c 'written differently' <<<"$out")" "0"
else
	echo "SKIP letter case check with two files: this filesystem ignores letter case"
fi

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
		*'[anidb-2]'*) key=anidb-2 ;;
		*'[anidb-3]'*) key=anidb-3 ;;
		*'[anidb-4]'*) key=anidb-4 ;;
		*'[anidb-5]'*) key=anidb-5 ;;
		*'[anidb-6]'*) key=anidb-6 ;;
		*'[tvdb3-901]'*) key=tvdb-901 ;;
		*'[tvdb3-902]'*) key=tvdb-902 ;;
		*'[tvdb-903]'*) key=tvdb-903 ;;
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
	# the same credits with EPISODE_LANGS=ja: Japanese titles
	"$FAKE"/anidb/library/metadata/anidb-2-s0/children)
		printf '{"MediaContainer":{"Metadata":[%s]}}' \
			'{"index":101,"title":"オープニング 1a"},{"index":102,"title":"オープニング 1b"},{"index":103,"title":"オープニング２"},{"index":151,"title":"エンディング"}'
		;;
	# titles the script can't read (another language, song names)
	"$FAKE"/anidb/library/metadata/anidb-3-s0/children)
		printf '{"MediaContainer":{"Metadata":[%s]}}' \
			'{"index":101,"title":"Sobakasu"},{"index":102,"title":"Kimi ni Todoke"},{"index":151,"title":"Tsuki"}'
		;;
	# openings readable, endings not
	"$FAKE"/anidb/library/metadata/anidb-4-s0/children)
		printf '{"MediaContainer":{"Metadata":[%s]}}' '{"index":101,"title":"Opening 1"},{"index":151,"title":"Tsuki"}'
		;;
	# an ending AniDB has no title for: Kizuna's placeholder, not a title to read
	"$FAKE"/anidb/library/metadata/anidb-5-s0/children)
		printf '{"MediaContainer":{"Metadata":[%s]}}' '{"index":101,"title":"Opening 1"},{"index":151,"title":"Episode 151"}'
		;;
	# Answers that are not Kizuna's: text where a number belongs. Bash runs the
	# $(...) in such a text when it uses the text as a number, which would
	# leave a file named marker in the folder the script runs in.
	"$FAKE"/tvdb/library/metadata/tvdb-901/grandchildren) # a season that is a text
		printf '{"MediaContainer":{"Metadata":[%s]}}' \
			'{"index":5,"parentIndex":"BASH_VERSINFO[$(echo>marker)0]"},{"index":6,"parentIndex":2}'
		;;
	"$FAKE"/tvdb/library/metadata/tvdb-902/grandchildren) # an episode number that is a text
		printf '{"MediaContainer":{"Metadata":[%s]}}' \
			'{"index":"5 BASH_VERSINFO[$(echo>marker)0]+","parentIndex":2},{"index":6,"parentIndex":2}'
		;;
	"$FAKE"/tvdb/library/metadata/tvdb-903/grandchildren) # season 2: a text and a fraction
		printf '{"MediaContainer":{"Metadata":[%s]}}' \
			'{"index":1,"parentIndex":1},{"index":"9x","parentIndex":2},{"index":7.5,"parentIndex":2}'
		;;
	# a number that is a text, and a title with a line break that reads as a
	# line of its own ("08<TAB>Opening 1")
	"$FAKE"/anidb/library/metadata/anidb-6-s0/children)
		printf '{"MediaContainer":{"Metadata":[%s]}}' \
			'{"index":"BASH_VERSINFO[$(echo>marker)0]","title":"Opening 3"},{"index":101,"title":"Song\n08\tOpening 1"},{"index":152,"title":"Ending 2"}'
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

	# tvdb rules: the movie's hint next to a trailer and a -featurette extra
	# (left out, no warning) too; an episode with an extra's ending is reported; an unnumbered video
	# that is no lone movie is reported as before; underscore names are read
	lib=$WORK/tvdb-movies
	mkdir -p "$lib/Film [tvdb3-200]" "$lib/Loose [tvdb3-200]"
	for f in "Film [tvdb3-200]/Film (2016)" "Film [tvdb3-200]/Film (2016) - PV" "Film [tvdb3-200]/Film (2016)-featurette" \
		"Loose [tvdb3-200]/Loose - 01" "Loose [tvdb3-200]/Loose - 033 - Love-scene" \
		"Loose [tvdb3-200]/Loose Recap" "Loose [tvdb3-200]/[Grp]_Loose_-_05_[1080p]"; do
		: >"$lib/$f.mkv"
	done
	out=$(bash "$SCRIPT" -m tvdb -u "$FAKE" "$lib" 2>&1)
	check "tvdb movies: exit" "$?" "0"
	check "tvdb movies: the hints, and no other" "$(grep '^  + ' <<<"$out")" \
		"  + ep: S01E01: Film (2016).mkv
  + ep: S02E05: [Grp]_Loose_-_05_[1080p].mkv"
	check "tvdb movies: the warnings" "$(grep '^WARNING' <<<"$out")" \
		"WARNING: $lib/Film [tvdb3-200]/Film (2016) - PV.mkv: special file - add its TVDB number by hand (ep: S00Exx: Film (2016) - PV.mkv)
WARNING: $lib/Loose [tvdb3-200]/Loose - 033 - Love-scene.mkv: Plex's scanner makes it an extra, not an episode or a movie: its name ends in \"-scene\". Rename it without that
WARNING: $lib/Loose [tvdb3-200]/Loose Recap.mkv: no episode number found"

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

	# tvdb rules: the same episode in two layouts or two versions is one
	# episode on purpose; a planned hint on the number of a file's own hint is
	# reported
	lib=$WORK/tvdb-same
	mkdir -p "$lib/Rel [tvdb-100]/Season 02" "$lib/Abs [tvdb3-200]"
	for f in "Rel [tvdb-100]/Rel - S02E02" "Rel [tvdb-100]/Season 02/Rel - 02" "Abs [tvdb3-200]/Abs - 05" \
		"Abs [tvdb3-200]/Abs - 05v2" "Abs [tvdb3-200]/Abs - 06" "Abs [tvdb3-200]/Abs Extra"; do
		: >"$lib/$f.mkv"
	done
	printf 'ep: S02E06: Abs Extra.mkv\n' >"$lib/Abs [tvdb3-200]/.plexmatch"
	out=$(bash "$SCRIPT" -m tvdb -u "$FAKE" "$lib" 2>&1)
	check "tvdb same number: exit" "$?" "0"
	check "tvdb same number: the hints" "$(grep '^  + ' <<<"$out")" \
		"  + ep: S02E05: Abs - 05.mkv
  + ep: S02E05: Abs - 05v2.mkv
  + ep: S02E06: Abs - 06.mkv
  + ep: S02E05: Rel - S02E02.mkv
  + ep: S02E05: Season 02/Rel - 02.mkv"
	check "tvdb same number: only the two different files are reported" "$(grep '^WARNING' <<<"$out")" \
		"$(same "$lib/Abs [tvdb3-200]" S02E06 '"Abs - 06.mkv", "Abs Extra.mkv" (already hinted)')"

	# a planned hint on the number Plex reads from another file's own name:
	# S02E05 left alone as already absolute, S02E06 in an absolute folder; the
	# same episode in two versions in a season folder is silent
	lib=$WORK/tvdb-same2
	mkdir -p "$lib/Rel2 [tvdb-100]/Season 02" "$lib/Abs2 [tvdb3-200]"
	for f in "Rel2 [tvdb-100]/Rel2 - S02E02" "Rel2 [tvdb-100]/Rel2 - S02E05" \
		"Rel2 [tvdb-100]/Season 02/Rel2 - 03 [720p]" "Rel2 [tvdb-100]/Season 02/Rel2 - 03 [1080p]" \
		"Abs2 [tvdb3-200]/Abs2 - 06" "Abs2 [tvdb3-200]/Abs2 - S02E06"; do
		: >"$lib/$f.mkv"
	done
	out=$(bash "$SCRIPT" -m tvdb -u "$FAKE" "$lib" 2>&1)
	check "tvdb same number 2: exit" "$?" "0"
	check "tvdb same number 2: the hints" "$(grep '^  + ' <<<"$out")" \
		"  + ep: S02E06: Abs2 - 06.mkv
  + ep: S02E05: Rel2 - S02E02.mkv
  + ep: S02E06: Season 02/Rel2 - 03 [1080p].mkv
  + ep: S02E06: Season 02/Rel2 - 03 [720p].mkv"
	check "tvdb same number 2: files Plex numbers by their names are reported" "$(grep '^WARNING' <<<"$out")" \
		"$(same "$lib/Abs2 [tvdb3-200]" S02E06 '"Abs2 - 06.mkv", "Abs2 - S02E06.mkv" (numbered by its name)')
$(same "$lib/Rel2 [tvdb-100]" S02E05 '"Rel2 - S02E02.mkv", "Rel2 - S02E05.mkv" (numbered by its name)')"

	# anidb mode with Kizuna: openings and endings are reported with AniDB's
	# own numbers, and none is hinted, however it is spelled
	lib=$WORK/credits
	mkdir -p "$lib/Cred [anidb-1]"
	for f in op1b op2 ed1 op1 op3 OP1a "Opening 2" NCOP3 "S01.E02 - op2"; do : >"$lib/Cred [anidb-1]/Cred - $f.mkv"; done
	out=$(bash "$SCRIPT" -u "$FAKE" "$lib" 2>&1)
	check "credits: exit" "$?" "0"
	# number_of FILE: what the file's WARNING line says about its number
	number_of() { grep -F "Cred - $1.mkv: " <<<"$out" | sed 's/.*, and add //'; }
	by_hand_end="to the show's .plexmatch by hand"
	check "credits: variant" "$(number_of op1b)" "\"ep: S00E102: <new name>\" $by_hand_end (S00E102 is Kizuna's number for it)"
	check "credits: shifted by a variant" "$(number_of op2)" "\"ep: S00E103: <new name>\" $by_hand_end (S00E103 is Kizuna's number for it)"
	check "credits: ending" "$(number_of ed1)" "\"ep: S00E151: <new name>\" $by_hand_end (S00E151 is Kizuna's number for it)"
	check "credits: no letter when AniDB has variants" "$(number_of op1)" \
		"\"ep: S00E101: <new name>\" $by_hand_end (S00E101 is Kizuna's number for it; AniDB has variants of this one: that is 1a's number)"
	check "credits: one AniDB doesn't list" "$(number_of op3)" \
		"an ep: hint for the new name, with its Kizuna number (S00E101...; OP3: not among this show's AniDB openings/endings), $by_hand_end"
	check "credits: an extra" "$(number_of OP1a)" "\"ep: S00E101: <new name>\" $by_hand_end (S00E101 is Kizuna's number for it)"
	check "credits: a file Plex leaves out" "$(number_of "Opening 2")" "\"ep: S00E103: <new name>\" $by_hand_end (S00E103 is Kizuna's number for it)"
	check "credits: an extra AniDB doesn't list" "$(number_of NCOP3)" \
		"an ep: hint for the new name, with its Kizuna number (S00E101...; OP3: not among this show's AniDB openings/endings), $by_hand_end"
	check_contains "credits: a lowercase name is told the name in capitals" "$out" "rename it with OP1b in place of that, e.g. to \"Cred - OP1b.mkv\""
	check "credits: a name Plex reads a season and episode in is left alone" "$(grep -c 'S01\.E02' <<<"$out")" "0"
	check "credits: no hint at all" "$(grep -c '^  + ' <<<"$out")" "0"
	check_contains "credits: summary" "$out" "0 .plexmatch files to write, 0 hints in all, 0 files already have a hint, 8 warnings"

	# Kizuna's credit titles in Japanese (EPISODE_LANGS=ja) are read like the
	# English ones; titles the script can't read say how to fix that, not
	# "run with -u"
	lib=$WORK/credits-langs
	mkdir -p "$lib/Ja [anidb-2]" "$lib/Unread [anidb-3]"
	for f in OP1b OP2 ED1; do : >"$lib/Ja [anidb-2]/Ja - $f.mkv"; done
	for f in OP1 ED1b; do : >"$lib/Unread [anidb-3]/Unread - $f.mkv"; done
	mkdir -p "$lib/Mixed [anidb-4]" "$lib/Placeholder [anidb-5]"
	: >"$lib/Mixed [anidb-4]/Mixed - ED2.mkv"
	: >"$lib/Placeholder [anidb-5]/Placeholder - ED2.mkv"
	out=$(bash "$SCRIPT" -u "$FAKE" "$lib" 2>&1)
	check "credit langs: exit" "$?" "0"
	number_of() { grep -F "$1.mkv: " <<<"$out" | sed 's/.*, and add //'; }
	check "credit langs: Japanese variant" "$(number_of "Ja - OP1b")" "\"ep: S00E102: <new name>\" $by_hand_end (S00E102 is Kizuna's number for it)"
	check "credit langs: Japanese, full-width digit" "$(number_of "Ja - OP2")" "\"ep: S00E103: <new name>\" $by_hand_end (S00E103 is Kizuna's number for it)"
	check "credit langs: Japanese ending" "$(number_of "Ja - ED1")" "\"ep: S00E151: <new name>\" $by_hand_end (S00E151 is Kizuna's number for it)"
	langs_fix="for this run, put en first in Kizuna's EPISODE_LANGS and restart it (then set it back)"
	unread="Kizuna's titles for this show's openings and endings can't be read; $langs_fix"
	check "credit langs: unreadable titles, no guess for a plain number" "$(number_of "Unread - OP1")" \
		"an ep: hint for the new name, with its Kizuna number (S00E101...; OP1: $unread), $by_hand_end"
	check "credit langs: unreadable titles, a variant" "$(number_of "Unread - ED1b")" \
		"an ep: hint for the new name, with its Kizuna number (S00E101...; ED1b: $unread), $by_hand_end"
	check "credit langs: some titles unreadable" "$(number_of "Mixed - ED2")" \
		"an ep: hint for the new name, with its Kizuna number (S00E101...; ED2: not among the ones whose Kizuna titles the script can read; $langs_fix), $by_hand_end"
	check "credit langs: a placeholder title isn't unreadable" "$(number_of "Placeholder - ED2")" \
		"an ep: hint for the new name, with its Kizuna number (S00E101...; ED2: not among this show's AniDB openings/endings), $by_hand_end"
	check "credit langs: never 'run with -u'" "$(grep -c 'run with -u' <<<"$out")" "0"

	# What answers at -u may not be Kizuna. Text where a number belongs is left
	# out: no command in it runs (no marker file), and its line plans no hint.
	# Each case runs in a folder of its own, where the marker would land.
	# marker_in DIR: "marker" when a command from an answer ran there
	marker_in() { if [[ -e $1/marker ]]; then echo marker; fi; }

	t=$(mktemp -d "$WORK/hostile-season.XXXXXX")
	mkdir -p "$t/lib/Abs [tvdb3-901]"
	: >"$t/lib/Abs [tvdb3-901]/Abs - 05.mkv"
	: >"$t/lib/Abs [tvdb3-901]/Abs - 06.mkv"
	out=$(cd "$t" && bash "$SCRIPT" -m tvdb -u "$FAKE" lib 2>&1)
	check "hostile season, dry run: exit" "$?" "0"
	check "hostile season, dry run: no command ran" "$(marker_in "$t")" ""
	check_contains "hostile season, dry run: its episode is unknown" "$out" "Abs - 05.mkv: absolute episode 5 not in tvdb-901"
	check_contains "hostile season, dry run: a good line still counts" "$out" "+ ep: S02E06: Abs - 06.mkv"
	check_contains "hostile season, dry run: summary" "$out" "1 .plexmatch files to write, 1 hints in all"
	out=$(cd "$t" && bash "$SCRIPT" -m tvdb -u "$FAKE" -a lib 2>&1)
	check "hostile season, apply: exit" "$?" "0"
	check "hostile season, apply: no command ran" "$(marker_in "$t")" ""
	check "hostile season, apply: only the good line's hint written" "$(bytes "$t/lib/Abs [tvdb3-901]/.plexmatch")" \
		'ep:S02E06:Abs-06.mkv\n'

	t=$(mktemp -d "$WORK/hostile-index.XXXXXX")
	mkdir -p "$t/lib/Abs [tvdb3-902]"
	: >"$t/lib/Abs [tvdb3-902]/Abs - 05.mkv"
	: >"$t/lib/Abs [tvdb3-902]/Abs - 06.mkv"
	out=$(cd "$t" && bash "$SCRIPT" -m tvdb -u "$FAKE" lib 2>&1)
	check "hostile episode number, dry run: exit" "$?" "0"
	check "hostile episode number, dry run: no command ran" "$(marker_in "$t")" ""
	check "hostile episode number, dry run: no hint from its line" "$(grep -c '+ ep: .*Abs - 05.mkv' <<<"$out")" "0"
	check_contains "hostile episode number, dry run: its episode is unknown" "$out" "Abs - 05.mkv: absolute episode 5 not in tvdb-902"
	check_contains "hostile episode number, dry run: a good line still counts" "$out" "+ ep: S02E06: Abs - 06.mkv"
	out=$(cd "$t" && bash "$SCRIPT" -m tvdb -u "$FAKE" -a lib 2>&1)
	check "hostile episode number, apply: exit" "$?" "0"
	check "hostile episode number, apply: no command ran" "$(marker_in "$t")" ""
	check "hostile episode number, apply: only the good line's hint written" "$(bytes "$t/lib/Abs [tvdb3-902]/.plexmatch")" \
		'ep:S02E06:Abs-06.mkv\n'

	# a plain [tvdb-N] folder takes the season's Nth episode number from the
	# answer: one that is no whole number is no episode
	t=$(mktemp -d "$WORK/hostile-nth.XXXXXX")
	mkdir -p "$t/lib/Rel [tvdb-903]"
	: >"$t/lib/Rel [tvdb-903]/Rel - S02E01.mkv"
	: >"$t/lib/Rel [tvdb-903]/Rel - S02E02.mkv"
	out=$(cd "$t" && bash "$SCRIPT" -m tvdb -u "$FAKE" lib 2>&1)
	check "episode numbers that are no whole numbers: exit" "$?" "0"
	check "episode numbers that are no whole numbers: no hint" "$(grep -c '^  + ' <<<"$out")" "0"
	check_contains "episode numbers that are no whole numbers: a text" "$out" \
		"Rel - S02E01.mkv: S02E01: TheTVDB's season 2 of tvdb-903 has no episode 1"
	check_contains "episode numbers that are no whole numbers: a fraction" "$out" \
		"Rel - S02E02.mkv: S02E02: TheTVDB's season 2 of tvdb-903 has no episode 2"
	check "episode numbers that are no whole numbers: nothing bash complains about" "$(grep -c 'invalid number' <<<"$out")" "0"

	t=$(mktemp -d "$WORK/hostile-credits.XXXXXX")
	mkdir -p "$t/lib/Inj [anidb-6]"
	for f in OP1 OP3 ED2; do : >"$t/lib/Inj [anidb-6]/Inj - $f.mkv"; done
	out=$(cd "$t" && bash "$SCRIPT" -u "$FAKE" lib 2>&1)
	check "hostile credits: exit" "$?" "0"
	check "hostile credits: no command ran" "$(marker_in "$t")" ""
	check "hostile credits: nothing bash complains about" "$(grep -cE 'value too great for base|invalid' <<<"$out")" "0"
	unlisted="not among the ones whose Kizuna titles the script can read; $langs_fix"
	check "hostile credits: a line made by a title's line break is no credit" "$(number_of "Inj - OP1")" \
		"an ep: hint for the new name, with its Kizuna number (S00E101...; OP1: $unlisted), $by_hand_end"
	check "hostile credits: a number that is a text is no credit" "$(number_of "Inj - OP3")" \
		"an ep: hint for the new name, with its Kizuna number (S00E101...; OP3: $unlisted), $by_hand_end"
	check "hostile credits: a good line still counts" "$(number_of "Inj - ED2")" \
		"\"ep: S00E152: <new name>\" $by_hand_end (S00E152 is Kizuna's number for it)"
else
	echo "SKIP fake-Kizuna checks (tvdb, auto and anidb with Kizuna): jq not found (install jq, or set JQ=gojq)"
fi

echo "$CHECKS checks, $FAILS failed"
((FAILS == 0))
