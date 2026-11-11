#!/usr/bin/env bash
# kizuna-migrate.sh - prepare an anime library that uses absolute file names for
# Plex's own "Plex TV Series" scanner and Kizuna, without renaming anything: it
# writes .plexmatch episode hints. See MIGRATION.md.
#
#   kizuna-migrate.sh [-m anidb|tvdb|auto] [-u URL] LIBRARY     show the planned hints (dry run)
#   kizuna-migrate.sh [-m anidb|tvdb|auto] [-u URL] -a LIBRARY  write them
#
# Each show folder gets one .plexmatch with an "ep:" line for every file the
# scanner would number differently from Kizuna:
#   anidb rules  SP3 / PV / OVA                        -> ep: S00E03: Show - SP3.mkv etc.
#                openings and endings never get a hint, however they are
#                spelled. Plex's scanner takes them by name before it reads a
#                hint (PLEX_CREDIT_RE): a file named like NCOP1 / OP1b / ED2 /
#                Creditless Opening becomes an extra, one named with the whole
#                word (Opening 1, Ending) is left out of the library, and one
#                in small letters (op1) is no extra. Each is only reported,
#                with AniDB's own number (S00E101..., from Kizuna when it's
#                reachable) for whoever renames the file and hints it by hand
#                a folder's only video without a number -> ep: S01E01 (a movie)
#   tvdb rules   absolute "Show - 033.mkv"             -> ep: S02E33: Show - 033.mkv
#                plain HAMA [tvdb-N] folders (seasons counted from 1 each):
#                "Show - S02E01.mkv" / "Season 02/Show - 01.mkv" -> ep: S02E26: ...
#                (plus the movie rule; special files are only reported)
#   auto mode    per show, the rules Kizuna's /auto provider picks for it: the
#                first id tag decides when it's [anidbN-N] (anidb) or [tvdbN-N]
#                (tvdb); other tags and untagged folders follow Kizuna's
#                AUTO_DEFAULT and its mapping, so Kizuna is asked for those
#
# The hints are added as plain ep: lines at the end of the show's .plexmatch,
# without any marker, so they aren't tracked: the next run reads them like your
# own lines, and changing or removing one is done by hand. The rest of an
# existing .plexmatch is kept and read the way Plex reads it: title and year
# hints go into the match requests sent to Kizuna, season hints set a folder's
# season, a file that already has an ep: hint keeps it (reported when it
# differs), and a show with pattern: hints is skipped, because the script can't
# evaluate them. Id hints (tvdbid:, guid:...) aren't supported: they're
# reported and ignored, the show is matched by its folder tag or title.
#
# A show folder without an id tag but with a HAMA .id file (anidb.id, tvdb.id,
# tvdb3.id, tmdb.id, imdb.id) is planned as if its name had that tag, and a
# warning gives the folder name to rename it to: Plex never reads .id files.
#
# tvdb mode, and tvdb-rule shows in auto mode, ask a running Kizuna (-u,
# default $KIZUNA_URL or http://localhost:32600) which season holds each
# absolute episode, through the same match and metadata calls Plex uses. It
# needs curl and jq. Without Kizuna, auto mode plans only [anidb-N] folders.
# Requires bash 4.4+ (macOS: brew install bash).

if ((BASH_VERSINFO[0] * 100 + BASH_VERSINFO[1] < 404)); then
	echo "error: this script needs bash 4.4 or newer; this is bash $BASH_VERSION (macOS: brew install bash)" >&2
	exit 1
fi
set -euo pipefail
shopt -s nullglob

PLEXMATCH=.plexmatch
VIDEO_EXT=" mkv mp4 avi m4v ts m2ts wmv mov webm ogm flv mpg "

usage() {
	sed -n '6,7p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
}

die() { echo "error: $*" >&2; exit 1; }

# trim removes leading and trailing whitespace.
trim() {
	local s=$1
	s=${s#"${s%%[![:space:]]*}"}
	s=${s%"${s##*[![:space:]]}"}
	printf '%s' "$s"
}

# --- name parsing ------------------------------------------------------------

# strip_tags removes [..], (..) and {..} groups and trims spaces.
strip_tags() {
	local s=$1 re='\[[^]]*\]|\([^)]*\)|\{[^}]*\}'
	while [[ $s =~ $re ]]; do s=${s/"${BASH_REMATCH[0]}"/}; done
	trim "$s"
}

# Regexes are matched with nocasematch on (case-insensitive).
SXE='(^|[^[:alnum:]_])s[0-9]{1,2}[[:space:]]*e[0-9]{1,4}([^[:alnum:]_]|$)'
SXE_NUM='(^|[^[:alnum:]_])s([0-9]{1,2})[[:space:]]*e([0-9]{1,4})([^[:alnum:]_]|$)'
# a plain HAMA tvdb tag ([tvdb-81797], {tvdb-81797}): no digit after "tvdb", unlike [tvdb3-...]
PLAIN_TVDB_TAG='[[{]tvdb-[0-9]+[]}]'
# an id tag as Kizuna reads it (tagRe in internal/provider/match.go); the first one in a name counts
ID_TAG='[[{](anidb|tvdb|tmdb|imdb|mal|anilist|kitsu|ann)[0-9]?-(tt[0-9]+|[0-9]+)[]}]'
ABS_DASH=' - ([0-9]{1,4})(v[0-9])?([^[:alnum:]_]|$)'
ABS_EP='(^|[^[:alnum:]_])(e|ep|episode)[[:space:]]*([0-9]{1,4})(v[0-9])?([^[:alnum:]_]|$)'
ABS_END='(^|[^[:alnum:]_])([0-9]{1,4})(v[0-9])?[[:space:]]*$'
SEASON_DIR='^((season|series|saison|staffel|temporada)[ ._-]*[0-9]{1,4}|s[0-9]{1,4}|specials?)$'
GROUP_DIR='^[[:space:]]*[[(][0-9]+[])]'
# openings / endings: NCOP1, OP1b, ED, Ending 2a (kind, number, variant letter)
CREDIT_RE='^(nc)?(op|opening|ed|ending)[[:space:]]*0*([0-9]{1,2})?[[:space:]]*([a-z])?(v[0-9])?$'
# Plex's own opening/ending check (Plex Media Server 1.43, from its binary):
#   (?:\b|_|\.)(?<title>(?:NC|(?:Creditless|Clean)[ ._])?(?<type>OP|ED|Opening|Ending|OPED)
#   (?:[ ._]?(?<index>\d{1,2})[A-Za-z]?)?(?:[ ._][A-Za-z])?)(?:v\d)?(?:\b|_|\.)
# Plex takes a file whose name matches before .plexmatch hints are read: see
# plex_credit for what it does with it. Same pattern in bash ERE (\b as "not
# a letter or digit"), matched with case, as Plex spells the words out in
# capitals. Group 2 is Plex's "title", 4 Creditless or Clean, 5 the word.
PLEX_CREDIT_RE='(^|[^[:alnum:]])((NC|(Creditless|Clean)[ ._])?(OP|ED|Opening|Ending|OPED)([ ._]?[0-9]{1,2}[A-Za-z]?)?([ ._][A-Za-z])?)(v[0-9])?([^[:alnum:]]|$)'
# Plex's season-and-episode check, from the same binary, in any case:
#   (?<show>.*?)[s](?<season>[0-9]+)[._ ]*[e](?<ep>[0-9]+)...
#   (?<show>.*?)[s](?<season>[0-9]{2})[-._ ]+(?<ep>[0-9]+)
# A name it matches is an episode, and Plex doesn't look for an opening or
# ending in it. Wider than SXE: no word boundaries, "S01.E05", "s01 05".
PLEX_SXE_RE='s[0-9]+[._ ]*e[0-9]+|s[0-9]{2}[-._ ]+[0-9]+'
# a spelled-out word and its number, as plex_credit prints them: "NCOpening 1"
SHORT_CREDIT_RE='^(NC)?(OP|ED)[ ._]([0-9].*)$'
# AniDB credit titles: "Opening 1a", "Opening 1 (b)", "Ending", "Ending 2: Song",
# and the Japanese ones Kizuna sends when EPISODE_LANGS puts ja first
# ("オープニング2", "エンディング")
CREDIT_TITLE_RE='^(opening|op|ending|ed|オープニング|エンディング)([[:space:]]*0*([0-9]+))?[[:space:]]*(\(([a-z])\)|([a-z]))?([^[:alnum:]]|$)'
# full-width digits and space, turned into ASCII before CREDIT_TITLE_RE: in
# the C locale "２" isn't a digit, so "オープニング２" would read as opening 1
FULLWIDTH=(０ １ ２ ３ ４ ５ ６ ７ ８ ９)
# other special patterns: "regex|base|number group" (no group = number 1)
SPECIALS=(
	'^(pv|cm|trailer|teaser|promo|preview)[[:space:]]*0*([0-9]{1,2})?(v[0-9])?$|200|2'
	'^(sp|special|ova|oad|s)[[:space:]]*0*([0-9]{1,3})(v[0-9])?$|0|2'
	'^(ova|oad|special)$|0|0'
)

# absolute_number prints the episode number in a file name, or 0.
absolute_number() {
	local core
	core=$(strip_tags "$1")
	shopt -s nocasematch
	if [[ $core =~ $ABS_DASH ]]; then echo $((10#${BASH_REMATCH[1]}))
	elif [[ $core =~ $ABS_EP ]]; then echo $((10#${BASH_REMATCH[3]}))
	elif [[ $core =~ $ABS_END ]]; then echo $((10#${BASH_REMATCH[2]}))
	else echo 0; fi
	shopt -u nocasematch
}

# name_tails prints the parts of a file name that may name a special: the
# text after the last " - ", the last two words, the last word. Nothing for
# names that already have an episode number (S01E05, "Show - 05").
name_tails() {
	local core words
	core=$(strip_tags "$1")
	shopt -s nocasematch
	if [[ $core =~ $SXE || $core =~ $ABS_DASH ]]; then
		shopt -u nocasematch
		return
	fi
	shopt -u nocasematch
	if [[ $core == *" - "* ]]; then printf '%s\n' "${core##* - }"; fi
	read -ra words <<<"$core"
	if ((${#words[@]} > 1)); then printf '%s\n' "${words[-2]} ${words[-1]}"; fi
	if ((${#words[@]} > 0)); then printf '%s\n' "${words[-1]}"; fi
}

# credit_of prints "op|ed NUMBER LETTER TEXT" for an opening or ending file
# name, TEXT being the part of the name that says so (Show - NCOP1 ->
# "op 1 - NCOP1", OP1b -> "op 1 b OP1b", ED -> "ed 1 - ED"), or nothing.
credit_of() {
	local t kind num letter
	while IFS= read -r t; do
		t=$(strip_tags "$t")
		shopt -s nocasematch
		if [[ $t =~ $CREDIT_RE ]]; then
			kind=${BASH_REMATCH[2],,} num=${BASH_REMATCH[3]:-1} letter=${BASH_REMATCH[4],,}
			shopt -u nocasematch
			if [[ $kind == e* ]]; then kind="ed"; else kind="op"; fi
			echo "$kind $((10#$num)) ${letter:--} $t"
			return
		fi
		shopt -u nocasematch
	done < <(name_tails "$1")
}

# credit_title prints "NUMBER LETTER" for an AniDB credit title
# ("Opening 1a" -> "1 a", "Ending" -> "1 -", "オープニング２" -> "2 -"), or
# nothing.
credit_title() {
	local num letter t=$1 d
	for d in "${!FULLWIDTH[@]}"; do t=${t//"${FULLWIDTH[d]}"/$d}; done
	t=${t//　/ }
	shopt -s nocasematch
	if [[ $t =~ $CREDIT_TITLE_RE ]]; then
		num=${BASH_REMATCH[3]:-1} letter=${BASH_REMATCH[5]:-${BASH_REMATCH[6]}}
		shopt -u nocasematch
		letter=${letter,,}
		echo "$((10#$num)) ${letter:--}"
		return
	fi
	shopt -u nocasematch
}

# plex_numbered says whether Plex reads a season and episode in file name $1
# (without its extension).
plex_numbered() {
	shopt -s nocasematch
	if [[ $1 =~ $PLEX_SXE_RE ]]; then
		shopt -u nocasematch
		return 0
	fi
	shopt -u nocasematch
	return 1
}

# plex_credit prints what Plex's scanner does with file name $1 (without its
# extension) as an opening or ending, and the part of the name that does it:
# "extra NCOP1" (an opening/ending extra) or "dropped Opening 1" (left out of
# the library: the whole word Opening or Ending without Creditless or Clean
# in front). Fails for a name Plex reads like any other. The Go port of the
# same rules is CreditOf in internal/plex/title.go: keep the two in step.
plex_credit() {
	local kind=extra word
	if plex_numbered "$1"; then return 1; fi
	[[ $1 =~ $PLEX_CREDIT_RE ]] || return 1
	word=${BASH_REMATCH[5]}
	if [[ ($word == Opening || $word == Ending) && -z ${BASH_REMATCH[4]} ]]; then kind=dropped; fi
	printf '%s %s\n' "$kind" "${BASH_REMATCH[2]}"
}

# short_credit prints the name Plex makes an extra of for a spelled-out one
# ("Opening 1" -> "OP1", "NCOpening" -> "NCOP", "Ending a" -> "ED a").
short_credit() {
	local s=${1/Opening/OP}
	s=${s/Ending/ED}
	if [[ $s =~ $SHORT_CREDIT_RE ]]; then s=${BASH_REMATCH[1]}${BASH_REMATCH[2]}${BASH_REMATCH[3]}; fi
	printf '%s' "$s"
}

# capital_credit prints the name Plex makes an extra of for an opening or
# ending spelled another way ("op1" -> "OP1", "ncop2" -> "NCOP2",
# "ending 2A" -> "ED2a"), or nothing.
capital_credit() {
	local nc word num letter
	shopt -s nocasematch
	if [[ $1 =~ $CREDIT_RE ]]; then
		nc=${BASH_REMATCH[1]^^} word=${BASH_REMATCH[2],,} num=${BASH_REMATCH[3]} letter=${BASH_REMATCH[4],,}
		shopt -u nocasematch
		if [[ $word == e* ]]; then word=ED; else word=OP; fi
		printf '%s' "$nc$word${num:+$((10#$num))}$letter"
		return
	fi
	shopt -u nocasematch
}

# credit_note explains an opening or ending file, which never gets a hint:
# $1 the file's name, $2 what Plex does with it (extra or dropped, as
# plex_credit says, or plain: it doesn't take the name), $3 the part of the
# name that says so, $4 the special number it would have, if known, with $5
# a remark on that number, else $5 how to find the number. The script
# renames nothing, so making the file a special is left to the user: the
# note only says how.
credit_note() {
	local note short
	case $2 in
	extra)
		note="Plex's scanner makes files with \"$3\" in the name opening/ending extras and ignores .plexmatch hints for them, so it stays an extra (it still plays from the show page) and gets no hint"
		;;
	dropped)
		short=$(short_credit "$3")
		note="Plex's scanner leaves files with the whole word \"$3\" in the name out of the library, as neither an extra nor an episode, so it gets no hint. To have it as an extra, rename it with $short in place of that, e.g. to \"${1/"$3"/$short}\""
		;;
	*)
		short=$(capital_credit "$3")
		note="Plex's scanner only takes these words in capitals, so \"$3\" doesn't make it an opening/ending extra, and the script writes no hint for openings and endings. To have it as an extra (Kizuna can then give it a readable title), rename it with $short in place of that, e.g. to \"${1/"$3"/$short}\""
		;;
	esac
	note+=". If you want it as a special, that is yours to do: rename it to a name without \"$3\" or another such word, and add "
	if [[ -n ${4:-} ]]; then
		note+="\"ep: $4: <new name>\" to the show's .plexmatch by hand ($4 is Kizuna's number for it${5:+; $5})"
	else
		note+="an ep: hint for the new name, with ${5:-its number}, to the show's .plexmatch by hand"
	fi
	printf '%s' "$note"
}

# special_number maps other special file names to season-0 numbers
# (PV -> 201, SP03 -> 3); 0 = not one. Openings and endings: credit_of.
special_number() {
	local t entry re base group n
	shopt -s nocasematch
	while IFS= read -r t; do
		t=$(strip_tags "$t")
		for entry in "${SPECIALS[@]}"; do
			re=${entry%|*|*} base=${entry%|*} base=${base##*|} group=${entry##*|}
			if [[ $t =~ $re ]]; then
				n=1
				if ((group > 0)) && [[ -n ${BASH_REMATCH[group]} ]]; then n=$((10#${BASH_REMATCH[group]})); fi
				if ((base + n > 0)); then
					shopt -u nocasematch
					echo $((base + n))
					return
				fi
			fi
		done
	done < <(name_tails "$1")
	shopt -u nocasematch
	echo 0
}

is_video() {
	local ext=${1##*.}
	[[ $1 == *.* && $VIDEO_EXT == *" ${ext,,} "* ]]
}

# MAX_DEPTH: how far below a show folder its videos and .plexmatch files are
# looked for. A real show is at most a few levels deep (Season 01/Disc 1/...).
# It bounds a symlink loop where find doesn't catch one: GNU and BSD find
# report a loop and skip it, but busybox find may follow it until the path
# is too long, planning the same files again under every longer path.
MAX_DEPTH=6

# list_videos prints the videos under a show folder, sorted, skipping hidden
# and "extras" folders. Symlinked folders and files are followed, as Plex does
# (find -L reports a symlink loop and skips it; MAX_DEPTH bounds one it doesn't).
list_videos() {
	find -L "$1" -mindepth 1 -maxdepth "$MAX_DEPTH" \( -type d \( -name '.*' -o -iname extras \) -prune \) -o -type f -print |
		while IFS= read -r f; do if is_video "$f"; then printf '%s\n' "$f"; fi; done | LC_ALL=C sort
}

# --- existing .plexmatch files -------------------------------------------------

SHOW_TITLE="" SHOW_YEAR=0 # what Plex sends to Kizuna for the current show
ID_HINT=""                # an id hint (tvdbid:, guid:...) in the show's .plexmatch: not supported
ROOT_SEASON=""                        # a season: hint in the show folder's .plexmatch
HAS_PATTERN=0                         # a pattern: hint anywhere in the show
declare -A USER_EP=()                 # show-relative file -> episode from an ep: hint already there
declare -A DIR_SEASON=()              # show-relative folder -> season from its own season: hint

# read_plexmatches reads every .plexmatch in the show folder $1 and below,
# in the folders list_videos looks in. Hints in subfolders only count for
# their own folder, like in Plex. Every ep: line counts as the user's, also
# those an earlier run wrote.
read_plexmatches() {
	local dir=$1 pm reldir line name value file
	SHOW_TITLE=$(basename "$dir") SHOW_YEAR=0 ID_HINT="" ROOT_SEASON="" HAS_PATTERN=0
	USER_EP=() DIR_SEASON=()
	while IFS= read -r pm; do
		reldir=${pm%/"$PLEXMATCH"} reldir=${reldir#"$dir"} reldir=${reldir#/} # "" = the show folder
		while IFS= read -r line || [[ -n $line ]]; do
			line=$(trim "${line%$'\r'}")
			if [[ -z $line || $line == \#* || $line != *:* ]]; then continue; fi
			name=$(trim "${line%%:*}") value=$(trim "${line#*:}")
			case ${name,,} in
			title | show) if [[ -z $reldir && -n $value ]]; then SHOW_TITLE=$value; fi ;;
			year) if [[ -z $reldir && $value =~ ^[0-9]{4}$ ]]; then SHOW_YEAR=$((10#$value)); fi ;;
			tvdbid | tmdbid | imdbid | guid) # not supported: whether Plex passes them to Kizuna is unknown
				if [[ -z $reldir ]]; then ID_HINT="$name: $value"; fi
				;;
			season)
				if [[ $value =~ ^[0-9]+$ ]]; then
					if [[ -z $reldir ]]; then ROOT_SEASON=$((10#$value)); else DIR_SEASON[$reldir]=$((10#$value)); fi
				fi
				;;
			pattern | pt) HAS_PATTERN=1 ;;
			episode | ep)
				# "NUMBER: file", the number directly followed by the colon
				if [[ $value =~ ^([A-Za-z0-9-]+):(.*)$ ]]; then
					file=$(trim "${BASH_REMATCH[2]}")
					file=${file//\\//}
					if [[ -z $file ]]; then continue; fi # "ep: 5:" names no file
					if [[ -n $reldir ]]; then file="$reldir/$file"; fi
					USER_EP[$file]=${BASH_REMATCH[1]}
				fi
				;;
			esac
		done <"$pm"
	done < <(find -L "$dir" -mindepth 1 -maxdepth "$MAX_DEPTH" \( -type d \( -name '.*' -o -iname extras \) -prune \) -o -type f -name "$PLEXMATCH" -print)
}

# --- HAMA .id files ------------------------------------------------------------

# HAMA / ASS id files in a show folder: anidb.id, tvdb.id, tvdb3.id, tmdb.id, imdb.id...
ID_FILE_RE='^(anidb|tvdb|tmdb|imdb)([0-9]?)\.id$'
ID_FILE_TAG="" # the folder tag the show's .id file stands for ([anidb-23]), when its name has none

# read_id_file sets ID_FILE_TAG from the HAMA .id file in the show folder $1,
# for a folder whose name has no id tag. Plex never reads these files, so the
# tag only counts once it's in the folder name: the warning says which name.
# The first of anidb, tvdb, tmdb, imdb wins.
read_id_file() {
	local dir=$1 f base src kind line id used="" others=""
	local -A by_kind=()
	ID_FILE_TAG=""
	shopt -s nocasematch
	if [[ $(basename "$dir") =~ $ID_TAG ]]; then
		shopt -u nocasematch
		return 0 # a tag in the folder name wins, as in HAMA
	fi
	for f in "$dir"/*; do
		base=$(basename "$f")
		if [[ -f $f && $base =~ $ID_FILE_RE && -z ${by_kind[${BASH_REMATCH[1],,}]:-} ]]; then
			by_kind[${BASH_REMATCH[1],,}]=$f
		fi
	done
	shopt -u nocasematch
	for kind in anidb tvdb tmdb imdb; do
		f=${by_kind[$kind]:-}
		[[ -n $f ]] || continue
		if [[ -n $used ]]; then others+=" $(basename "$f")"; continue; fi
		line=""
		IFS= read -r line <"$f" || true
		line=$(trim "${line%$'\r'}")
		base=$(basename "$f")
		shopt -s nocasematch
		[[ $base =~ $ID_FILE_RE ]]
		src=${BASH_REMATCH[1],,}${BASH_REMATCH[2]}
		if [[ $kind == imdb && $line =~ ^(tt)?([0-9]+) ]]; then id=tt${BASH_REMATCH[2]}
		elif [[ $kind != imdb && $line =~ ^([0-9]+) ]] && ((10#${BASH_REMATCH[1]} > 0)); then id=$((10#${BASH_REMATCH[1]}))
		else
			shopt -u nocasematch
			warn "$f: no id in it; ignored"
			continue
		fi
		shopt -u nocasematch
		ID_FILE_TAG="[$src-$id]" used=$base
	done
	if [[ -n $used ]]; then
		warn "$dir: $used says $ID_FILE_TAG, but Plex never reads .id files. Rename the folder to \"$(basename "$dir") $ID_FILE_TAG\" before switching the library (the hints above assume that tag)"
		if [[ -n $others ]]; then warn "$dir: also has$others; used $used"; fi
	fi
}

# unsupported_order prints the first id tag in $1 when it names one of HAMA's
# numbering orders Kizuna doesn't support ([anidb2-N], [tvdb2-N], [tvdb4-N]...).
# The tag still finds the show, but its episodes follow Kizuna's numbering.
unsupported_order() {
	local tag=""
	shopt -s nocasematch
	if [[ $1 =~ $ID_TAG ]]; then tag=${BASH_REMATCH[0]}; fi
	if [[ $tag =~ ^.(anidb[0-9]|tvdb[0-24-9])- ]]; then echo "$tag"; fi
	shopt -u nocasematch
}

# dir_season prints the season Plex's scanner gives the show-relative folder
# $1 ("" = the show folder): its season: hint, its name (Season 02, Specials),
# or nothing for other folders.
dir_season() {
	local rd=$1 base
	if [[ -z $rd ]]; then echo "${ROOT_SEASON:-1}"; return; fi
	if [[ -n ${DIR_SEASON[$rd]:-} ]]; then echo "${DIR_SEASON[$rd]}"; return; fi
	base=${rd##*/}
	shopt -s nocasematch
	if [[ $base =~ $SEASON_DIR ]]; then
		if [[ $base =~ ([0-9]+) ]]; then echo $((10#${BASH_REMATCH[1]})); else echo 0; fi
	fi
	shopt -u nocasematch
}

# norm_ep prints "SEASON EPISODE" for a .plexmatch episode number ($2 = the
# folder's season, for a bare number), the start of a range: SP04 -> "0 4",
# S03E04 -> "3 4", E02 / 02 -> "$2 2".
norm_ep() {
	local v=${1%%-*}
	shopt -s nocasematch
	if [[ $v =~ ^sp([0-9]+)$ ]]; then echo "0 $((10#${BASH_REMATCH[1]}))"
	elif [[ $v =~ ^s([0-9]+)e([0-9]+)$ ]]; then echo "$((10#${BASH_REMATCH[1]})) $((10#${BASH_REMATCH[2]}))"
	elif [[ $v =~ ^e?([0-9]+)$ ]]; then echo "$2 $((10#${BASH_REMATCH[1]}))"
	fi
	shopt -u nocasematch
}

# --- provider lookup -----------------------------------------------------------

# match_body prints a match request for the current show the way Plex sends it:
# $1 = one of its videos, $2 = 1 for a manual (title list only) match.
match_body() {
	local rel
	rel="$(basename -- "$LIBRARY")/${1#"$LIBRARY"/}" # path the way Plex sends it
	# a HAMA .id file's tag goes into the title, as if the folder were already renamed
	jq -n --arg t "$SHOW_TITLE${ID_FILE_TAG:+ $ID_FILE_TAG}" --arg f "$rel" --argjson y "$SHOW_YEAR" --argjson m "$2" \
		'{type: 2, title: $t, filename: $f} + (if $y > 0 then {year: $y} else {} end)
		 + (if $m == 1 then {manual: 1} else {} end)'
}

declare -A SEASON_OF # absolute episode -> season, for the current show
declare -A ABS_OF    # "season N" -> absolute number of the season's Nth episode, for the current show
SHOW_KEY=""          # its Kizuna rating key

# load_seasons asks Kizuna for the show of video $1 and sets SHOW_KEY,
# SEASON_OF and ABS_OF. Fails when Kizuna has no match.
load_seasons() {
	local video=$1 key lines abs season
	local -A nth=()
	SEASON_OF=() ABS_OF=() SHOW_KEY=""
	key=$(curl -fsS --max-time 60 -H 'Content-Type: application/json' -d "$(match_body "$video" 0)" \
		"$URL/$MODE/library/metadata/matches" | jq -r '.MediaContainer.Metadata[0].ratingKey // empty' | tr -d '\r') ||
		return 1
	[[ -n $key ]] || return 1
	lines=$(curl -fsS --max-time 60 -H 'X-Plex-Container-Size: 100000' \
		"$URL/$MODE/library/metadata/$key/grandchildren" |
		jq -r '[.MediaContainer.Metadata[]? | select(.parentIndex > 0)] | sort_by(.parentIndex)[] | "\(.index) \(.parentIndex)"' | tr -d '\r') ||
		return 1
	while read -r abs season; do
		[[ -n $abs ]] || continue
		if [[ -z ${SEASON_OF[$abs]:-} ]]; then SEASON_OF[$abs]=$season; fi
		nth[$season]=$((${nth[$season]:-0} + 1)) # Kizuna lists each season's episodes in order
		ABS_OF["$season ${nth[$season]}"]=$abs
	done <<<"$lines"
	SHOW_KEY=$key
}

# tag_rules prints "anidb" or "tvdb" when a name ($1) settles the /auto rules
# by itself: its first id tag is an AniDB or TVDB one. Like Kizuna, a tag
# beats AUTO_DEFAULT. Nothing for other tags and no tag.
tag_rules() {
	local src id
	shopt -s nocasematch
	if [[ $1 =~ $ID_TAG ]]; then
		src=${BASH_REMATCH[1],,} id=${BASH_REMATCH[2]}
		shopt -u nocasematch
		if [[ ($src == anidb || $src == tvdb) && $id =~ ^[0-9]+$ ]] && ((10#$id > 0)); then echo "$src"; fi
		return 0
	fi
	shopt -u nocasematch
}

# show_rules prints how Kizuna's /auto provider treats the show of video $1:
# "anidb" or "tvdb", the prefix of its match's rating key. A manual match
# answers from the title list, without any API call.
show_rules() {
	local key
	key=$(curl -fsS --max-time 60 -H 'Content-Type: application/json' -d "$(match_body "$1" 1)" \
		"$URL/auto/library/metadata/matches" | jq -r '.MediaContainer.Metadata[0].ratingKey // empty' | tr -d '\r') ||
		return 1
	[[ $key == anidb-* || $key == tvdb-* ]] || return 1
	echo "${key%%-*}"
}

# --- openings and endings (anidb rules) ---------------------------------------

declare -A CREDIT_AT # "op 1 a" -> Plex episode (101...), for the show in CREDITS_FOR
CREDITS_FOR=""
CREDITS_UNREAD=0 # Kizuna's credit titles for that show that credit_title can't read
CREDIT_N=0 CREDIT_NOTE=""
# what fixes unreadable credit titles: Kizuna sends them in EPISODE_LANGS
LANGS_FIX="Kizuna's titles for this show's openings and endings can't be read: put en first in its EPISODE_LANGS"
SHOW_VARIANTS="" # " op ed": kinds with variant files (OP1b...) in the current show folder

# fetch_credits prints "INDEX<TAB>TITLE" for the openings and endings Kizuna
# has for the show of video $1: its season 0 from 101 on.
fetch_credits() {
	local key
	key=$(curl -fsS --max-time 60 -H 'Content-Type: application/json' -d "$(match_body "$1" 1)" \
		"$URL/$MODE/library/metadata/matches" | jq -r '.MediaContainer.Metadata[0].ratingKey // empty' | tr -d '\r') ||
		return 0
	[[ $key == anidb-* ]] || return 0
	curl -fsS --max-time 120 -H 'X-Plex-Container-Size: 100000' "$URL/$MODE/library/metadata/$key-s0/children" |
		jq -r '.MediaContainer.Metadata[]? | select(.index > 100 and .index < 200) | "\(.index)\t\(.title)"' |
		tr -d '\r' || return 0
}

# load_credits fills CREDIT_AT from Kizuna's credit titles for the show in $1
# ($2 = one video in it). Empty when Kizuna isn't used or has no titles
# (AniDB unavailable). CREDITS_UNREAD counts the titles it couldn't read, such
# as a song name or a language without "Opening" in it.
load_credits() {
	local index title kind num letter
	CREDIT_AT=() CREDITS_FOR=$1 CREDITS_UNREAD=0
	((KIZUNA)) || return 0
	while IFS=$'\t' read -r index title; do
		[[ $index =~ ^[0-9]+$ ]] || continue
		kind=op
		if ((index > 150)); then kind="ed"; fi
		if read -r num letter < <(credit_title "$title"); then
			if [[ -z ${CREDIT_AT["$kind $num $letter"]:-} ]]; then CREDIT_AT["$kind $num $letter"]=$index; fi
		else
			CREDITS_UNREAD=$((CREDITS_UNREAD + 1))
		fi
	done < <(fetch_credits "$2")
}

# credit_index sets CREDIT_N to the Plex number of an opening or ending
# ($1 op|ed, $2 number, $3 letter or -) and CREDIT_NOTE to anything worth
# saying about it. With Kizuna's titles it uses AniDB's own numbering; without them
# 100/150 + number, which is only safe without a variant letter.
credit_index() {
	local kind=$1 num=$2 letter=$3 l
	CREDIT_N=0 CREDIT_NOTE=""
	if ((${#CREDIT_AT[@]} > 0)); then
		if [[ -n ${CREDIT_AT["$kind $num $letter"]:-} ]]; then
			CREDIT_N=${CREDIT_AT["$kind $num $letter"]}
		elif [[ $letter != - && -n ${CREDIT_AT["$kind $num -"]:-} ]]; then
			CREDIT_N=${CREDIT_AT["$kind $num -"]} # AniDB lists no variants of it
		elif [[ $letter == - ]]; then
			for l in {a..z}; do
				if [[ -n ${CREDIT_AT["$kind $num $l"]:-} ]]; then
					CREDIT_N=${CREDIT_AT["$kind $num $l"]}
					CREDIT_NOTE="AniDB has variants of this one: that is $num$l's number"
					break
				fi
			done
		fi
		if ((CREDIT_N == 0)); then
			CREDIT_NOTE="not among this show's AniDB openings/endings"
			if ((CREDITS_UNREAD > 0)); then CREDIT_NOTE="not among those Kizuna's titles could be read for ($LANGS_FIX)"; fi
		fi
	elif ((CREDITS_UNREAD > 0)); then # Kizuna answered, in titles credit_title can't read
		if [[ $letter == - && $SHOW_VARIANTS != *" $kind"* ]]; then
			CREDIT_N=$((100 + num))
			if [[ $kind == ed ]]; then CREDIT_N=$((150 + num)); fi
			CREDIT_NOTE="guessed from the file name, as $LANGS_FIX"
		else
			CREDIT_NOTE=$LANGS_FIX
		fi
	elif [[ $letter != - ]]; then
		CREDIT_NOTE="a variant needs Kizuna with AniDB details to find its number (run with -u)"
	elif [[ $SHOW_VARIANTS == *" $kind"* ]]; then # OP1a/OP1b shift every later number: 100 + n would be wrong
		CREDIT_NOTE="this show has variant files, so its numbers need Kizuna with AniDB details (run with -u)"
	elif [[ $kind == ed ]]; then
		CREDIT_N=$((150 + num))
	else
		CREDIT_N=$((100 + num))
	fi
	((CREDIT_N > 0))
}

# season_episode prints "SEASON EPISODE" for a file whose name or folder gives
# a season above 0 ("Show - S02E01", or "Season 02/Show - 01"), or nothing.
# $1 = file name without extension, $2 = the season of its subfolder, if any.
season_episode() {
	local n
	shopt -s nocasematch
	if [[ $1 =~ $SXE_NUM ]]; then
		shopt -u nocasematch
		if ((10#${BASH_REMATCH[2]} > 0)); then echo "$((10#${BASH_REMATCH[2]})) $((10#${BASH_REMATCH[3]}))"; fi
		return 0
	fi
	shopt -u nocasematch
	if [[ -n $2 ]] && (($2 > 0)); then
		n=$(absolute_number "$1")
		if ((n > 0)); then echo "$2 $n"; fi
	fi
	return 0
}

# --- planning ----------------------------------------------------------------

WARNINGS=()
warn() { WARNINGS+=("$1"); }

PLAN_REL=() PLAN_VAL=() # hints for the current show: show-relative file, episode number
KEPT=0                  # files that already have a hint
SHOW_SKIPPED=0

# hint plans "ep: $2: $1" for the show-relative file $1, unless the file
# already has a hint (yours, or from an earlier run): that one is kept, with a
# warning when it numbers the file differently.
hint() {
	local have=${USER_EP[$1]:-} rd ds
	shopt -s nocasematch
	if [[ $have =~ ^sp[0-9] ]]; then # Plex 1.43 drops files hinted "SPxx"
		shopt -u nocasematch
		warn "$SHOW_DIR/$1: its hint \"ep: $have\" makes Plex leave the file out (Plex reads SPxx without a season); change that line by hand to \"ep: $2: $1\""
		KEPT=$((KEPT + 1))
		return
	fi
	shopt -u nocasematch
	if [[ -n $have ]]; then
		rd=$(dirname -- "$1")
		if [[ $rd == . ]]; then rd=""; fi
		ds=$(dir_season "$rd")
		if [[ $(norm_ep "$have" "${ds:-1}") != "$(norm_ep "$2" "${ds:-1}")" ]]; then
			warn "$SHOW_DIR/$1: kept its hint \"ep: $have\"; Kizuna's numbering is $2. Change that line by hand if it's wrong"
		fi
		KEPT=$((KEPT + 1))
		return
	fi
	PLAN_REL+=("$1") PLAN_VAL+=("$2")
}

plan_show() {
	local dir=$1 sub videos=() v rel rd ds stem n abs loaded=0 season rules=$MODE ckind cnum cletter order
	local relative=0 se rs re pk pe numbered ctext
	SHOW_DIR=$dir PLAN_REL=() PLAN_VAL=() SHOW_SKIPPED=0 ID_FILE_TAG=""
	for sub in "$dir"/*/; do
		sub=$(basename "$sub")
		if [[ $sub =~ $GROUP_DIR ]]; then warn "$dir: grouping folder \"$sub\" - move it to the library root as its own show"; fi
	done
	read_plexmatches "$dir"
	if ((HAS_PATTERN)); then
		warn "$dir: a .plexmatch has pattern: hints, which the script can't evaluate. Show skipped: number its files by hand"
		SHOW_SKIPPED=1
		return
	fi
	if [[ -n $ID_HINT ]]; then
		warn "$dir/$PLEXMATCH: \"$ID_HINT\" is ignored: id hints aren't supported, the show is matched by its folder tag or title. Put a [anidb-N] or [tvdb-N] tag in the folder name instead"
	fi
	if [[ -n $ROOT_SEASON ]] && ((ROOT_SEASON != 1)); then
		warn "$dir/$PLEXMATCH: \"season: $ROOT_SEASON\" puts every file without a hint of its own in season $ROOT_SEASON"
	fi
	read_id_file "$dir"
	order=$(unsupported_order "$(basename "$dir")${ID_FILE_TAG:+ $ID_FILE_TAG}")
	if [[ -n $order ]]; then
		warn "$dir: $order is a HAMA numbering order Kizuna doesn't support: the tag finds the show, but its episodes follow Kizuna's numbering, not that order. Check this show's hints against your files"
	fi
	mapfile -t videos < <(list_videos "$dir")
	SHOW_VARIANTS=""
	shopt -s nocasematch
	if [[ $(basename "$dir") =~ $PLAIN_TVDB_TAG || $ID_FILE_TAG =~ $PLAIN_TVDB_TAG ]]; then
		relative=1 # HAMA plain tvdb: seasons counted from 1
	fi
	shopt -u nocasematch
	for v in "${videos[@]}"; do
		stem=$(basename -- "${v%.*}")
		if ! plex_numbered "$stem" && read -r ckind cnum cletter ctext < <(credit_of "$stem") && [[ $cletter != - ]]; then
			SHOW_VARIANTS+=" $ckind"
		fi
	done
	if [[ $MODE == auto ]] && ((${#videos[@]} > 0)); then
		rules=$(tag_rules "$SHOW_TITLE${ID_FILE_TAG:+ $ID_FILE_TAG}")
		if [[ -z $rules ]]; then rules=$(tag_rules "$(basename "$LIBRARY")/${videos[0]#"$LIBRARY"/}"); fi
		if [[ -z $rules ]]; then # AUTO_DEFAULT and the mapping decide: only Kizuna knows them
			if ((!KIZUNA)); then
				warn "$dir: its first id tag (if any) isn't [anidb-N] or [tvdb-N], so Kizuna's AUTO_DEFAULT decides; Kizuna not reachable, skipped"
				SHOW_SKIPPED=1
				return
			fi
			rules=$(show_rules "${videos[0]}") || {
				warn "$dir: no match from Kizuna at $URL/auto; skipped"
				SHOW_SKIPPED=1
				return
			}
		elif [[ $rules == tvdb ]] && ((!KIZUNA)); then
			warn "$dir: tvdb rules need Kizuna for the seasons; Kizuna not reachable, skipped"
			SHOW_SKIPPED=1
			return
		fi
	fi
	for v in "${videos[@]}"; do
		rel=${v#"$dir"/} stem=$(basename -- "${v%.*}") rd=$(dirname -- "$rel")
		if [[ $rd == . ]]; then rd=""; fi
		ds=""
		if [[ -n $rd ]]; then ds=$(dir_season "$rd"); fi # a subfolder's season, if it has one

		# what Plex makes of the name by itself: an episode (a season and
		# episode in it), an opening/ending extra, a file it leaves out, or none
		numbered=0 pk="" pe=""
		if plex_numbered "$stem"; then
			numbered=1
		else
			read -r pk pe < <(plex_credit "$stem") || pk="" pe=""
		fi
		# an opening or ending: reported, never hinted
		if ((!numbered)) && read -r ckind cnum cletter ctext < <(credit_of "$stem"); then
			if [[ -z $pk ]]; then pk=plain pe=$ctext; fi # not spelled the way Plex takes it
			if [[ $rules == tvdb ]]; then # TVDB numbers its own specials; the 101+ scheme doesn't apply
				warn "$v: $(credit_note "$(basename "$v")" "$pk" "$pe" "" "its TVDB number (S00Exx)")"
				continue
			fi
			if [[ $CREDITS_FOR != "$dir" ]]; then load_credits "$dir" "$v"; fi
			if credit_index "$ckind" "$cnum" "$cletter"; then
				warn "$v: $(credit_note "$(basename "$v")" "$pk" "$pe" "$(printf 'S00E%02d' "$CREDIT_N")" "$CREDIT_NOTE")"
			else
				warn "$v: $(credit_note "$(basename "$v")" "$pk" "$pe" "" "its Kizuna number (S00E101...; ${ckind^^}$cnum${cletter#-}: $CREDIT_NOTE)")"
			fi
			continue
		fi
		n=$(special_number "$stem")
		if ((n > 0)); then
			if [[ -n $pk ]]; then
				if [[ $rules == tvdb ]]; then
					warn "$v: $(credit_note "$(basename "$v")" "$pk" "$pe" "" "its TVDB number (S00Exx)")"
				else
					warn "$v: $(credit_note "$(basename "$v")" "$pk" "$pe" "$(printf 'S00E%02d' "$n")")"
				fi
			elif [[ $rules == tvdb ]]; then
				warn "$v: special file - add its TVDB number by hand (ep: S00Exx: $rel)"
			else
				hint "$rel" "$(printf 'S00E%02d' "$n")"
			fi
			continue
		fi
		# an episode whose name Plex reads as an opening/ending
		if [[ $pk == extra ]]; then
			warn "$v: Plex's scanner makes it an opening/ending extra, not an episode: its name contains \"$pe\". Rename it without that"
		elif [[ $pk == dropped ]]; then
			warn "$v: Plex's scanner leaves it out of the library: its name contains the whole word \"$pe\". Rename it without that"
		fi
		abs=$(absolute_number "$stem")
		shopt -s nocasematch
		if ((abs == 0 && ${#videos[@]} == 1)) && [[ ! $stem =~ $SXE ]]; then # a movie: one file, no number
			shopt -u nocasematch
			hint "$rel" "S01E01"
			continue
		fi
		shopt -u nocasematch
		# plain HAMA tvdb: S02E01 is the 1st episode of TVDB season 2 -> its absolute number
		if [[ $rules == tvdb ]] && ((relative)) && se=$(season_episode "$stem" "$ds") && [[ -n $se ]]; then
			read -r rs re <<<"$se"
			if ((!loaded)); then
				load_seasons "$v" || { warn "$dir: no match from Kizuna at $URL; skipped"; SHOW_SKIPPED=1; return; }
				loaded=1
			fi
			if [[ ${SEASON_OF[$re]:-} == "$rs" ]]; then
				continue # already absolute (season 1 always is)
			fi
			abs=${ABS_OF["$rs $re"]:-}
			if [[ -z $abs ]]; then
				warn "$v: S$(printf '%02d' "$rs")E$(printf '%02d' "$re"): TheTVDB's season $rs of $SHOW_KEY has no episode $re (and $re isn't one of its absolute numbers)"
				continue
			fi
			hint "$rel" "$(printf 'S%02dE%02d' "$rs" "$abs")"
			continue
		fi
		shopt -s nocasematch
		if [[ $rules != tvdb || -n $ds || $stem =~ $SXE ]]; then
			shopt -u nocasematch
			continue # Plex's scanner reads these fine
		fi
		shopt -u nocasematch
		if ((abs == 0)) && [[ -n ${USER_EP[$rel]:-} ]]; then
			KEPT=$((KEPT + 1))
			continue # numbered by a hint of its own
		fi
		if ((abs == 0)); then
			warn "$v: no episode number found"
			continue
		fi
		if ((!loaded)); then
			load_seasons "$v" || { warn "$dir: no match from Kizuna at $URL; skipped"; SHOW_SKIPPED=1; return; }
			loaded=1
		fi
		season=${SEASON_OF[$abs]:-0}
		if ((season == 0)); then
			warn "$v: absolute episode $abs not in $SHOW_KEY"
			continue
		fi
		if ((season == ${ROOT_SEASON:-1})) && [[ -z ${USER_EP[$rel]:-} ]]; then
			continue # the scanner already reads "Show - 05" in the show folder as S01E05
		fi
		hint "$rel" "$(printf 'S%02dE%02d' "$season" "$abs")"
	done
}

# --- writing -------------------------------------------------------------------

CHANGED=()           # show folders whose .plexmatch gets new hints
declare -A NEW_HINTS # show folder -> the ep: lines to add
HINTS=0

# finish_show prints the hints planned for the show's .plexmatch. A .plexmatch
# that is a symlink is never written: the write moves a new file over it,
# which would replace the link with a plain file. Its hints are only listed.
finish_show() {
	local dir=$1 pm=$1/$PLEXMATCH new="" i
	((${#PLAN_REL[@]} > 0)) || return 0
	if [[ -L $pm ]]; then
		echo "$pm: a symlink, not written: add these hints by hand"
		for i in "${!PLAN_REL[@]}"; do printf '  + ep: %s: %s\n' "${PLAN_VAL[i]}" "${PLAN_REL[i]}"; done
		warn "$pm is a symlink, so the script doesn't write it (that would replace the link with a plain file): add the hints listed for it above by hand"
		return 0
	fi
	for i in "${!PLAN_REL[@]}"; do new+="ep: ${PLAN_VAL[i]}: ${PLAN_REL[i]}"$'\n'; done
	HINTS=$((HINTS + ${#PLAN_REL[@]}))
	CHANGED+=("$dir")
	NEW_HINTS[$dir]=$new
	if [[ -f $pm ]]; then echo "$pm: add hints to the existing file"; else echo "$pm: new file"; fi
	for i in "${!PLAN_REL[@]}"; do printf '  + ep: %s: %s\n' "${PLAN_VAL[i]}" "${PLAN_REL[i]}"; done
}

# add_hints appends the ep: lines in $2 to the .plexmatch $1, in the file's own
# line endings. The lines already there stay as they are. The new file is
# written next to it first and then moved over it, so an interrupted run or a
# full disk leaves the old file whole.
add_hints() {
	local pm=$1 new=$2 tmp=$1.kizuna-tmp line nl eol=$'\n' out=""
	if [[ -f $pm ]]; then
		while true; do # bash keeps each line as it is, \r included
			if IFS= read -r line; then nl=$'\n'; elif [[ -n $line ]]; then nl=""; else break; fi
			if [[ $line == *$'\r' ]]; then eol=$'\r\n'; fi # keep a Windows file's line endings
			out+=$line$nl
			if [[ -z $nl ]]; then break; fi # last line without a newline
		done <"$pm"
	fi
	if [[ -n $out && $out != *$'\n' ]]; then out+=$eol; fi
	while IFS= read -r line; do out+=$line$eol; done <<<"${new%$'\n'}"
	# a copy of the old file first, for its permissions (and owner, as root)
	if [[ -f $pm ]]; then cp -p "$pm" "$tmp" 2>/dev/null || :; fi
	if ! printf '%s' "$out" >"$tmp" || ! mv -f "$tmp" "$pm"; then
		rm -f "$tmp" 2>/dev/null || :
		die "can't write $pm (full disk? permissions?): it was left as it was"
	fi
}

# --- main --------------------------------------------------------------------

main() {
	MODE=anidb URL=${KIZUNA_URL:-http://localhost:32600}
	local do_apply=0 d w
	while getopts 'm:u:ah' opt; do
		case $opt in
		m) MODE=$OPTARG ;;
		u) URL=${OPTARG%/} ;;
		a) do_apply=1 ;;
		*) usage ;;
		esac
	done
	shift $((OPTIND - 1))
	(($# == 1)) || usage
	[[ -d $1 ]] || die "$1 is not a folder"
	LIBRARY=${1%/}
	if [[ $LIBRARY == -* ]]; then LIBRARY=./$LIBRARY; fi # not an option to find or basename
	[[ $MODE == anidb || $MODE == tvdb || $MODE == auto ]] || die "unknown mode $MODE (anidb, tvdb or auto)"

	KIZUNA=0
	if [[ $MODE == tvdb ]]; then
		command -v curl >/dev/null && command -v jq >/dev/null || die "$MODE mode needs curl and jq"
		curl -fsS --max-time 10 "$URL/health" >/dev/null || die "Kizuna is not ready at $URL (set -u or KIZUNA_URL)"
		KIZUNA=1
	elif command -v curl >/dev/null && command -v jq >/dev/null && curl -fsS --max-time 10 "$URL/health" >/dev/null 2>&1; then
		KIZUNA=1 # optional in anidb and auto modes: AniDB's own numbers for openings and endings (reported, not hinted), and in auto the tvdb shows
	else
		if [[ $MODE == auto ]]; then
			echo "note: Kizuna not reachable at $URL (or no curl/jq): only folders tagged [anidb-N] are planned;" >&2
			echo "      [tvdb-N] folders, other tags and untagged folders are skipped (start Kizuna or set -u)" >&2
		fi
		echo "note: Kizuna not reachable at $URL (or no curl/jq): the numbers reported for openings and endings come" >&2
		echo "      from their file names, and variants such as OP1b get none (start Kizuna or set -u to fix that)" >&2
	fi

	for d in "$LIBRARY"/*/; do
		d=${d%/} # no hidden folders: the glob skips them
		plan_show "$d"
		if ((!SHOW_SKIPPED)); then finish_show "$d"; fi
	done

	for w in "${WARNINGS[@]}"; do echo "WARNING: $w"; done
	echo "${#CHANGED[@]} .plexmatch files to write, $HINTS hints in all, $KEPT files already have a hint," \
		"${#WARNINGS[@]} warnings"
	((${#CHANGED[@]} > 0)) || return 0
	if ((do_apply)); then
		for d in "${CHANGED[@]}"; do add_hints "$d/$PLEXMATCH" "${NEW_HINTS[$d]}"; done
		echo "wrote ${#CHANGED[@]} .plexmatch files. The new ep: lines are plain lines like your own: the next run"
		echo "keeps them, and to change or remove one, edit the .plexmatch by hand."
	else
		echo "dry run: nothing was changed. Check every hint and WARNING above against your files;"
		echo "the script can't know every naming style. Run again with -a only when the plan is right:"
		echo "the hints it writes aren't tracked, so the script can't change or remove them afterwards."
	fi
}

[[ ${BASH_SOURCE[0]} != "$0" ]] || main "$@"
