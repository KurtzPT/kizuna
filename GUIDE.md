<div align="center">

<a href="README.md"><img src="assets/logo.png" alt="Kizuna logo" width="72"></a>

# Kizuna · Guide

**What Kizuna does with your library once it runs, and what to do when a show looks wrong.**

[Home](README.md) &nbsp;·&nbsp; [Setup](SETUP.md) &nbsp;·&nbsp; **Guide** &nbsp;·&nbsp; [Settings](SETTINGS.md) &nbsp;·&nbsp; [Mapping](MAPPING.md) &nbsp;·&nbsp; [Migration](MIGRATION.md)

<sub>On this page: [Matching](#-matching-in-detail) · [Movies](#-movies-and-specials) · [Plex](#-plex-as-a-source) · [Openings](#-openings-and-endings) · [AniDB](#-anidb-fair-use) · [Logs](#-logs) · [Reports](#-match-reports) · [Problems](#-metadata-problems)</sub>

</div>

---

## 🔍 Matching in detail

The README says [how a show is found](README.md#-how-a-show-is-found): an id tag, then the GUID Plex already has,
then the title search. The finer points:

| Case | What happens |
|---|---|
| Two different id tags, or in `/auto` a tag for a mode that is off | no match and a warning. The GUID and the title aren't tried |
| A `[tmdb-…]` tag or `tmdb://` GUID | read as a TheMovieDb **TV show** id. It's a movie id only when no entry has that number as a TV show; when both exist, the title search decides, else the TV show wins |
| `/anidb`, an id that covers several entries (a TVDB series with one entry per season) | the series' first entry, by that site's own order. Movies and specials only when nothing else matches |
| *Fix Match* on a show with no match because of its tags | still lists the results for the title you type (not for a season or an episode) |

> [!WARNING]
> **A mistyped id tag is skipped without a warning.** When Kizuna can't resolve a tag (a typo, or an id the
> [id mapping](README.md#-id-mapping) doesn't have), the GUID and then the title decide, so the show may match by
> title instead.

## 🎬 Movies and specials

TheTVDB often files an anime's movies, OVAs and specials among a series' specials: One Piece Film Red is S00E46 of
One Piece. Kizuna makes each of them **its own show, in every mode**, with the `/anidb` rules: the movie is
`S01E01`.

> [!TIP]
> **The sure way to get a movie:** tag its folder with the movie's AniDB id, `[anidb-16983]`.

| You have | You get |
|---|---|
| a folder tagged `[anidb-16983]` | that movie |
| a folder tagged `[tmdb-900667]` (the movie's TheMovieDb id) | that movie, unless the number is also a TheMovieDb TV show ([above](#-matching-in-detail)) |
| an untagged folder, `One Piece Film Red` | the movie, found by its title |
| a folder tagged with the series, `[tvdb-81797]` (or another agent's TheTVDB GUID) | the whole series, unless the name clearly names one of its movies: then that movie, with the warning `matched to an AniDB entry by name inside a TheTVDB id; may be wrong` |
| `One Piece - S00E46.mkv` inside the series' `/tvdb` folder | a special of the series |

<sub>*Clearly* means at least [`MIN_SCORE`](SETTINGS.md#-matching--titles), and 10 points ahead of the series' and the other movies' titles. A
year in the name, such as `(2022)`, or the one Plex sends tells two close movies apart.</sub>

- **Without AniDB details**, such a show still gets its episodes from TheTVDB's specials.
- **GUIDs:** a movie (an entry with a TheMovieDb movie id, and no regular TVDB season or no TheMovieDb TV show id)
  gets the movie's `tmdb://` and `imdb://`, not the series' `tvdb://`.

## 📺 Plex as a source

Kizuna can also use the metadata Plex's own *Plex Series* agent shows, as the source `plex`. By default it comes
after the other sources, so it fills what they leave empty: often logos, backgrounds, cast photos and episode
thumbnails.

- **Turn it on:** set [`PLEX_TOKEN`](SETTINGS.md#-accounts--keys). Nothing else is needed.
- **Change what it fills, or turn it off:** move `plex` up in a source list to prefer it, or take it out
  ([Sources per field](SETTINGS.md#-sources-per-field)).
- **A series needs a TheTVDB id** in the [id mapping](README.md#-id-mapping), and a movie a TheMovieDb or IMDb id:
  a show without one gets nothing from Plex.
- **`Plex rejected the request: check PLEX_TOKEN`** in the log means the token is wrong or no longer valid.

> [!CAUTION]
> **Plex doesn't document this service.** Kizuna uses it the way Plex's own agent does, but Plex can change it or
> turn it off at any time. Then the `plex` source just gives nothing, and the other sources answer as before.

## 🎵 Openings and endings

Plex's scanner decides by itself which files are openings and endings, from the file name alone, before Kizuna or a
[`.plexmatch`](MIGRATION.md#-how-it-works) hint is asked (seen in Plex Media Server 1.43.5):

| The name has, as a separate word | Example | Plex makes the file |
|---|---|---|
| `OP`, `ED`, `OPED`, with or without `NC` | `OP1` `NCED2` `ED_03` `NCOP` | an opening or ending **extra** |
| `Creditless` or `Clean` + `Opening`, `Ending`, `OP`, `ED` | `Creditless Opening` `Clean Ending 2` | an opening or ending **extra** |
| `Opening` or `Ending` alone | `Opening 1` `NCOpening` | ❌ **nothing**: the file is left out of the library |
| the word in small letters | `op1` `ncop` | a normal file, no extra |
| a season and episode | `Show - S00E101.mkv` | an episode: a special with its AniDB title |

An **extra** plays from the show's page (*Extras*). It has no episode number and nothing from Kizuna. To have an
opening as a special instead, name it by its number ([the numbers](MIGRATION.md#openings-and-endings-with-variants)).

> [!WARNING]
> The first such word counts, wherever it is: `Show - 05 [ED fix].mkv` becomes an ending extra too.

> [!IMPORTANT]
> Plex adds and renames extras only on **Refresh Metadata**, not on *Scan Library Files*.

### Readable titles

Set `PLEX_URL` and `PLEX_TOKEN` ([how](SETTINGS.md#-opening-and-ending-titles)), then run **Refresh All Metadata** once.
Kizuna renames the opening and ending extras of the shows it matched through Plex's API (your files are never
touched):

| File | Plex's title | Kizuna's title |
|---|---|---|
| `OP01.mkv` · `OP 1 a.mkv` | `OP01` · `OP 1 a` | **Opening 1** · **Opening 1a** |
| `OP1v2.mkv` | `OP1` | **Opening 1** |
| `NCED2.mkv` · `Clean Ending 2.mkv` | `NCED2` · `Clean Ending 2` | **Ending 2** |
| `NCOP.mkv` · `OPED.mkv` | `NCOP` · `OPED` | **Opening** · **Opening & Ending** |
| `NCED1.mkv` next to `ED1.mkv` | `NCED1` | **Ending 1 (creditless)** |

> [!NOTE]
> Plex resets the titles on every refresh, so Kizuna sets them again **about 15 seconds after each show's
> refresh** (and once more 2 minutes later). Until then you see Plex's own title.

Each rename logs `plex: extra renamed`. Titles stuck as `OP1`? See [problems](#episodes-and-details).

## 🤝 AniDB fair use

> [!WARNING]
> AniDB bans clients that ask too much, and **a ban covers your whole IP**.

Kizuna keeps to these limits, and no setting changes them:

| What | Limit |
|---|---|
| **Finding a show** | never calls the API: the daily title list is searched offline |
| **Pace** | one call every 4 s, at most 200 a day (background fetches stop at 150) |
| **Cache** | 6 days while airing and for 60 days after, then 180 days |
| **One anime** | asked at most once a day |
| **Failures** | a growing pause for every call: 30 s, 2 min, 15 min… up to 24 h |
| **A ban** | every call paused for 24 h |
| **A rejected `ANIDB_CLIENT` / `ANIDB_CLIENTVER`** | every call paused for 24 h, or until a restart with changed values |

The limits survive restarts: they are kept in `anidb-state.json` in the data folder. While AniDB can't be asked,
cached data and the other sources fill in.

## 📜 Logs

Logs go to `docker logs kizuna` and to `logs/kizuna.log` in the data folder, one `key=value` line per event. Each
day's log is then kept as `kizuna-YYYY-MM-DD.log` for [`LOG_RETENTION_DAYS`](SETTINGS.md#-logging). Keys and
tokens are [never in them](SECURITY.md#%EF%B8%8F-how-kizuna-is-protected).

| [`LOG_LEVEL`](SETTINGS.md#-logging) | Logs |
|---|---|
| `info` (default) | the settings in effect as the `ready` line, the lists loaded, one `match` line per match with how it was decided, one `show built` line per show, and failures: rejected keys, AniDB pauses and bans |
| `debug` | also every Plex request, outgoing call and cache decision, the title-search scores, and which source gave each field |

- **`show built … complete=false`**: something was missing (AniDB details, part of an episode list, a source that
  failed). Kizuna keeps that show for only 2 minutes, so a refresh after that builds it again.
- The same `match:` warning is logged at most once every 10 minutes.

## 📊 Match reports

Set [**`MATCH_REPORTS=true`**](SETTINGS.md#-logging), then open **`reports/index.html`** in the data folder in a browser. No server is
needed. The index lists every show, newest first, and the titles that matched nothing, filtered by title, id, mode
or status.

A show's page covers its last 5 builds (the episode and art lists only for the newest):

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

- A title that matched nothing gets `unmatched-<title>-<hash>.html`, with the closest candidates and how to fix it.
  It goes once the title matches, or after 30 days without a request for it.
- Delete the `reports/` folder to start over. Turning reports off leaves it as it is.

> [!TIP]
> **Reporting a bug?** *Copy for a bug report* gives the selected build as Markdown for the issue, without folders,
> request URLs, summaries, cast or image addresses.

## 🧰 Metadata problems

Kizuna won't start, or Plex can't reach it: [SETUP, troubleshooting](SETUP.md#-troubleshooting). Quoted messages are
from Kizuna's [log](#-logs) unless marked *Plex*.

### Matching

| Symptom | Fix |
|---|---|
| A show doesn't match | `match: nothing found` says why in `how=`; its [unmatched report](#-match-reports) lists the closest titles. Add an id tag to the folder: `Show [anidb-1234]` |
| Wrong show (a remake, a sequel) | Add an id tag, or use *Fix Match* in Plex |
| An id tag seems ignored | Kizuna doesn't know that id (a typo?) and matched by title ([why](#-matching-in-detail)) |
| A new anime isn't found by its AniDB id | It's unknown until the next daily title list |
| A movie folder became the whole series | Tag it `[anidb-…]` instead of `[tvdb-…]` ([movies and specials](#-movies-and-specials)) |
| `match: more than one id tag; only one is allowed` | Keep one tag in the folder and file names |
| `/auto`: `match: id tag needs /tvdb, which is off`, or the same for `/anidb` | Set up that mode's key ([keys](SETUP.md#-1-get-your-keys)), or change the tag |
| `a request for a mode that is off` | The library's provider URL is for a mode without its key: set the key, or use another mode |

### Episodes and details

| Symptom | Fix |
|---|---|
| No summaries, only *Episode N* titles | AniDB isn't set up, as the startup log's `ANIDB_CLIENT not set` says, or can't be asked yet: `AniDB details not available` says why. A later *Refresh Metadata* fills them in ([AniDB's limits](#-anidb-fair-use)) |
| `/tvdb` show has no episodes | `show build failed` with `TheTVDB has no episodes for this show`: use `/anidb` for it |
| Episodes in the wrong season | Plex's scanner sets seasons from file names and `.plexmatch` hints, not Kizuna: see [MIGRATION.md](MIGRATION.md)'s script |
| Episodes paired with the wrong TVDB episode | [Fix the mapping](MAPPING.md) |
| Openings and endings keep Plex's titles, such as `OP1` | Check the startup warning about `PLEX_URL` / `PLEX_TOKEN`, or `can't set the opening and ending titles in Plex` / `Plex doesn't answer`. Then *Refresh Metadata* ([readable titles](#readable-titles)) |

### Sources and keys

| Symptom | Fix |
|---|---|
| *Plex*: `503` `the show wasn't ready in time` / `the show's sources didn't answer in time` | A slow source (often AniDB's queue). Plex keeps what it has; the next refresh gets the show |
| *Plex*: `503` `the show can't be built now` | A source is down with nothing cached, or the show needs a key that isn't set (Kizuna logs `show build failed` with the reason). Plex asks again later |
| `TVDB rejected the API key`, `TMDB rejected the request`, `MyAnimeList rejected the request`, `Plex rejected the request` | That key (or `PLEX_TOKEN`) is wrong or expired; a user-supported TheTVDB key also needs [`TVDB_PIN`](SETTINGS.md#-accounts--keys). The source is skipped for 10 min; cached data still answers |
| `TVDB rejected TVDB_API_KEY: falling back to TVDB_LEGACY_API_KEY` | Fix `TVDB_API_KEY` or `TVDB_PIN` and restart. Until then: no TheTVDB logos, studio or show crew |
| `TVDB login failed` | TheTVDB didn't answer; not a key problem. Tried again after 30 seconds |
| `source rejected the request: check its API key / client id` | A source refused a call: check that source's key or client id |
| `AniDB rejected the client` | Use the name and version of a client registered at anidb.net (protocol HTTP), then restart |
| `AniDB call failed; backing off` | AniDB is down or unreachable. Kizuna waits longer after each failure ([limits](#-anidb-fair-use)) |
| `AniDB banned this IP` | AniDB is paused for 24 h; cached data and the other sources fill in |

**Still stuck?** [Open an issue](CONTRIBUTING.md#-reporting-a-problem).
