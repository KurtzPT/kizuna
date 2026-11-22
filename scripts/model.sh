#!/usr/bin/env bash
# Checks the TLA+ model of AniDB's call lifecycle (model/AniDBCalls.tla) with
# TLC. The configs in model/ (today's code) must pass. Every config in
# model/replay (a past bug brought back), model/defence (one of today's
# defences dropped) and model/findings (a bug not fixed yet) must fail on the
# invariant its "expect:" line names, and differ from model/AniDBCalls.cfg
# only in the constants its "switches:" line names. A config that passes, or
# fails another way, means the model no longer catches that bug.
#
#   bash scripts/model.sh                  every config, one line each
#   bash scripts/model.sh replay/ee1ebd7   one config (under model/, no .cfg),
#                                          with TLC's whole output and trace
#
# Needs java (8 or later: TLC 1.7.4's classes target Java 8) and curl. TLC is
# downloaded once, checked against its SHA-256, and kept in $KIZUNA_CACHE
# (default ${XDG_CACHE_HOME:-~/.cache}/kizuna).
set -euo pipefail

tla_version=1.7.4
tla_sha256=936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88

root=$(cd "$(dirname "$0")/.." && pwd)
model=$root/model
cache=${KIZUNA_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/kizuna}
jar=$cache/tla2tools-$tla_version.jar

command -v java >/dev/null || { echo "model: java is needed (8 or later)" >&2; exit 1; }

sha256() {
  if command -v sha256sum >/dev/null; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

tmp=$(mktemp -d)
part=
trap 'rm -rf "$tmp" ${part:+"$part"}' EXIT

if [ ! -f "$jar" ] || [ "$(sha256 "$jar")" != "$tla_sha256" ]; then
  command -v curl >/dev/null || { echo "model: curl is needed to download TLC" >&2; exit 1; }
  mkdir -p "$cache"
  echo "model: downloading TLC $tla_version"
  # A name of its own, next to the jar: two runs at once (two worktrees)
  # never write the same file, and the mv into place is atomic.
  part=$(mktemp "$jar.XXXXXX")
  curl -fsSL --retry 3 -o "$part" \
    "https://github.com/tlaplus/tlaplus/releases/download/v$tla_version/tla2tools.jar"
  got=$(sha256 "$part")
  if [ "$got" != "$tla_sha256" ]; then
    echo "model: tla2tools.jar has SHA-256 $got, want $tla_sha256" >&2
    exit 1
  fi
  mv "$part" "$jar"
  part=
fi

# tlc <config> <output file>: TLC's exit code (0 pass, 11 deadlock, 12 an
# invariant broken, anything else: TLC could not check the model).
tlc() {
  local ec=0
  (cd "$model" && java -XX:+UseParallelGC -XX:ParallelGCThreads=2 -cp "$jar" tlc2.TLC -workers 2 \
    -metadir "$tmp/states-${1//\//-}" -config "$1.cfg" AniDBCalls.tla) >"$2" 2>&1 || ec=$?
  return "$ec"
}

# summary <output file>: the states TLC found and how long it took.
summary() {
  local states took
  states=$(grep -Eo '[0-9,]+ distinct states found' "$1" | tail -n1 || true)
  took=$(grep -Eo 'Finished in [^ ]+' "$1" | tail -n1 || true)
  echo "${states:-no state count}, ${took:-no time}"
}

# header <config> <key>: the config's "\* key: ..." line.
header() { sed -n "s/^\\\\\\* $2: //p" "$model/$1.cfg"; }

# changed <config>: the constants it sets otherwise than AniDBCalls.cfg,
# sorted, one line; "?" for any other line that differs.
changed() {
  { diff <(grep -v '^\\\*' "$model/AniDBCalls.cfg") <(grep -v '^\\\*' "$model/$1.cfg") || true; } |
    awk '/^[<>]/ { sub(/^[<>] +/, ""); if ($0 ~ /^[A-Za-z]+ = /) print $1; else print "?" }' |
    sort -u | xargs
}

if [ $# -gt 0 ]; then
  out=$tmp/out
  ec=0
  tlc "$1" "$out" || ec=$?
  cat "$out"
  exit "$ec"
fi

failed=0
check() {
  local cfg=$1 expect want got out ec=0
  out=$tmp/${cfg//\//-}.out
  if [ "${cfg#*/}" != "$cfg" ]; then
    expect=$(header "$cfg" expect)
    if [ -z "$expect" ]; then
      echo "FAIL  $cfg: needs an \"\\* expect:\" line"
      failed=1
      return
    fi
    want=$(header "$cfg" switches | xargs -n1 | sort -u | xargs)
    got=$(changed "$cfg")
    if [ -z "$want" ] || [ "$got" != "$want" ]; then
      echo "FAIL  $cfg: its \"\\* switches:\" line says \"$want\", it differs from AniDBCalls.cfg in \"$got\""
      failed=1
      return
    fi
  fi
  tlc "$cfg" "$out" || ec=$?
  if [ "${cfg#*/}" = "$cfg" ]; then
    if [ "$ec" -eq 0 ] && grep -q 'No error has been found' "$out"; then
      echo "pass  $cfg ($(summary "$out"))"
      return
    fi
    echo "FAIL  $cfg: today's code breaks the model (exit $ec); its trace:"
  else
    want=12
    case $expect in *Deadlock*) want=11 ;; esac
    if [ "$ec" -eq "$want" ] && grep -Eq "$expect" "$out"; then
      echo "fails $cfg as expected: $(grep -Eo "$expect" "$out" | head -n1) ($(summary "$out"))"
      return
    fi
    echo "FAIL  $cfg: want \"$expect\" (exit $want), got exit $ec; TLC's output:"
  fi
  cat "$out"
  failed=1
}

for f in "$model"/*.cfg "$model"/replay/*.cfg "$model"/defence/*.cfg "$model"/findings/*.cfg; do
  [ -e "$f" ] || continue
  f=${f#"$model"/}
  check "${f%.cfg}"
done
echo "model: done in ${SECONDS} s"
exit "$failed"
