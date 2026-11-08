#!/usr/bin/env bash
# shellcheck disable=SC2154 # the variables checked are set by the sourced script
# Behaviour tests for kizuna-migrate.sh: the file name parsing, reading and
# writing .plexmatch files, and whole runs in anidb mode without Kizuna.
#
#   bash scripts/kizuna-migrate_test.sh
#
# Offline: Kizuna is never asked (-u points at a closed port). Each run works
# in a new folder under $TMPDIR (or /tmp), printed at the start and left there.

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=$HERE/kizuna-migrate.sh
# shellcheck source=kizuna-migrate.sh
source "$SCRIPT"
set +e # the script sets -e: a failing check must not end the run

NO_KIZUNA=http://127.0.0.1:9 # nothing listens there
WORK=$(mktemp -d "${TMPDIR:-/tmp}/kizuna-migrate-test.XXXXXX") || exit 1
echo "test folder: $WORK"
CHECKS=0 FAILS=0

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

# --- whole runs (anidb mode, Kizuna not reachable) -------------------------------

lib=$WORK/library
show="$lib/Show [anidb-1]"
mkdir -p "$show" "$lib/Movie" "$lib/.hidden"
for f in "Show - 01.mkv" "Show - 02.mkv" "Show - SP1.mkv" "Show - SP2.mkv" "Show - SP3.mkv" "Show - PV.mkv" \
	"Show - NCOP1.mkv" "notes.txt"; do
	: >"$show/$f"
done
: >"$lib/Movie/Movie (2010).mkv"
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
check_contains "dry run: summary" "$out" "2 .plexmatch files to write, 3 hints in all, 2 files already have a hint"
check "dry run writes nothing" "$(bytes "$show/.plexmatch")" "$original"
check "dry run: no new .plexmatch" "$(ls -A "$lib/Movie")" "Movie (2010).mkv"

out=$(bash "$SCRIPT" -u "$NO_KIZUNA" -a "$lib" 2>&1)
check "apply exits 0" "$?" "0"
check_contains "apply: says so" "$out" "wrote 2 .plexmatch files"
check "apply: hints appended in the file's line endings" "$(bytes "$show/.plexmatch")" \
	"${original}"'ep:S00E201:Show-PV.mkv\r\nep:S00E01:Show-SP1.mkv\r\n'
check "apply: new .plexmatch for the movie" "$(bytes "$lib/Movie/.plexmatch")" 'ep:S01E01:Movie(2010).mkv\n'
check "apply: hidden folder untouched" "$(ls -A "$lib/.hidden")" ""

before=$(bytes "$show/.plexmatch")$(bytes "$lib/Movie/.plexmatch")
out=$(bash "$SCRIPT" -u "$NO_KIZUNA" -a "$lib" 2>&1)
check_contains "second run: nothing to write" "$out" "0 .plexmatch files to write"
check "second run changes nothing" "$(bytes "$show/.plexmatch")$(bytes "$lib/Movie/.plexmatch")" "$before"

out=$(bash "$SCRIPT" -m nope "$lib" 2>&1)
check "unknown mode exits 1" "$?" "1"
check_contains "unknown mode says so" "$out" "unknown mode nope"

echo "$CHECKS checks, $FAILS failed"
((FAILS == 0))
