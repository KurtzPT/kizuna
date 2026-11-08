<div align="center">

# 絆 Kizuna

**An anime metadata provider for Plex that keeps your absolute episode numbers.**

*Kizuna* means "bond". It ties AniDB, TheTVDB, TheMovieDb, MyAnimeList and Anime News Network together
into one Plex provider.

![Plex Media Server 1.43+](https://img.shields.io/badge/Plex_Media_Server-1.43%2B-e5a00d?style=flat-square&logo=plex&logoColor=white)
![Go](https://img.shields.io/badge/Go-stdlib_only-00add8?style=flat-square&logo=go&logoColor=white)
![Docker image](https://img.shields.io/badge/Docker-~10_MB-2496ed?style=flat-square&logo=docker&logoColor=white)
![amd64 · arm64](https://img.shields.io/badge/arch-amd64_·_arm64-555?style=flat-square)
[![License: GPL-3.0](https://img.shields.io/badge/license-GPL--3.0-blue?style=flat-square)](LICENSE)
<br>
[![ci](https://github.com/KurtzPT/kizuna/actions/workflows/ci.yml/badge.svg)](https://github.com/KurtzPT/kizuna/actions/workflows/ci.yml)
[![docker](https://github.com/KurtzPT/kizuna/actions/workflows/docker.yml/badge.svg)](https://github.com/KurtzPT/kizuna/actions/workflows/docker.yml)
[![Latest release](https://img.shields.io/github/v/release/KurtzPT/kizuna?style=flat-square&label=release&sort=semver)](https://github.com/KurtzPT/kizuna/releases/latest)

[**Setup →**](SETUP.md) &nbsp;·&nbsp; [How it works](#-how-it-works) &nbsp;·&nbsp; [Modes](#%EF%B8%8F-modes) &nbsp;·&nbsp; [Matching](#-how-a-show-is-found) &nbsp;·&nbsp; [Sources](#-sources) &nbsp;·&nbsp; [Migrating from HAMA](MIGRATION.md) &nbsp;·&nbsp; [Contributing](CONTRIBUTING.md)

</div>

---

## ✨ What it is

Kizuna is a **custom metadata provider**: the new kind of Plex agent, running as a small web service next to Plex.

| | |
|---|---|
| 🔢 **Absolute numbering** | Episode 148 stays episode 148, in every mode |
| 🗂️ **Three modes** | One AniDB entry per show, one TVDB series per show, or [both in one library](#auto-both-in-one-library) |
| 🏷️ **Matches by id** | `[anidb-8550]`, `[tvdb-81797]`, `[mal-21]`… in the folder name. No guessing |
| 🔀 **Per-field sources** | Pick the source order for titles, summaries, posters, cast… separately |
| 🖼️ **Your files win** | `poster.jpg`, `fanart.jpg`, `theme.mp3` next to your files override every online source |
| 🤝 **Plays fair with AniDB** | [Rate limits, caching and ban handling](#-anidb-fair-use) built in, so your IP stays welcome |
| 🪶 **Tiny** | One static Go binary, standard library only, a ~10 MB image |

## 🧭 How it works

```mermaid
flowchart LR
    F["📁 Your files<br/>Show [anidb-23]/Show - 05.mkv"] --> S["Plex TV Series<br/>scanner"]
    S -- "show · S01E05" --> K(["絆 Kizuna"])
    K -- "titles, summaries,<br/>artwork, cast…" --> P["📺 Plex library"]
    K <--> A[("AniDB · TheTVDB · TheMovieDb<br/>MyAnimeList · Anime News Network")]
    L["🖼️ Plex Local Media<br/>poster.jpg, theme.mp3"] --> P
```

| Step | Who | Does |
|---|---|---|
| 1 | Plex's **Plex TV Series** scanner | reads your file names and `.plexmatch` hints, and sets each file's season and episode |
| 2 | **Kizuna** | gets the show name and file path from Plex, [finds the show](#-how-a-show-is-found), and describes it |
| 3 | **Plex Local Media** | adds your own artwork and theme songs on top |

Kizuna never reads your media folders and can't move an episode to another number. **The folder layout of your
[mode](#%EF%B8%8F-modes) is what puts each file in the right place.**

---

## 🗂️ Modes

Each mode is its own provider URL; a library's agent uses one of them.

| Mode | A show in Plex is… | Files look like | Pick it when |
|---|---|---|---|
| **`/anidb`** | one **AniDB entry** | `Show - 01.mkv` … `Show - 148.mkv` | every season or sequel is its own folder |
| **`/tvdb`** | one **TVDB series** | `Show - S02E26.mkv` | one folder per series, with seasons |
| **`/auto`** | either, **per folder** | both, side by side | your library mixes the two |

The same anime in both models, *Attack on Titan* (25 + 12 episodes):

```mermaid
flowchart LR
    subgraph src[" "]
        direction TB
        A1["AniDB<br/><b>Shingeki no Kyojin</b><br/>25 episodes"]
        A2["AniDB<br/><b>Shingeki no Kyojin Season 2</b><br/>12 episodes"]
    end
    subgraph anidb["/anidb → two shows"]
        direction TB
        P1["📺 Shingeki no Kyojin<br/>S01E01 … S01E25"]
        P2["📺 Shingeki no Kyojin Season 2<br/>S01E01 … S01E12"]
    end
    subgraph tvdb["/tvdb → one show"]
        direction TB
        T1["📺 Attack on Titan<br/>Season 1: E01 … E25<br/>Season 2: E26 … E37"]
    end
    A1 --> P1
    A2 --> P2
    A1 --> T1
    A2 --> T1
```

### `/anidb`: one AniDB entry per show

```text
📁 Hunter x Hunter (2011) [anidb-8550]
 ├─ Hunter x Hunter - 001.mkv        →  S01E01
 ├─ Hunter x Hunter - 148.mkv        →  S01E148
 ├─ Hunter x Hunter - S00E01.mkv     →  Specials · special 1
 └─ Hunter x Hunter - S00E101.mkv    →  Specials · opening 1
```

**Season 1** holds every regular episode, 1…n. **Season 0** holds everything else, in number blocks:

| AniDB type | Plex episodes |
|---|---|
| `S` specials | `S00E01` … |
| `C` openings | `S00E101` … |
| `C` endings | `S00E151` … |
| `T` trailers | `S00E201` … |
| `P` parodies | `S00E301` … |
| `O` other | `S00E401` … |

- **Movies** are one-episode AniDB entries: `S01E01`, with the movie's own title and summary.
- **Episode thumbnails** come from TheTVDB, since AniDB has none, through [pairing](#%EF%B8%8F-episode-titles-and-pairing).
  Missing AniDB summaries and dates are filled the same way.
- **Without AniDB details** (no client, ban, daily limit), every episode still exists as *Episode N*. A later
  refresh fills it in.

### `/tvdb`: one TVDB series per show

```text
📁 Attack on Titan [tvdb-267440]
 ├─ Attack on Titan - S01E01.mkv     →  Season 1 · episode 1
 ├─ Attack on Titan - S01E25.mkv     →  Season 1 · episode 25
 ├─ Attack on Titan - S02E26.mkv     →  Season 2 · episode 26   ← keeps counting
 └─ Attack on Titan - S02E37.mkv     →  Season 2 · episode 37
```

| | |
|---|---|
| **Seasons** | TheTVDB's. A `Season 02/` folder works, but the `S02` in the name is enough |
| **Episode numbers** | keep counting across seasons: season 1 ends at 25, so season 2 starts at **26** |
| **Specials** | season 0, with TheTVDB's numbers |
| **Episode list** | TheTVDB, or TheMovieDb when there's no TVDB key |
| **Show details** | the series' first AniDB entry |
| **Episode titles** | AniDB by default, from *each season's own* AniDB entry ([pairing](#%EF%B8%8F-episode-titles-and-pairing)) |
| **No TVDB series** | an entry without one (most movies, many OVAs) uses the `/anidb` rules |

### `/auto`: both in one library

Each show folder picks its rules:

```mermaid
flowchart LR
    D["📁 show folder"] --> Q{"id tag, or<br/>existing GUID?"}
    Q -- "[anidb-8550]<br/>[anidb2-…]" --> AN["/anidb rules"]
    Q -- "[tvdb-267440]<br/>[tvdb3-…]" --> TV["/tvdb rules"]
    Q -- "other id, or none" --> DEF["AUTO_DEFAULT<br/><i>anidb unless set</i>"]
```

```text
📁 Anime
 ├─ 📁 Hunter x Hunter (2011) [anidb-8550]     ← /anidb: Hunter x Hunter - 148.mkv
 ├─ 📁 Attack on Titan [tvdb-267440]           ← /tvdb:  Attack on Titan - S02E26.mkv
 └─ 📁 Cowboy Bebop                            ← AUTO_DEFAULT
```

> [!TIP]
> **Coming from HAMA + Absolute Series Scanner?** Kizuna supports HAMA's `anidb`, `tvdb3` and plain `tvdb` modes,
> without renaming files. [MIGRATION.md](MIGRATION.md) has a script that writes the `.plexmatch` hints Plex's
> scanner needs.

## 🔍 How a show is found

The first piece of evidence that names a show wins:

| | Evidence | Example |
|---|---|---|
| 1 | **An id tag** in the folder or file name | `[anidb-69]` `[tvdb-81797]` `[tmdb-37854]` `[imdb-tt0388629]` `[mal-21]` `[anilist-21]` `[kitsu-12]` `[ann-13]` (`{…}` works too) |
| 2 | **The GUID Plex already has** from another agent | `com.plexapp.agents.thetvdb://81797`, a HAMA `anidb-69` GUID, `imdb://tt0388629` |
| 3 | **AniDB title search** in the offline title list (~1 ms, no API call) | `Hunter x Hunter` + year 2011 → *Hunter x Hunter (2011)*, not the 1999 series |

Ids from other sites become an AniDB entry through the [id mapping](#-id-mapping). In `/anidb`, an id that covers
several AniDB entries (a TVDB series with one entry per season) gives the series' first entry.

---

## 📚 Sources

| Source | Best at | Key |
|---|---|---|
| [**AniDB**](https://anidb.net) | titles in every language, summaries, episodes and specials, cast with characters, staff | free HTTP client |
| [**TheTVDB**](https://thetvdb.com) | `/tvdb` seasons and episodes, episode thumbnails, artwork | API key (v4, or a legacy key) |
| [**TheMovieDb**](https://www.themoviedb.org) | posters, backgrounds, logos; `/tvdb` episodes without a TVDB key | API key |
| [**MyAnimeList**](https://myanimelist.net) | summary, poster, genres, studio, rating, date | client ID |
| [**Anime News Network**](https://www.animenewsnetwork.com/encyclopedia/) | summary, genres, Japanese cast, staff, studio, date | none |

Every field has its own source order, and any source can come first. Later sources are only downloaded when the ones
before them have nothing. By default AniDB comes first for show details, and TheMovieDb for artwork. Posters,
backgrounds and logos from every listed source are offered in Plex's *Edit* dialog.

**Setup:** [which keys each mode needs and what each source can supply](SETUP.md#-1-get-your-keys) ·
[changing the order per field](SETUP.md#-sources-per-field).

### 🏷️ Episode titles and pairing

To give an episode a title from another source, Kizuna first finds that episode on the other source: it **pairs**
them through the [id mapping](#-id-mapping).

```mermaid
flowchart LR
    T["📺 TVDB S02E05<br/><i>(absolute 30)</i>"] --> M{"mapping"}
    M -- "per-episode exception?" --> X["use it<br/><i>recaps, split seasons, specials</i>"]
    M -- "else" --> D["entry's TVDB season + offset"]
    X --> A["AniDB <b>Season 2</b> entry · episode 5"]
    D --> A
    A --> L["title in EPISODE_LANGS<br/><i>en → x-jat</i>"]
```

| | |
|---|---|
| **Pairing** | Anime-Lists' per-episode exceptions first, else each AniDB entry's TVDB season and episode offset. TheMovieDb is paired at the same season and number as TheTVDB |
| **Picking the title** | `EPISODE_TITLES` sets the order (default AniDB → TheTVDB → TheMovieDb), in both modes. The first **real** title wins: *Episode 12* or *TBA* don't count |
| **AniDB languages** | `EPISODE_LANGS` (default English, then romaji). With none of those, the next source is tried; AniDB's title in any language is the last resort |
| **In `/tvdb`** | a show spans several AniDB entries. Uncached ones are fetched in the background, within AniDB's limits, so their titles appear on a later refresh |

### 🔗 Id mapping

Two community lists connect AniDB to every other site. Both are downloaded daily; when they disagree, the first list
in `MAPPING_SOURCES` wins:

| List | Adds |
|---|---|
| [Anime-Lists](https://github.com/Anime-Lists/anime-lists) | TVDB, TMDB and IMDb ids, plus each entry's TVDB season, episode offset and per-episode exceptions |
| [Fribb/anime-lists](https://github.com/Fribb/anime-lists) | MyAnimeList, AniList, Kitsu and ANN ids, plus extra IMDb ids |

Wrong mapping? Correct it yourself with `anime-list-custom.xml` ([setup](SETUP.md#-fixing-a-wrong-mapping)).

### 🤝 AniDB fair use

AniDB bans clients that ask too much, so these rules are built in. No setting changes them:

| Rule | |
|---|---|
| Matching never calls the API | it searches the daily title list |
| One call every 4 s, at most 200 a day | AniDB's limits |
| Every answer is cached | 6 days while airing and for 60 days after the end, then 180 days |
| A failed anime waits 24 h | before it's asked again |
| Backoff after errors | 30 s → 2 min → 15 min → 1 h → 6 h → 24 h |
| A ban pauses all calls for 24 h | more traffic would only extend it |

The state survives restarts (`anidb-state.json` in the data folder). While AniDB can't be asked, shows use cached
AniDB data, then the other sources.

---

<div align="center">

**[Set it up →](SETUP.md)** &nbsp;·&nbsp; [Contribute](CONTRIBUTING.md)

<sub>Metadata from <a href="https://anidb.net">AniDB</a>, <a href="https://thetvdb.com">TheTVDB</a>,
<a href="https://www.themoviedb.org">TheMovieDb</a>, <a href="https://myanimelist.net">MyAnimeList</a> and the
<a href="https://www.animenewsnetwork.com/encyclopedia/">Anime News Network encyclopedia</a>. Id mappings from
<a href="https://github.com/Anime-Lists/anime-lists">Anime-Lists</a> and
<a href="https://github.com/Fribb/anime-lists">Fribb/anime-lists</a>.</sub>

<sub>Copyright © 2026 KurtzPT · Kizuna is free software under the <a href="LICENSE">GNU General Public License v3.0</a>:
you may use, change and share it, and anything you distribute that's built on it must stay under the same license.
It comes with no warranty.</sub>

</div>
