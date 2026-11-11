<div align="center">

<a href="README.md"><img src="assets/logo.png" alt="Kizuna logo" width="72"></a>

# Kizuna · Guide

**What Kizuna does with your library once it runs, and what to do when a show looks wrong.**

[Home](README.md) &nbsp;·&nbsp; [Setup](SETUP.md) &nbsp;·&nbsp; **Guide** &nbsp;·&nbsp; [Settings](SETTINGS.md) &nbsp;·&nbsp; [Mapping](MAPPING.md) &nbsp;·&nbsp; [Migration](MIGRATION.md)

<sub>On this page: [Movies](#-movies-and-specials) · [Openings](#-openings-and-endings) · [Matching](#-matching-in-detail) · [Logs](#-logs) · [Reports](#-match-reports) · [AniDB](#-anidb-fair-use) · [Problems](#-metadata-problems)</sub>

</div>

---

## 🎬 Movies and specials

TheTVDB often files an anime's movies, OVAs and specials among a series' specials: One Piece Film Red is S00E46 of
One Piece. Kizuna makes each of them **its own show, in every mode**, with the `/anidb` rules (the movie is
`S01E01`).

| You have | You get |
|---|---|
| a folder tagged `[anidb-16983]`, or `[tmdb-900667]` (the movie's TheMovieDb id) | that movie. **The sure way** |
| an untagged folder, `One Piece Film Red` | the movie, found by its title |
| a folder tagged with the series, `[tvdb-81797]` (or another agent's TheTVDB GUID) | the whole series, unless the name clearly names one of its movies: then that movie, with the warning `matched to an AniDB entry by name inside a TheTVDB id; may be wrong` |
| `One Piece - S00E46.mkv` inside the series' `/tvdb` folder | a special of the series |

<sub>*Clearly* means at least `MIN_SCORE`, and 10 points ahead of the series' and the other movies' titles. The
year (`(2022)` in the name, or the one Plex sends) tells two close movies apart.</sub>

- **Without AniDB details**, such a show still gets its episodes from TheTVDB's specials.
- **GUIDs:** a movie (an entry with a TheMovieDb movie id, and no regular TVDB season or no TheMovieDb TV show id)
  gets the movie's `tmdb://` and `imdb://`, not the series' `tvdb://`.

## 🎵 Openings and endings

Plex's scanner decides by itself which files are openings and endings, from the file name alone, before Kizuna or a
[`.plexmatch`](MIGRATION.md#-how-it-works) hint is asked (read from Plex Media Server 1.43.5).

| The name has, as a separate word | Example | Plex makes the file |
|---|---|---|
| `OP`, `ED`, `OPED`, with or without `NC` | `OP1` `NCED2` `ED_03` `NCOP` | an opening or ending **extra** |
| `Creditless` or `Clean` + `Opening`, `Ending`, `OP`, `ED` | `Creditless Opening` `Clean Ending 2` | an opening or ending **extra** |
| `Opening` or `Ending` alone | `Opening 1` `NCOpening` | ❌ **nothing**: the file is left out of the library |
| the word in small letters | `op1` `ncop` | a normal file, no extra |
| a season and episode | `Show - S00E101.mkv` | an episode: a special with its AniDB title |

- An **extra** plays from the show's page (*Extras*). It has no episode number and nothing from Kizuna. To have an
  opening as a special instead, name it by its number ([the numbers](MIGRATION.md#openings-and-endings-with-variants)).
- The first such word counts, wherever it is: `Show - 05 [ED fix].mkv` becomes an ending extra too.
- Plex adds and renames extras only on **Refresh Metadata**, not on *Scan Library Files*.

### Readable titles

Set [`PLEX_URL` and `PLEX_TOKEN`](SETTINGS.md#-opening-and-ending-titles) and Kizuna renames these extras through
Plex's API:

| File | Plex's title | Kizuna's title |
|---|---|---|
| `OP01.mkv` · `OP 1 a.mkv` | `OP01` · `OP 1 a` | **Opening 1** · **Opening 1a** |
| `OP1v2.mkv` | `OP1` | **Opening 1** |
| `NCED2.mkv` · `Clean Ending 2.mkv` | `NCED2` · `Clean Ending 2` | **Ending 2** |
| `NCOP.mkv` · `OPED.mkv` | `NCOP` · `OPED` | **Opening** · **Opening & Ending** |
| `NCED1.mkv` next to `ED1.mkv` | `NCED1` | **Ending 1 (creditless)** |

- Plex resets the titles on every refresh, so Kizuna sets them again **about 15 seconds after each show's refresh**
  (and once more 2 minutes later). Until then you see Plex's own title.
- After setting the two values, run **Refresh All Metadata** once.
- Only the opening and ending extras of shows Kizuna matched are renamed. Your files are never touched.
- Each rename logs `plex: extra renamed`. When Plex doesn't answer, Kizuna pauses for a minute, and those shows get
  their titles on their next refresh.

## 🔍 Matching in detail

The README says [how a show is found](README.md#-how-a-show-is-found): an id tag, then the GUID Plex already has,
then the title search. The finer points:

| Case | What happens |
|---|---|
| Two different id tags (`Show [anidb-1234]/Show [tvdb-5678] - 01.mkv`) | no match and a warning. The GUID and the title aren't tried |
| `/auto`, a tag whose mode is off (`[tvdb-…]` without a TheTVDB key) | no match, the same way |
| `/auto` with only one mode set up | every other show gets that mode's rules; a GUID of the other site is converted through the [id mapping](README.md#-id-mapping) |
| *Fix Match* on a blocked show | still lists the results for the title you type (not for a season or an episode) |
| A `[tmdb-…]` tag or `tmdb://` GUID | a TheMovieDb **TV show** id. It counts as a movie id only when no entry has that number as a TV show; when both exist, the title search decides, else the TV show wins |
| `/anidb`, an id that covers several entries (a TVDB series with one entry per season) | the series' first entry, by that site's own order. Movies and specials only when nothing else matches |
| A movie or special inside a `[tvdb-…]` tag | found only when the name clearly names it, with a warning ([movies and specials](#-movies-and-specials)) |

## 📜 Logs

Logs go to `logs/kizuna.log` in the data folder and to `docker logs kizuna`, one `key=value` line per event. A new
file starts each day, kept for [`LOG_RETENTION_DAYS`](SETTINGS.md#-logging). Keys and tokens always show as `***`.

| `LOG_LEVEL` | Logs |
|---|---|
| `info` (default) | the settings in effect (`ready`), the lists loaded, one `match` line per match with how it was decided, one `show built` line per show, and failures: rejected keys, AniDB pauses and bans |
| `debug` | also every Plex request, outgoing call and cache decision, the title-search scores, and which source gave each field |

**`show built … complete=false`** means something was missing (AniDB details, part of an episode list, a source that
failed), so the show is built again in 2 minutes. When AniDB was the missing part, the `AniDB details not available`
line before it says why (`why=`).

## 📊 Match reports

Set **`MATCH_REPORTS=true`**, then open **`reports/index.html`** in the data folder in a browser. No server is
needed. The index lists every show, newest first, and the titles that matched nothing, filtered by title, id, mode
or status.

A show's page covers its last 5 builds:

| Part | Shows |
|---|---|
| **What to check** | what looks wrong: episodes without a real title or date, a failed source, an inexact title match |
| **Match** | what Plex sent, and how Kizuna decided: the tag, the GUID, or the title-search scores |
| **Sources** | the AniDB entries behind the show, what each source offered per field, and which one Plex got |
| **Episodes** | every episode, its place on each source, and where its title came from |
| **Art** | every image sent to Plex, with its source |
| **Sent to Plex** | the whole answer, each value with its source |
| **Timeline** | every download and cache read, so a slow or failing source stands out |
| **Plex requests** | the last 50 requests about the show |

- **Copy for a bug report** gives the page as Markdown for an issue, without folders, request URLs, summaries, cast
  or image addresses.
- A title that matched nothing gets `unmatched-<title>-<hash>.html`, with the closest candidates and how to fix it.
  It is removed once the title matches, or after 30 days.
- Delete the `reports/` folder to start over. Turning reports off leaves it as it is.

## 🤝 AniDB fair use

AniDB bans clients that ask too much, and a ban covers your whole IP. Kizuna keeps to these limits, and no setting
changes them:

| | |
|---|---|
| **Finding a show** | never calls the API: the daily title list is searched offline |
| **Pace** | one call every 4 s, at most 200 a day (background fetches stop at 150) |
| **Cache** | 6 days while airing and for 60 days after, then 180 days |
| **One anime** | asked at most once a day |
| **Failures** | a growing pause for every call: 30 s, 2 min, 15 min… up to 24 h |
| **A ban** | every call paused for 24 h |
| **A rejected `ANIDB_CLIENT` / `ANIDB_CLIENTVER`** | every call paused for 24 h, or until a restart with changed values |

So a large library takes **several days** to get all its AniDB details; shows fill in on later refreshes. The limits
survive restarts (`anidb-state.json` in the data folder). While AniDB can't be asked, cached data and the other
sources fill in.

## 🧰 Metadata problems

Kizuna won't start, or Plex can't reach it: [SETUP, troubleshooting](SETUP.md#-troubleshooting).

| Symptom | Fix |
|---|---|
| A show doesn't match | Its [unmatched report](#-match-reports) or the `match: nothing found` log line gives the closest titles. Add an id tag to the folder: `Show [anidb-1234]` |
| Wrong show (a remake, a sequel) | Add an id tag, or use *Fix Match* in Plex |
| `match: more than one id tag; only one is allowed` | Keep one tag in the folder and file names ([why](#-matching-in-detail)) |
| `/auto`: `match: id tag needs /tvdb, which is off` (or `/anidb`) | Set up that mode's key ([keys](SETUP.md#-1-get-your-keys)), or change the tag |
| A movie folder became the whole series | Tag it `[anidb-…]` or `[tmdb-…]` instead of `[tvdb-…]` ([movies and specials](#-movies-and-specials)) |
| No summaries, only *Episode N* titles | AniDB isn't set up, or is paused: the `AniDB details not available` line says why. A later *Refresh Metadata* fills them in ([AniDB's limits](#-anidb-fair-use)) |
| A new anime isn't found by its AniDB id | It's unknown until the next daily title list |
| `/tvdb` show has no episodes | Set a TheTVDB key. With one, `TheTVDB has no episodes for this show`: use `/anidb` for it |
| Episodes in the wrong season | Plex's scanner sets seasons from file names and `.plexmatch` hints, not Kizuna: see [MIGRATION.md](MIGRATION.md)'s script |
| Episodes paired with the wrong TVDB episode | [Fix the mapping](MAPPING.md) |
| `503` `the show wasn't ready in time` / `the show's sources didn't answer in time` | A slow source (often AniDB's queue). Plex keeps what it has; the next refresh gets the show |
| `503` `the show can't be built now` | A source is down with nothing cached, or the show needs a key that isn't set (the reason follows). Plex asks again later |
| `TVDB rejected the API key`, `TMDB rejected the request`, `MyAnimeList rejected the request` | That key is wrong or expired (a user-supported TheTVDB key also needs `TVDB_PIN`). The source is skipped for 10 min; cached data still answers |
| `source rejected the request: check its API key / client id` | A source refused a call: check that source's key or client id |
| `AniDB rejected the client` | Use the name and version of a client registered at anidb.net (protocol HTTP), then restart |
| `TVDB rejected TVDB_API_KEY: falling back to TVDB_LEGACY_API_KEY` | Fix `TVDB_API_KEY` (or `TVDB_PIN`) and restart. Until then: no TheTVDB logos, studio or show crew |

**Still stuck?** [Open an issue](CONTRIBUTING.md#-reporting-a-problem).
