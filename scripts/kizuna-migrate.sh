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
#                a movie, the only video without a number once specials,
#                trailers, openings and endings are left out -> ep: S01E01
#   tvdb rules   absolute "Show - 033.mkv"             -> ep: S02E33: Show - 033.mkv
#                plain HAMA [tvdb-N] folders (seasons counted from 1 each):
#                "Show - S02E01.mkv" / "Season 02/Show - 01.mkv" -> ep: S02E26: ...
#                (plus the movie rule; special files are only reported)
#   both rules   a file with one of Plex's extra suffixes ("Movie-trailer.mkv")
#                is left out, like the extras/ folders
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

# the two usage lines of the header, kept here: piped to bash -s, $0 is no file to read them from
usage() {
	printf '%s\n' \
		"  kizuna-migrate.sh [-m anidb|tvdb|auto] [-u URL] LIBRARY     show the planned hints (dry run)" \
		"  kizuna-migrate.sh [-m anidb|tvdb|auto] [-u URL] -a LIBRARY  write them" >&2
	exit 2
}

# The script reads ASCII words in any letter case (Season, OPENING, [ANIDB-1],
# .AVI, TITLE:), with nocasematch and ${x,,}, which follow the locale's letter
# case. In a Turkic one (tr_TR, az_AZ) I and i are no pair: "TITLE" lowers to
# "tıtle", and Git Bash's nocasematch regexes even end the script. There, and
# only there, the script takes the letter case of a locale where they are a
# pair, exported for find -iname too. Every other locale is left as it is, so
# file names compare as they always did.
ascii_case() {
	local i=I u=i
	[[ ${i,,} == i && ${u^^} == I ]]
}
if ! ascii_case; then
	for loc in C.UTF-8 C.utf8 en_US.UTF-8 en_US.utf8 C; do
		# LC_ALL, when set, overrides LC_CTYPE; bash warns about a missing locale
		if [[ -n ${LC_ALL:-} ]]; then { export LC_ALL=$loc; } 2>/dev/null; else { export LC_CTYPE=$loc; } 2>/dev/null; fi
		if ascii_case; then break; fi
	done
	unset loc
fi

die() { echo "error: $*" >&2; exit 1; }

# Helpers that run for every video set a result variable instead of printing:
# each $(...) starts a process, and those made the script slow (on Git Bash
# above all). The variable is the helper's name in capitals (trim sets TRIM,
# core_credit the array CORE_CREDIT), set on every path; numbered_name, user_ep
# and credit_index are older and keep theirs. The printing helpers the tests
# call (absolute_number, credit_of...) call the setter and print its variable.
# Rules for code that calls them:
# - Copy a result into a local straight after the call: other helpers reuse
#   the variables (strip_tags sets TRIM; core_credit and core_special set
#   NAME_TAILS and STRIP_TAGS).
# - Never capture a setter with $(...): it prints nothing. trim, strip_tags,
#   name_tails, season_episode, main_videos and list_videos kept their names
#   and used to print.
# - Between shopt -s nocasematch and shopt -u, call no helper that sets it
#   (core_number, name_tails, plex_numbered...): each leaves it off, also for
#   its caller.

# trim sets TRIM to $1 without leading and trailing whitespace.
trim() {
	TRIM=${1#"${1%%[![:space:]]*}"}
	TRIM=${TRIM%"${TRIM##*[![:space:]]}"}
}

# --- name parsing ------------------------------------------------------------

# strip_tags sets STRIP_TAGS to $1 without its [..], (..) and {..} groups,
# trimmed. What is left without any space is a name that uses underscores for
# them, and is read like the same name with spaces: "[Grp]_Show_-_SP1_[1080p]"
# and "[Grp] Show_-_SP1 [1080p]" like "Show - SP1". Plex takes an underscore
# for a space too. The spaces around the tags are trimmed before that check,
# so they don't count.
strip_tags() {
	local s=$1 re='\[[^]]*\]|\([^)]*\)|\{[^}]*\}'
	while [[ $s =~ $re ]]; do s=${s/"${BASH_REMATCH[0]}"/}; done
	trim "$s"
	if [[ $TRIM != *[[:space:]]* ]]; then trim "${TRIM//_/ }"; fi
	STRIP_TAGS=$TRIM
}

# Regexes are matched with nocasematch on (case-insensitive).
# group 2 is the season, 3 the episode
SXE='(^|[^[:alnum:]_])s([0-9]{1,2})[[:space:]]*e([0-9]{1,4})([^[:alnum:]_]|$)'
# a plain HAMA tvdb tag ([tvdb-81797], {tvdb-81797}): no digit after "tvdb", unlike [tvdb3-...]
PLAIN_TVDB_TAG='[[{]tvdb-[0-9]+[]}]'
# an id tag as Kizuna reads it (tagRe in internal/provider/match.go); the first one in a name counts
ID_TAG='[[{](anidb|tvdb|tmdb|imdb|mal|anilist|kitsu|ann)[0-9]?-(tt[0-9]+|[0-9]+)[]}]'
# a release's version after its number ("05v2"): one group, counted in the
# group numbers of the patterns that use it
VERSION_RE='(v[0-9])?'
ABS_DASH=' - ([0-9]{1,4})'"$VERSION_RE"'([^[:alnum:]_]|$)'
ABS_EP='(^|[^[:alnum:]_])(e|ep|episode)[[:space:]]*([0-9]{1,4})'"$VERSION_RE"'([^[:alnum:]_]|$)'
ABS_END='(^|[^[:alnum:]_])([0-9]{1,4})'"$VERSION_RE"'[[:space:]]*$'
SEASON_DIR='^((season|series|saison|staffel|temporada)[ ._-]*[0-9]{1,4}|s[0-9]{1,4}|specials?)$'
GROUP_DIR='^[[:space:]]*[[(][0-9]+[])]'
# openings / endings: NCOP1, OP1b, ED, Ending 2a (kind, number, variant letter)
CREDIT_RE='^(nc)?(op|opening|ed|ending)[[:space:]]*0*([0-9]{1,2})?[[:space:]]*([a-z])?'"$VERSION_RE"'$'
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
# Plex's suffixes for a local extra next to the video ("Movie (2016)-trailer"):
# Plex makes the file an extra, never an episode or the movie
PLEX_EXTRA_RE='-(behindthescenes|deleted|featurette|interview|scene|short|trailer|other)$'
# what may be an episode number in a name the script reads none in: "1x01",
# or a word of 1 to 3 digits ("Show 01 - Title", "Show.01.Title.1080p").
# Plex's scanner reads some of these, so a file with one isn't reported.
NUMBER_LIKE_RE='(^|[^[:alnum:]])([0-9]{1,2}x[0-9]{1,3}|[0-9]{1,3})([^[:alnum:]]|$)'
# a spelled-out word and its number, as plex_credit prints them: "NCOpening 1"
SHORT_CREDIT_RE='^(NC)?(OP|ED)[ ._]([0-9].*)$'
# AniDB credit titles: "Opening 1a", "Opening 1 (b)", "Ending", "Ending 2: Song",
# and the Japanese ones Kizuna sends when EPISODE_LANGS puts ja first
# ("オープニング2", "エンディング")
CREDIT_TITLE_RE='^(opening|op|ending|ed|オープニング|エンディング)([[:space:]]*0*([0-9]+))?[[:space:]]*(\(([a-z])\)|([a-z]))?([^[:alnum:]]|$)'
# full-width digits, letters and brackets, turned into the ASCII at the same
# place in FULLWIDTH_ASCII before CREDIT_TITLE_RE reads a title: CREDIT_TITLE_RE
# only knows ASCII, so "オープニング1（ｂ）" would lose its letter and read as
# opening 1, and "オープニング２" would be unreadable
FULLWIDTH=(０ １ ２ ３ ４ ５ ６ ７ ８ ９
	Ａ Ｂ Ｃ Ｄ Ｅ Ｆ Ｇ Ｈ Ｉ Ｊ Ｋ Ｌ Ｍ Ｎ Ｏ Ｐ Ｑ Ｒ Ｓ Ｔ Ｕ Ｖ Ｗ Ｘ Ｙ Ｚ
	ａ ｂ ｃ ｄ ｅ ｆ ｇ ｈ ｉ ｊ ｋ ｌ ｍ ｎ ｏ ｐ ｑ ｒ ｓ ｔ ｕ ｖ ｗ ｘ ｙ ｚ （ ） 　)
FULLWIDTH_ASCII='0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz() '
# Kizuna's title for a credit AniDB has no title for: not unreadable, just empty
CREDIT_PLACEHOLDER_RE='^(episode|ep\.?)[[:space:]]*[0-9]+$'
# other special patterns: "regex|base|number group" (no group = number 1)
SPECIALS=(
	'^(pv|cm|trailer|teaser|promo|preview)[[:space:]]*0*([0-9]{1,2})?'"$VERSION_RE"'$|200|2'
	'^(sp|special|ova|oad|s)[[:space:]]*0*([0-9]{1,3})'"$VERSION_RE"'$|0|2'
	'^(ova|oad|special)$|0|0'
)

# numbered_name says whether name $1 (tags stripped) is a numbered episode
# whatever the rest of it says: "Show - 05", "Show Ep 05", S01E05. NAME_NUM
# is its absolute number, empty for S01E05. Call it with nocasematch on.
# absolute_number and name_tails both ask here, so a form read as an episode
# number is never also read as a special.
# A number ending the name (ABS_END) is not one of these forms: "Show OVA 2"
# and "Show - Special 2" end in a number too, and are specials.
numbered_name() {
	NAME_NUM=""
	if [[ $1 =~ $ABS_DASH ]]; then NAME_NUM=$((10#${BASH_REMATCH[1]}))
	elif [[ $1 =~ $ABS_EP ]]; then NAME_NUM=$((10#${BASH_REMATCH[3]}))
	elif [[ ! $1 =~ $SXE ]]; then return 1; fi
}

# core_number sets CORE_NUMBER to the episode number in a file name without
# its tags (strip_tags), or 0.
core_number() {
	CORE_NUMBER=0
	shopt -s nocasematch
	if numbered_name "$1" && [[ -n $NAME_NUM ]]; then CORE_NUMBER=$NAME_NUM
	elif [[ $1 =~ $ABS_END ]]; then CORE_NUMBER=$((10#${BASH_REMATCH[2]})); fi
	shopt -u nocasematch
}

# absolute_number prints the episode number in a file name, or 0.
absolute_number() {
	strip_tags "$1"
	core_number "$STRIP_TAGS"
	echo "$CORE_NUMBER"
}

# name_tails sets NAME_TAILS to the parts of a file name without its tags
# that may name a special: the text after the last " - ", the last two words,
# the last word. None for a numbered episode (S01E05, "Show - 05", "Show Ep
# 05"): a word in its title ("Show Ep 13 - Christmas Special") doesn't make
# it a special. It leaves nocasematch off (see the rules at the top).
name_tails() {
	local words
	NAME_TAILS=()
	shopt -s nocasematch
	if numbered_name "$1"; then
		shopt -u nocasematch
		return 0
	fi
	shopt -u nocasematch
	if [[ $1 == *" - "* ]]; then NAME_TAILS+=("${1##* - }"); fi
	read -ra words <<<"$1"
	if ((${#words[@]} > 1)); then NAME_TAILS+=("${words[-2]} ${words[-1]}"); fi
	if ((${#words[@]} > 0)); then NAME_TAILS+=("${words[-1]}"); fi
}

# core_credit sets CORE_CREDIT to (op|ed NUMBER LETTER TEXT) for an opening
# or ending file name without its tags, TEXT being the part of the name that
# says so (Show - NCOP1 -> op 1 - NCOP1, OP1b -> op 1 b OP1b, ED -> ed 1 - ED),
# or to none.
core_credit() {
	local t kind num letter
	CORE_CREDIT=()
	name_tails "$1"
	for t in "${NAME_TAILS[@]}"; do
		strip_tags "$t"
		t=$STRIP_TAGS
		shopt -s nocasematch
		if [[ $t =~ $CREDIT_RE ]]; then
			kind=${BASH_REMATCH[2],,} num=${BASH_REMATCH[3]:-1} letter=${BASH_REMATCH[4],,}
			shopt -u nocasematch
			if [[ $kind == e* ]]; then kind="ed"; else kind="op"; fi
			CORE_CREDIT=("$kind" "$((10#$num))" "${letter:--}" "$t")
			return 0
		fi
		shopt -u nocasematch
	done
}

# credit_of prints core_credit's answer for a file name, on one line.
credit_of() {
	strip_tags "$1"
	core_credit "$STRIP_TAGS"
	if ((${#CORE_CREDIT[@]} > 0)); then printf '%s %s %s %s\n' "${CORE_CREDIT[@]}"; fi
}

# credit_title prints "NUMBER LETTER" for an AniDB credit title
# ("Opening 1a" -> "1 a", "Ending" -> "1 -", "オープニング２" -> "2 -"), or
# nothing.
credit_title() {
	local num letter t=$1 i
	for i in "${!FULLWIDTH[@]}"; do t=${t//"${FULLWIDTH[i]}"/"${FULLWIDTH_ASCII:i:1}"}; done
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

# number_like says whether file name $1 (without its extension), one the
# script reads no number in, still has what may be one: NUMBER_LIKE_RE, or
# any digit in a season folder ($2, its season, is set), where Plex reads a
# bare number too. Such a file isn't reported: a wrong warning on every
# episode of a show is worse than a missed one.
number_like() {
	local core
	strip_tags "$1"
	core=$STRIP_TAGS
	if [[ -n $2 && $core == *[0-9]* ]]; then return 0; fi
	shopt -s nocasematch
	if [[ $core =~ $NUMBER_LIKE_RE ]]; then
		shopt -u nocasematch
		return 0
	fi
	shopt -u nocasematch
	return 1
}

# plex_extra says whether Plex makes file name $1 (without its extension) a
# local extra by its suffix: "Movie (2016)-trailer", "Show-featurette".
plex_extra() {
	shopt -s nocasematch
	if [[ $1 =~ $PLEX_EXTRA_RE ]]; then
		shopt -u nocasematch
		return 0
	fi
	shopt -u nocasematch
	return 1
}

# plex_credit_of sets PLEX_CREDIT_OF to what Plex's scanner does with file
# name $1 (without its extension) as an opening or ending, and the part of
# the name that does it: (extra NCOP1) for an opening/ending extra, (dropped
# "Opening 1") for one left out of the library (the whole word Opening or
# Ending without Creditless or Clean in front). None for a name Plex reads
# like any other. The Go port of the same rules is CreditOf in
# internal/plex/title.go: keep the two in step.
plex_credit_of() {
	local kind=extra word
	PLEX_CREDIT_OF=()
	if plex_numbered "$1"; then return 0; fi
	[[ $1 =~ $PLEX_CREDIT_RE ]] || return 0
	word=${BASH_REMATCH[5]}
	if [[ ($word == Opening || $word == Ending) && -z ${BASH_REMATCH[4]} ]]; then kind=dropped; fi
	PLEX_CREDIT_OF=("$kind" "${BASH_REMATCH[2]}")
}

# plex_credit prints plex_credit_of's answer on one line, and fails for none.
plex_credit() {
	plex_credit_of "$1"
	((${#PLEX_CREDIT_OF[@]} > 0)) || return 1
	printf '%s %s\n' "${PLEX_CREDIT_OF[@]}"
}

# short_name sets SHORT_NAME to the name Plex makes an extra of for a
# spelled-out one ("Opening 1" -> "OP1", "NCOpening" -> "NCOP", "Ending a" ->
# "ED a").
short_name() {
	local s=${1/Opening/OP}
	s=${s/Ending/ED}
	if [[ $s =~ $SHORT_CREDIT_RE ]]; then s=${BASH_REMATCH[1]}${BASH_REMATCH[2]}${BASH_REMATCH[3]}; fi
	SHORT_NAME=$s
}

short_credit() { short_name "$1"; printf '%s' "$SHORT_NAME"; }

# capital_name sets CAPITAL_NAME to the name Plex makes an extra of for an
# opening or ending spelled another way ("op1" -> "OP1", "ncop2" -> "NCOP2",
# "ending 2A" -> "ED2a"), or to "".
capital_name() {
	local nc word num letter
	CAPITAL_NAME=""
	shopt -s nocasematch
	if [[ $1 =~ $CREDIT_RE ]]; then
		nc=${BASH_REMATCH[1]^^} word=${BASH_REMATCH[2],,} num=${BASH_REMATCH[3]} letter=${BASH_REMATCH[4],,}
		shopt -u nocasematch
		if [[ $word == e* ]]; then word=ED; else word=OP; fi
		CAPITAL_NAME=$nc$word${num:+$((10#$num))}$letter
		return 0
	fi
	shopt -u nocasematch
}

capital_credit() { capital_name "$1"; printf '%s' "$CAPITAL_NAME"; }

# credit_note warns about an opening or ending file, which never gets a hint:
# $1 the file, $2 what Plex does with it (extra or dropped, as plex_credit_of
# says, or plain: it doesn't take the name), $3 the part of the name that
# says so, $4 the special number it would have, if known, with $5 a remark on
# that number, else $5 how to find the number; $6 1 when the number is
# guessed from the file name, not Kizuna's. The script renames nothing, so
# making the file a special is left to the user: the note only says how.
credit_note() {
	local note short name=${1##*/}
	case $2 in
	extra)
		note="Plex's scanner makes files with \"$3\" in the name opening/ending extras and ignores .plexmatch hints for them, so it stays an extra (it still plays from the show page) and gets no hint"
		;;
	dropped)
		short_name "$3"
		short=$SHORT_NAME
		note="Plex's scanner leaves files with the whole word \"$3\" in the name out of the library, as neither an extra nor an episode, so it gets no hint. To have it as an extra, rename it with $short in place of that, e.g. to \"${name/"$3"/$short}\""
		;;
	*)
		capital_name "$3"
		short=$CAPITAL_NAME
		note="Plex's scanner only takes these words in capitals, so \"$3\" doesn't make it an opening/ending extra, and the script writes no hint for openings and endings. To have it as an extra (Kizuna can then give it a readable title), rename it with $short in place of that, e.g. to \"${name/"$3"/$short}\""
		;;
	esac
	note+=". If you want it as a special, that is yours to do: rename it to a name without \"$3\" or another such word, and add "
	if [[ -n ${4:-} ]]; then
		if ((${6:-0})); then
			note+="\"ep: $4: <new name>\" to the show's .plexmatch by hand ($4 is its number from the file name, right unless AniDB has variants of it${5:+; $5})"
		else
			note+="\"ep: $4: <new name>\" to the show's .plexmatch by hand ($4 is Kizuna's number for it${5:+; $5})"
		fi
	else
		note+="an ep: hint for the new name, with ${5:-its number}, to the show's .plexmatch by hand"
	fi
	warn "$1: $note"
}

# core_special sets CORE_SPECIAL to the season-0 number of other special
# file names without their tags (PV -> 201, SP03 -> 3); 0 = not one. Openings
# and endings: core_credit.
core_special() {
	local t entry re base group n
	CORE_SPECIAL=0
	name_tails "$1"
	shopt -s nocasematch
	for t in "${NAME_TAILS[@]}"; do
		strip_tags "$t"
		t=$STRIP_TAGS
		for entry in "${SPECIALS[@]}"; do
			re=${entry%|*|*} base=${entry%|*} base=${base##*|} group=${entry##*|}
			if [[ $t =~ $re ]]; then
				n=1
				if ((group > 0)) && [[ -n ${BASH_REMATCH[group]} ]]; then n=$((10#${BASH_REMATCH[group]})); fi
				if ((base + n > 0)); then
					shopt -u nocasematch
					CORE_SPECIAL=$((base + n))
					return 0
				fi
			fi
		done
	done
	shopt -u nocasematch
}

# special_number prints core_special's answer for a file name.
special_number() {
	strip_tags "$1"
	core_special "$STRIP_TAGS"
	echo "$CORE_SPECIAL"
}

# file_stem sets FILE_STEM to the name of the video path $1 without its
# extension, as basename gives it: a file named only ".mkv" gets its folder's.
file_stem() {
	local s=${1%.*}
	s=${s%/}
	FILE_STEM=${s##*/}
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

SHOW_FILES=() # the current show's files: show_files

# show_files sets SHOW_FILES to the files under the show folder $1, in find's
# order, skipping hidden and "extras" folders: one listing for both the
# .plexmatch files and the videos. Symlinked folders and files are followed, as
# Plex does (find -L reports a symlink loop and skips it; MAX_DEPTH bounds one
# it doesn't).
show_files() {
	mapfile -t SHOW_FILES < <(find -L "$1" -mindepth 1 -maxdepth "$MAX_DEPTH" \( -type d \( -name '.*' -o -iname extras \) -prune \) -o -type f -print)
}

# list_videos sets LIST_VIDEOS to the videos in SHOW_FILES, sorted.
list_videos() {
	local f list=""
	LIST_VIDEOS=()
	for f in "${SHOW_FILES[@]}"; do if is_video "$f"; then list+=$f$'\n'; fi; done
	if [[ -n $list ]]; then mapfile -t LIST_VIDEOS < <(LC_ALL=C sort <<<"${list%$'\n'}"); fi
}

# --- existing .plexmatch files -------------------------------------------------

SHOW_TITLE="" SHOW_YEAR=0 # what Plex sends to Kizuna for the current show
ID_HINT=""                # an id hint (tvdbid:, guid:...) in the show's .plexmatch: not supported
ROOT_SEASON=""                        # a season: hint in the show folder's .plexmatch
HAS_PATTERN=0                         # a pattern: hint anywhere in the show
declare -A USER_EP=()                 # hint_key of a file -> episode from an ep: hint already there
declare -A USER_EP_LC=()              # a USER_EP key in small letters -> that key (the first hint's)
declare -A USER_EP_AS=()              # a USER_EP key -> the file name as its line writes it
declare -A USER_EP_DIR=()             # a USER_EP key -> the show-relative folder of its .plexmatch
declare -A USER_EP_SK=()              # a USER_EP key -> space_key of the name as written, with its folder
declare -A DIR_SEASON=()              # show-relative folder -> season from its own season: hint
UTF8_BOM=$'\xEF\xBB\xBF'              # what some Windows editors put at the start of a file

# space_key sets SPACE_KEY to the show-relative path $1 without the spaces at
# the start of its name and folders: what an ep: line keeps of a path written
# plainly. A hint whose name, read that way, isn't the file's is written in
# another way (./, a backslash, other capital letters) that Plex may not read.
space_key() {
	local k=${1#"${1%%[![:space:]]*}"}
	while [[ $k =~ ^(.*/)[[:space:]]+(.*)$ ]]; do k=${BASH_REMATCH[1]}${BASH_REMATCH[2]}; done
	SPACE_KEY=$k
}

# hint_key sets HINT_KEY to the key a show-relative file is known by in
# USER_EP, the same for the file and for the ep: line that names it: "./" and
# the spaces at the start of a name are dropped (a line loses them when it's
# read, the script's own included), a backslash is a slash. Letter case is
# kept. No subshell: it runs for every video.
hint_key() {
	local k=${1//\\//}
	k=${k#"${k%%[![:space:]]*}"}
	while [[ $k == ./* ]]; do k=${k#./} k=${k#"${k%%[![:space:]]*}"}; done
	while [[ $k =~ ^(.*/)(\./|[[:space:]]+)(.*)$ ]]; do k=${BASH_REMATCH[1]}${BASH_REMATCH[3]}; done
	HINT_KEY=$k
}

# read_plexmatches reads every .plexmatch in the show folder $1 and below,
# in the folders show_files looks in, and leaves that listing in SHOW_FILES
# for list_videos. Hints in subfolders only count for their own folder, like
# in Plex. Every ep: line counts as the user's, also those an earlier run
# wrote.
read_plexmatches() {
	local dir=$1 pm reldir line name value file top as
	SHOW_TITLE=${dir##*/} SHOW_YEAR=0 ID_HINT="" ROOT_SEASON="" HAS_PATTERN=0
	USER_EP=() USER_EP_LC=() USER_EP_AS=() USER_EP_DIR=() USER_EP_SK=() DIR_SEASON=()
	show_files "$dir"
	for pm in "${SHOW_FILES[@]}"; do
		[[ ${pm##*/} == "$PLEXMATCH" ]] || continue
		reldir=${pm%/"$PLEXMATCH"} reldir=${reldir#"$dir"} reldir=${reldir#/} # "" = the show folder
		top=1
		while IFS= read -r line || [[ -n $line ]]; do
			if ((top)) && [[ $line == "$UTF8_BOM"* ]]; then
				# read as if it weren't there; the file keeps it (add_hints copies its lines)
				line=${line#"$UTF8_BOM"}
				warn "$pm: starts with a byte order mark, and Plex may not read its first line. Save the file without it (UTF-8 without BOM)"
			fi
			top=0
			trim "${line%$'\r'}"
			line=$TRIM
			if [[ -z $line || $line == \#* || $line != *:* ]]; then continue; fi
			trim "${line%%:*}"
			name=$TRIM
			trim "${line#*:}"
			value=$TRIM
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
					value=${BASH_REMATCH[1]} as=${BASH_REMATCH[2]}
					as=${as#"${as%%[![:space:]]*}"}
					hint_key "$as"
					if [[ -z $HINT_KEY ]]; then continue; fi # "ep: 5:" names no file
					if [[ -n $reldir ]]; then hint_key "$reldir/$HINT_KEY"; fi
					file=$HINT_KEY
					USER_EP[$file]=$value
					# A later line for the file wins, but not over how an earlier
					# one wrote its name when that one was plain (its space_key is
					# the file's key): Plex reads that line, so the name needs no
					# warning
					if [[ ${USER_EP_SK[$file]:-} != "$file" ]]; then
						USER_EP_AS[$file]=$as USER_EP_DIR[$file]=$reldir
						space_key "${reldir:+$reldir/}$as"
						USER_EP_SK[$file]=$SPACE_KEY
					fi
					if [[ -z ${USER_EP_LC[${file,,}]:-} ]]; then USER_EP_LC[${file,,}]=$file; fi
				fi
				;;
			esac
		done <"$pm"
	done
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
	if [[ ${dir##*/} =~ $ID_TAG ]]; then
		shopt -u nocasematch
		return 0 # a tag in the folder name wins, as in HAMA
	fi
	for f in "$dir"/*; do
		base=${f##*/}
		if [[ -f $f && $base =~ $ID_FILE_RE && -z ${by_kind[${BASH_REMATCH[1],,}]:-} ]]; then
			by_kind[${BASH_REMATCH[1],,}]=$f
		fi
	done
	shopt -u nocasematch
	for kind in anidb tvdb tmdb imdb; do
		f=${by_kind[$kind]:-}
		[[ -n $f ]] || continue
		if [[ -n $used ]]; then others+=" ${f##*/}"; continue; fi
		line=""
		IFS= read -r line <"$f" || true
		trim "${line%$'\r'}"
		line=$TRIM
		base=${f##*/}
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
		warn "$dir: $used says $ID_FILE_TAG, but Plex never reads .id files. Rename the folder to \"${dir##*/} $ID_FILE_TAG\" before switching the library (the hints above assume that tag)"
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

# season_of_dir sets SEASON_OF_DIR to the season Plex's scanner gives the
# show-relative folder $1 ("" = the show folder): its season: hint, its name
# (Season 02, Specials), or "" for other folders.
season_of_dir() {
	local rd=$1 base
	SEASON_OF_DIR=""
	if [[ -z $rd ]]; then SEASON_OF_DIR=${ROOT_SEASON:-1}; return 0; fi
	if [[ -n ${DIR_SEASON[$rd]:-} ]]; then SEASON_OF_DIR=${DIR_SEASON[$rd]}; return 0; fi
	base=${rd##*/}
	shopt -s nocasematch
	if [[ $base =~ $SEASON_DIR ]]; then
		if [[ $base =~ ([0-9]+) ]]; then SEASON_OF_DIR=$((10#${BASH_REMATCH[1]})); else SEASON_OF_DIR=0; fi
	fi
	shopt -u nocasematch
}

dir_season() {
	season_of_dir "$1"
	if [[ -n $SEASON_OF_DIR ]]; then echo "$SEASON_OF_DIR"; fi
}

# ep_pair sets EP_PAIR to "SEASON EPISODE" for a .plexmatch episode number
# ($2 = the folder's season, for a bare number), the start of a range: SP04
# -> "0 4", S03E04 -> "3 4", E02 / 02 -> "$2 2"; "" for another one.
ep_pair() {
	local v=${1%%-*}
	EP_PAIR=""
	shopt -s nocasematch
	if [[ $v =~ ^sp([0-9]+)$ ]]; then EP_PAIR="0 $((10#${BASH_REMATCH[1]}))"
	elif [[ $v =~ ^s([0-9]+)e([0-9]+)$ ]]; then EP_PAIR="$((10#${BASH_REMATCH[1]})) $((10#${BASH_REMATCH[2]}))"
	elif [[ $v =~ ^e?([0-9]+)$ ]]; then EP_PAIR="$2 $((10#${BASH_REMATCH[1]}))"
	fi
	shopt -u nocasematch
}

norm_ep() {
	ep_pair "$1" "$2"
	if [[ -n $EP_PAIR ]]; then echo "$EP_PAIR"; fi
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

# match_key prints the rating key of Kizuna's match for the current show, the
# request made the way Plex makes it: $1 = one of its videos, $2 = 1 for a
# manual match, $3 = the provider. Nothing when there is no match; fails when
# Kizuna doesn't answer. The key goes into warnings and request paths, so a
# key that isn't Kizuna's show key (anidb-N, tvdb-N) counts as no match: what
# answers at $URL may not be Kizuna, and its text could hold terminal escape
# sequences or a path.
match_key() {
	curl -fsS --max-time 60 -H 'Content-Type: application/json' -d "$(match_body "$1" "$2")" \
		"$URL/$3/library/metadata/matches" |
		jq -r '.MediaContainer.Metadata[0].ratingKey // empty | select(type == "string" and test("\\A(anidb|tvdb)-[0-9]+\\z"))' |
		tr -d '\r'
}

# A whole number in Kizuna's answer, for jq. Anything else in a number's place
# (a string, a fraction) is not Kizuna's, and the entry is left out.
JQ_WHOLE='def whole: type == "number" and . == floor;'

declare -A SEASON_OF # absolute episode -> season, for the current show
declare -A ABS_OF    # "season N" -> absolute number of the season's Nth episode, for the current show
SHOW_KEY=""          # its Kizuna rating key

# load_seasons asks Kizuna for the show of video $1 and sets SHOW_KEY,
# SEASON_OF and ABS_OF. Fails when Kizuna has no match.
load_seasons() {
	local video=$1 key lines abs season
	local -A nth=()
	SEASON_OF=() ABS_OF=() SHOW_KEY=""
	key=$(match_key "$video" 0 "$MODE") || return 1
	[[ -n $key ]] || return 1
	lines=$(curl -fsS --max-time 60 -H 'X-Plex-Container-Size: 100000' \
		"$URL/$MODE/library/metadata/$key/grandchildren" |
		jq -r "$JQ_WHOLE"'
			[.MediaContainer.Metadata[]? | select((.index | whole) and (.parentIndex | whole) and .parentIndex > 0)]
			| sort_by(.parentIndex)[] | "\(.index | floor) \(.parentIndex | floor)"' | tr -d '\r') ||
		return 1
	while read -r abs season; do
		# bash evaluates a value used as a number, so only digits get that far:
		# what answers at $URL may not be Kizuna
		[[ $abs =~ ^[0-9]+$ && $season =~ ^[0-9]+$ ]] || continue
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
	key=$(match_key "$1" 1 auto) || return 1
	[[ $key == anidb-* || $key == tvdb-* ]] || return 1
	echo "${key%%-*}"
}

# --- openings and endings (anidb rules) ---------------------------------------

declare -A CREDIT_AT # "op 1 a" -> Plex episode (101...), for the show in CREDITS_FOR
CREDITS_FOR=""
CREDITS_UNREAD=0 # Kizuna's credit titles for that show that credit_title can't read
CREDIT_N=0 CREDIT_NOTE="" CREDIT_GUESS=0
# what helps with titles the script can't read: Kizuna sends them in its
# EPISODE_LANGS, a setting for all of Kizuna, so only for the run
LANGS_FIX="for this run, put en first in Kizuna's EPISODE_LANGS and restart it (then set it back)"
SHOW_VARIANTS="" # " op ed": kinds with variant files (OP1b...) in the current show folder

# fetch_credits prints "INDEX<TAB>TITLE" for the openings and endings Kizuna
# has for the show of video $1: its season 0 from 101 on.
fetch_credits() {
	local key
	key=$(match_key "$1" 1 "$MODE") || return 0
	[[ $key == anidb-* ]] || return 0
	curl -fsS --max-time 120 -H 'X-Plex-Container-Size: 100000' "$URL/$MODE/library/metadata/$key-s0/children" |
		jq -r "$JQ_WHOLE"'
			.MediaContainer.Metadata[]? | select((.index | whole) and .index > 100 and .index < 200)
			| "\(.index | floor)\t\(.title)"' |
		tr -d '\r' || return 0
}

# load_credits fills CREDIT_AT from Kizuna's credit titles for the show in $1
# ($2 = one video in it). Empty when Kizuna isn't used or has no titles
# (AniDB unavailable). CREDITS_UNREAD counts the titles it couldn't read, such
# as a song name or a language without "Opening" in it; Kizuna's placeholder
# for a credit without any title isn't one.
load_credits() {
	local index title kind num letter
	CREDIT_AT=() CREDITS_FOR=$1 CREDITS_UNREAD=0
	((KIZUNA)) || return 0
	while IFS=$'\t' read -r index title; do
		# 101 to 199 only: a title with a line break in it starts a line of its
		# own here, and its start is no number of Kizuna's
		[[ $index =~ ^1[0-9][0-9]$ && $index != 100 ]] || continue
		kind=op
		if ((index > 150)); then kind="ed"; fi
		if read -r num letter < <(credit_title "$title"); then
			if [[ -z ${CREDIT_AT["$kind $num $letter"]:-} ]]; then CREDIT_AT["$kind $num $letter"]=$index; fi
		elif ! [[ ${title,,} =~ $CREDIT_PLACEHOLDER_RE ]]; then
			CREDITS_UNREAD=$((CREDITS_UNREAD + 1))
		fi
	done < <(fetch_credits "$2")
}

# credit_guess sets CREDIT_N to 100/150 + number from the file name ($1
# op|ed, $2 number), with CREDIT_GUESS set: only right without variants.
credit_guess() {
	CREDIT_N=$((100 + $2)) CREDIT_GUESS=1
	if [[ $1 == ed ]]; then CREDIT_N=$((150 + $2)); fi
}

# credit_index sets CREDIT_N to the Plex number of an opening or ending
# ($1 op|ed, $2 number, $3 letter or -) and CREDIT_NOTE to anything worth
# saying about it. With Kizuna's titles it uses AniDB's own numbering; without
# them 100/150 + number (CREDIT_GUESS), which is only safe without a variant
# letter and without variant files of that kind in the show.
credit_index() {
	local kind=$1 num=$2 letter=$3 l
	CREDIT_N=0 CREDIT_NOTE="" CREDIT_GUESS=0
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
			if ((CREDITS_UNREAD > 0)); then
				CREDIT_NOTE="not among the ones whose Kizuna titles the script can read; $LANGS_FIX"
			fi
		fi
	elif ((CREDITS_UNREAD > 0)); then
		# Kizuna answered, in titles credit_title can't read: AniDB's list may
		# have variants that shift the numbers, so no guess from the file name
		CREDIT_NOTE="Kizuna's titles for this show's openings and endings can't be read; $LANGS_FIX"
	elif [[ $letter != - ]]; then
		CREDIT_NOTE="a variant needs Kizuna with AniDB details to find its number (run with -u)"
	elif [[ $SHOW_VARIANTS == *" $kind"* ]]; then # OP1a/OP1b shift every later number: 100 + n would be wrong
		CREDIT_NOTE="this show has variant files, so its numbers need Kizuna with AniDB details (run with -u)"
	else
		credit_guess "$kind" "$num"
	fi
	((CREDIT_N > 0))
}

# season_episode sets SEASON_EPISODE to (SEASON EPISODE) for a file whose name
# or folder gives a season above 0 ("Show - S02E01", or "Season 02/Show - 01"),
# or to none. $1 = file name without extension, $2 = the season of its
# subfolder, if any, $3 = the episode number in its name (core_number).
season_episode() {
	SEASON_EPISODE=()
	shopt -s nocasematch
	if [[ $1 =~ $SXE ]]; then
		shopt -u nocasematch
		if ((10#${BASH_REMATCH[2]} > 0)); then SEASON_EPISODE=("$((10#${BASH_REMATCH[2]}))" "$((10#${BASH_REMATCH[3]}))"); fi
		return 0
	fi
	shopt -u nocasematch
	if [[ -n $2 ]] && (($2 > 0 && $3 > 0)); then SEASON_EPISODE=("$2" "$3"); fi
	return 0
}

# --- planning ----------------------------------------------------------------

WARNINGS=()
warn() { WARNINGS+=("$1"); }

PLAN_REL=() PLAN_VAL=() # hints for the current show: show-relative file, episode number
KEPT=0                  # files that already have a hint
SHOW_SKIPPED=0
# What same_numbers needs to know about the current show's files, recorded by
# plan_show as it reads them, so that it reads no name again:
declare -A HINT_SRC=()      # show-relative file -> the season and episode its hint was planned from (S02E01)
declare -A NAME_KEY=()      # show-relative file -> the episode Plex reads from its folder and name, when no hint is planned for it
declare -A FOLDER_SEASON=() # show-relative folder -> its season, as dir_season gives it ("" for none)
declare -A VIDEO_LC=()      # hint_key of a video in small letters -> how many of the show's videos have it

# user_ep sets HAVE to the episode of the ep: hint the show's .plexmatch files
# already have for the show-relative file $1, "" for none. When that hint
# writes the name in a way Plex may not read as this file's (./, a backslash,
# other capital letters), it still counts as the file's, so that no second
# line is added next to it, and HAVE_AS is the name as the line writes it,
# HAVE_IN the folder of its .plexmatch ("" otherwise). Other capital letters
# count only when no other video of the show has that name in other letters:
# on Linux "a.mkv" and "A.mkv" are two files, and each hint is the one file's
# it names exactly. No subshell: it runs for every video.
user_ep() {
	local k lc
	hint_key "$1"
	k=$HINT_KEY HAVE=${USER_EP[$HINT_KEY]:-} HAVE_AS="" HAVE_IN=""
	if [[ -z $HAVE ]]; then
		lc=${HINT_KEY,,}
		if [[ -n ${USER_EP_LC[$lc]:-} && ${VIDEO_LC[$lc]:-0} == 1 ]]; then
			k=${USER_EP_LC[$lc]} HAVE=${USER_EP[${USER_EP_LC[$lc]}]}
		fi
	fi
	if [[ -n $HAVE ]]; then
		space_key "$1"
		if [[ ${USER_EP_SK[$k]:-} != "$SPACE_KEY" ]]; then HAVE_AS=${USER_EP_AS[$k]:-} HAVE_IN=${USER_EP_DIR[$k]:-}; fi
	fi
}

# hint plans "ep: $2: $1" for the show-relative file $1, unless the file
# already has a hint (yours, or from an earlier run): that one is kept, with a
# warning when it numbers the file differently. $3, if given, is the season
# and episode the number was read from (S02E01 for the plain tvdb S02E04):
# two files read from the same one are one episode in two layouts.
hint() {
	if [[ -n ${3:-} ]]; then HINT_SRC[$1]=$3; fi
	user_ep "$1"
	local have=$HAVE rd ds pair
	shopt -s nocasematch
	if [[ $have =~ ^sp[0-9] ]]; then # Plex 1.43 drops files hinted "SPxx"
		shopt -u nocasematch
		warn "$SHOW_DIR/$1: its hint \"ep: $have\" makes Plex leave the file out (Plex reads SPxx without a season); change that line by hand to \"ep: $2: $1\""
		KEPT=$((KEPT + 1))
		return
	fi
	shopt -u nocasematch
	if [[ -n $have ]]; then
		rd=""
		if [[ $1 == */* ]]; then rd=${1%/*}; fi
		season_of_dir "$rd"
		ds=$SEASON_OF_DIR
		ep_pair "$have" "${ds:-1}"
		pair=$EP_PAIR
		ep_pair "$2" "${ds:-1}"
		if [[ $pair != "$EP_PAIR" ]]; then
			warn "$SHOW_DIR/$1: kept its hint \"ep: $have\"; Kizuna's numbering is $2. Change that line by hand if it's wrong"
		fi
		KEPT=$((KEPT + 1))
		return
	fi
	if [[ $1 =~ (^|/)[[:space:]] ]]; then lead_space "$1"; fi # one already hinted: plan_show reports it
	PLAN_REL+=("$1") PLAN_VAL+=("$2")
}

# lead_space warns about the show-relative file $1, whose name or folder
# starts with a space, when it has a hint or gets one: the line is written
# with the space, and whether Plex keeps it or trims it isn't known.
lead_space() {
	warn "$SHOW_DIR/$1: its name or folder starts with a space, and Plex may not match its ep: hint to it. Rename it without the space"
}

# main_videos sets MAIN_VIDEOS to how many of the videos ($@) are no special,
# trailer, opening, ending or other extra: a show's episodes, or its movie. It
# stops at 2.
main_videos() {
	local v stem core n=0
	for v in "$@"; do
		stem=${v##*/} stem=${stem%.*}
		strip_tags "$stem"
		core=$STRIP_TAGS
		core_number "$core"
		if plex_extra "$stem" && ! plex_numbered "$stem" && ((CORE_NUMBER == 0)); then
			continue # an extra, unless it is meant as an episode (plan_show reports it)
		fi
		core_credit "$core"
		if ! plex_numbered "$stem" && ((${#CORE_CREDIT[@]} > 0)); then continue; fi
		core_special "$core"
		if ((CORE_SPECIAL > 0)); then continue; fi
		n=$((n + 1))
		if ((n > 1)); then break; fi
	done
	MAIN_VIDEOS=$n
}

plan_show() {
	local dir=$1 sub videos=() v rel rd ds stem n abs loaded=0 season rules=$MODE ckind cnum cletter order
	local relative=0 se rs re pk pe numbered ctext mains="" byhand have plain core ep
	SHOW_DIR=$dir PLAN_REL=() PLAN_VAL=() SHOW_SKIPPED=0 ID_FILE_TAG=""
	HINT_SRC=() NAME_KEY=() FOLDER_SEASON=() VIDEO_LC=()
	for sub in "$dir"/*/; do
		sub=${sub%/} sub=${sub##*/}
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
	order=$(unsupported_order "${dir##*/}${ID_FILE_TAG:+ $ID_FILE_TAG}")
	if [[ -n $order ]]; then
		warn "$dir: $order is a HAMA numbering order Kizuna doesn't support: the tag finds the show, but its episodes follow Kizuna's numbering, not that order. Check this show's hints against your files"
	fi
	list_videos
	videos=("${LIST_VIDEOS[@]}")
	SHOW_VARIANTS=""
	shopt -s nocasematch
	if [[ ${dir##*/} =~ $PLAIN_TVDB_TAG || $ID_FILE_TAG =~ $PLAIN_TVDB_TAG ]]; then
		relative=1 # HAMA plain tvdb: seasons counted from 1
	fi
	shopt -u nocasematch
	for v in "${videos[@]}"; do
		hint_key "${v#"$dir"/}"
		VIDEO_LC[${HINT_KEY,,}]=$((${VIDEO_LC[${HINT_KEY,,}]:-0} + 1))
		file_stem "$v"
		stem=$FILE_STEM
		if plex_numbered "$stem"; then continue; fi
		strip_tags "$stem"
		core_credit "$STRIP_TAGS"
		if ((${#CORE_CREDIT[@]} > 0)) && [[ ${CORE_CREDIT[2]} != - ]]; then SHOW_VARIANTS+=" ${CORE_CREDIT[0]}"; fi
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
		file_stem "$v"
		rel=${v#"$dir"/} stem=$FILE_STEM rd=""
		if [[ $rel == */* ]]; then rd=${rel%/*}; fi
		strip_tags "$stem"
		core=$STRIP_TAGS # read once, for every number below
		user_ep "$rel"
		have=$HAVE
		if [[ -n $HAVE_AS ]]; then
			plain=$rel
			if [[ -n $HAVE_IN ]]; then plain=${rel#"$HAVE_IN/"}; fi # the name as its own .plexmatch writes it
			warn "$v: kept the hint for \"$HAVE_AS\" (ep: $have) in ${HAVE_IN:+$HAVE_IN/}$PLEXMATCH as this file's, so it gets no other: that name is written differently from the file's (./, a backslash or other capital letters), and Plex may not take the line for this file. Write it as \"$plain\" by hand"
		fi
		if [[ -n $have && $rel =~ (^|/)[[:space:]] ]]; then lead_space "$rel"; fi
		ds=""
		if [[ -n $rd ]]; then season_of_dir "$rd"; ds=$SEASON_OF_DIR; FOLDER_SEASON[$rd]=$ds; fi # a subfolder's season, if it has one
		if plex_extra "$stem"; then # an extra to Plex, like those in extras/
			# reported when it is meant as an episode or the movie
			core_number "$core"
			if ((${#videos[@]} == 1 || CORE_NUMBER > 0)) || plex_numbered "$stem"; then
				warn "$v: Plex's scanner makes it an extra, not an episode or a movie: its name ends in \"-${stem##*-}\". Rename it without that"
			fi
			continue
		fi

		# what Plex makes of the name by itself: an episode (a season and
		# episode in it), an opening/ending extra, a file it leaves out, or none
		numbered=0 pk="" pe=""
		if plex_numbered "$stem"; then
			numbered=1
		else
			plex_credit_of "$stem"
			pk=${PLEX_CREDIT_OF[0]:-} pe=${PLEX_CREDIT_OF[1]:-}
		fi
		# an opening or ending: reported, never hinted
		core_credit "$core"
		if ((!numbered && ${#CORE_CREDIT[@]} > 0)); then
			ckind=${CORE_CREDIT[0]} cnum=${CORE_CREDIT[1]} cletter=${CORE_CREDIT[2]} ctext=${CORE_CREDIT[3]}
			if [[ -z $pk ]]; then pk=plain pe=$ctext; fi # not spelled the way Plex takes it
			if [[ $rules == tvdb ]]; then # TVDB numbers its own specials; the 101+ scheme doesn't apply
				credit_note "$v" "$pk" "$pe" "" "its TVDB number (S00Exx)"
				continue
			fi
			if [[ $CREDITS_FOR != "$dir" ]]; then load_credits "$dir" "$v"; fi
			if credit_index "$ckind" "$cnum" "$cletter"; then
				printf -v ep 'S00E%02d' "$CREDIT_N"
				credit_note "$v" "$pk" "$pe" "$ep" "$CREDIT_NOTE" "$CREDIT_GUESS"
			else
				credit_note "$v" "$pk" "$pe" "" "its Kizuna number (S00E101...; ${ckind^^}$cnum${cletter#-}: $CREDIT_NOTE)"
			fi
			continue
		fi
		core_special "$core"
		n=$CORE_SPECIAL
		if ((n > 0)); then
			printf -v ep 'S00E%02d' "$n"
			if [[ -n $pk ]]; then
				if [[ $rules == tvdb ]]; then
					credit_note "$v" "$pk" "$pe" "" "its TVDB number (S00Exx)"
				else
					credit_note "$v" "$pk" "$pe" "$ep"
				fi
			elif [[ $rules == tvdb ]]; then
				warn "$v: special file - add its TVDB number by hand (ep: S00Exx: $rel)"
			else
				hint "$rel" "$ep"
			fi
			continue
		fi
		# an episode whose name Plex reads as an opening/ending
		if [[ $pk == extra ]]; then
			warn "$v: Plex's scanner makes it an opening/ending extra, not an episode: its name contains \"$pe\". Rename it without that"
		elif [[ $pk == dropped ]]; then
			warn "$v: Plex's scanner leaves it out of the library: its name contains the whole word \"$pe\". Rename it without that"
		fi
		core_number "$core"
		abs=$CORE_NUMBER
		if ((abs == 0 && !numbered)); then # no number in it, for the script or for Plex
			# counted once per show, and only for a show with such a file
			if [[ -z $mains ]]; then main_videos "${videos[@]}"; mains=$MAIN_VIDEOS; fi
			if ((mains == 1)); then # a movie: next to it only specials, trailers, openings and endings
				hint "$rel" "S01E01"
				continue
			fi
			if [[ $rules != tvdb ]]; then # tvdb rules report it below
				if [[ -n $have ]]; then
					KEPT=$((KEPT + 1)) # numbered by a hint of its own
				elif [[ -z $pk ]] && ! number_like "$stem" "$ds"; then # pk: reported above
					if [[ -n $ds ]]; then # in a season folder: no movie
						printf -v byhand 'ep: S%02dExx: %s' "$ds" "$rel"
					else
						byhand="ep: S01E01: $rel for a movie"
					fi
					warn "$v: no episode number found, and the folder has other episodes or movies, so it isn't hinted as a lone movie. If Plex's scanner skips it, rename it with its number, or add its hint by hand ($byhand)"
				fi
				continue
			fi
		fi
		# plain HAMA tvdb: S02E01 is the 1st episode of TVDB season 2 -> its absolute number
		season_episode "$stem" "$ds" "$abs"
		if [[ $rules == tvdb ]] && ((relative && ${#SEASON_EPISODE[@]} > 0)); then
			rs=${SEASON_EPISODE[0]} re=${SEASON_EPISODE[1]}
			if ((!loaded)); then
				load_seasons "$v" || { warn "$dir: no match from Kizuna at $URL; skipped"; SHOW_SKIPPED=1; return; }
				loaded=1
			fi
			printf -v se 'S%02dE%02d' "$rs" "$re"
			if [[ ${SEASON_OF[$re]:-} == "$rs" ]]; then
				NAME_KEY[$rel]=$se
				continue # already absolute (season 1 always is)
			fi
			abs=${ABS_OF["$rs $re"]:-}
			if [[ -z $abs ]]; then
				warn "$v: $se: TheTVDB's season $rs of $SHOW_KEY has no episode $re (and $re isn't one of its absolute numbers)"
				continue
			fi
			printf -v ep 'S%02dE%02d' "$rs" "$abs"
			hint "$rel" "$ep" "$se"
			continue
		fi
		shopt -s nocasematch
		if [[ $rules != tvdb || -n $ds || $stem =~ $SXE ]]; then
			# a bare number in a season folder: Plex reads it with the folder's
			# season (a season and episode in the name, same_numbers reads itself)
			if [[ -n $ds ]] && ((abs > 0)) && [[ ! $stem =~ $SXE ]]; then
				printf -v se 'S%02dE%02d' "$ds" "$abs"
				NAME_KEY[$rel]=$se
			fi
			shopt -u nocasematch
			continue # Plex's scanner reads these fine
		fi
		shopt -u nocasematch
		if ((abs == 0)) && [[ -n $have ]]; then
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
		if ((season == ${ROOT_SEASON:-1})) && [[ -z $have ]]; then
			printf -v se 'S%02dE%02d' "$season" "$abs"
			NAME_KEY[$rel]=$se
			continue # the scanner already reads "Show - 05" in the show folder as S01E05
		fi
		printf -v ep 'S%02dE%02d' "$season" "$abs"
		hint "$rel" "$ep"
	done
	same_numbers "${videos[@]}"
}

# same_numbers warns about each episode number that two or more of the
# show's videos ($@) end up on, when a hint planned here is one of them: Plex
# shows the files of one number as one episode. They can be one (its two
# parts), so the hints stay planned. A file counts with its planned hint, else
# with the hint it already has, else with the number Plex reads from its name
# or folder ("Show - S00E01", "Specials/Show - 01"). A range hint
# (S00E04-E05) counts with its first number only. Not counted: a file Plex
# makes an extra ("Show-trailer") or leaves out (an SPxx hint), whatever its
# hint says.
# Files are one episode on purpose, so there is no warning, when their numbers
# were read from the same season and episode ("Show - S02E01" and
# "Season 02/Show - 01" in a plain [tvdb-N] folder), or when they are in one
# folder and their names differ only in tags, version or extension
# ("Show - SP3" and "Show - SP3v2 [1080p]").
# It reads what plan_show recorded (HINT_SRC, NAME_KEY, FOLDER_SEASON) and
# runs no subshell: it costs a show without such files next to nothing.
same_numbers() {
	local v rel stem key have kind s e i n list first differ same
	local -A planned=() members=() fresh=()
	local order=() rels=() kinds=()
	((${#PLAN_REL[@]} > 0)) || return 0
	for i in "${!PLAN_REL[@]}"; do planned[${PLAN_REL[i]}]=${PLAN_VAL[i]}; done
	n=0
	for v in "$@"; do
		rel=${v#"$SHOW_DIR"/} stem=${rel##*/} stem=${stem%.*}
		key=${planned[$rel]:-} kind=planned
		user_ep "$rel" # the hint the file has, read as plan_show and hint read it
		have=$HAVE
		if [[ -n $key ]]; then
			fresh[$key]=1
		elif plex_extra "$stem"; then
			continue
		elif [[ -n $have ]]; then
			kind=hinted have=${have%%-*} # a range: its first number
			if [[ $have =~ ^[sS][pP][0-9] ]]; then continue; fi
			if [[ $have =~ ^[sS]([0-9]+)[eE]([0-9]+)$ ]]; then
				s=${BASH_REMATCH[1]} e=${BASH_REMATCH[2]}
			elif [[ $have =~ ^[eE]?([0-9]+)$ ]]; then # a bare number: the folder's season
				s=${ROOT_SEASON:-1} e=${BASH_REMATCH[1]}
				if [[ $rel == */* ]]; then s=${FOLDER_SEASON[${rel%/*}]:-1}; fi
			else
				continue
			fi
			printf -v key 'S%02dE%02d' "$((10#$s))" "$((10#$e))"
		else
			kind=name key=${NAME_KEY[$rel]:-}
			if [[ -z $key ]]; then
				shopt -s nocasematch
				if [[ $stem =~ $SXE ]]; then
					printf -v key 'S%02dE%02d' "$((10#${BASH_REMATCH[2]}))" "$((10#${BASH_REMATCH[3]}))"
				fi
				shopt -u nocasematch
				[[ -n $key ]] || continue
			fi
		fi
		if [[ -z ${members[$key]:-} ]]; then order+=("$key"); fi
		members[$key]+="$n "
		rels+=("$rel") kinds+=("$kind")
		n=$((n + 1))
	done
	for key in "${order[@]}"; do
		if [[ -z ${fresh[$key]:-} || ${members[$key]} != *" "*" "* ]]; then continue; fi # planned here, and two files or more
		first="" differ=0 list=""
		for i in ${members[$key]}; do
			rel=${rels[i]}
			# what makes it the same episode as the others: the season and
			# episode its number was read from, else its folder and its name
			# without tags and version
			same=${HINT_SRC[$rel]:-}
			if [[ -z $same && ${kinds[i]} == name ]]; then same=$key; fi
			if [[ -z $same ]]; then
				stem=${rel##*/} stem=${stem%.*}
				strip_tags "$stem"
				stem=${STRIP_TAGS,,}
				while [[ $stem =~ ^(.*[0-9])v[0-9]($|[^[:alnum:]].*$) ]]; do stem=${BASH_REMATCH[1]}${BASH_REMATCH[2]}; done
				same=/$stem
				if [[ $rel == */* ]]; then same=${rel%/*}$same; fi
			fi
			if [[ -z $list ]]; then first=$same; elif [[ $same != "$first" ]]; then differ=1; fi
			list+="${list:+, }\"$rel\""
			case ${kinds[i]} in
			hinted) list+=" (already hinted)" ;;
			name) list+=" (numbered by its name)" ;;
			esac
		done
		if ((differ)); then
			warn "$SHOW_DIR: more than one file is on $key, and Plex shows them as one episode: $list. If they are different episodes, give each its own number: rename the file, or set its ep: line in the show's .plexmatch by hand"
		fi
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
	WRITING=$tmp # for on_signal, before the temp file exists
	# a copy of the old file first, for its permissions (and owner, as root)
	if [[ -f $pm ]]; then cp -p "$pm" "$tmp" 2>/dev/null || :; fi
	if ! printf '%s' "$out" >"$tmp" || ! mv -f "$tmp" "$pm"; then
		rm -f "$tmp" 2>/dev/null || :
		WRITING=""
		die "can't write $pm (full disk? permissions?): it was left as it was"
	fi
	WRITING=""
}

WRITING="" # the temp file add_hints is writing, if any

# on_signal is the trap for an interrupt (Ctrl+C) or a kill while the
# .plexmatch files are written: it deletes the temp file being written, if
# any (the old .plexmatch is still whole), then ends the script by the same
# signal ($1), so that its caller sees why. A variable set per file, and a
# trap set once: nothing runs per file that wasn't already there.
on_signal() {
	if [[ -n $WRITING ]]; then rm -f -- "$WRITING" 2>/dev/null || :; fi
	WRITING=""
	trap - "$1"
	kill -s "$1" "$BASHPID"
	# a signal ignored when the script started can't be reset: end it anyway
	case $1 in
	HUP) exit 129 ;;
	INT) exit 130 ;;
	*) exit 143 ;;
	esac
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
		trap 'on_signal HUP' HUP
		trap 'on_signal INT' INT
		trap 'on_signal TERM' TERM
		for d in "${CHANGED[@]}"; do add_hints "$d/$PLEXMATCH" "${NEW_HINTS[$d]}"; done
		trap - HUP INT TERM
		echo "wrote ${#CHANGED[@]} .plexmatch files. The new ep: lines are plain lines like your own: the next run"
		echo "keeps them, and to change or remove one, edit the .plexmatch by hand."
	else
		echo "dry run: nothing was changed. Check every hint and WARNING above against your files;"
		echo "the script can't know every naming style. Run again with -a only when the plan is right:"
		echo "the hints it writes aren't tracked, so the script can't change or remove them afterwards."
	fi
}

# Run main when the script is run, not when it is sourced (the tests do).
# Piped to bash -s, BASH_SOURCE is empty (unset for set -u): it is run then.
[[ ${BASH_SOURCE[0]:-$0} != "$0" ]] || main "$@"
