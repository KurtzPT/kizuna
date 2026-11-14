<div align="center">

<a href="README.md"><img src="assets/logo.png" alt="Kizuna logo" width="72"></a>

# Kizuna · Settings

**Every setting, its default and what it changes.**

[Home](README.md) &nbsp;·&nbsp; [Setup](SETUP.md) &nbsp;·&nbsp; [Guide](GUIDE.md) &nbsp;·&nbsp; **Settings** &nbsp;·&nbsp; [Mapping](MAPPING.md) &nbsp;·&nbsp; [Migration](MIGRATION.md)

<sub>Settings: [Keys](#-accounts--keys) · [Matching](#-matching--titles) · [Sources](#-sources-per-field) · [Openings](#-opening-and-ending-titles) · [Logging](#-logging) · [Cache](#-storage--cache) · [Server](#%EF%B8%8F-server) · [Access](#%EF%B8%8F-access)</sub>

<sub>More: [The settings file](#-where-settings-go) · [How sources are used](#-how-sources-are-used) · [Cache cleanup](#-cache-cleanup) · [Wrong values](#%EF%B8%8F-when-a-value-is-wrong)</sub>

</div>

---

## 📄 Where settings go

In `kizuna.env` in the data folder: [SETUP step 2](SETUP.md#-2-run-kizuna) creates it and says how to change it.
[`.env.sample`](.env.sample) lists every setting with its default. Settings apply to every mode and every library.

- **One `NAME=value` per line, names in upper case.** Other names are ignored with a warning.
- **A `#` after a space starts a comment.** Put a value in quotes to keep it exactly as written.
- **To keep the file somewhere else,** set `KIZUNA_CONFIG=/path/to/file` in the environment. Kizuna won't start if
  that file is missing.

---

## ⚙️ All settings

> [!WARNING]
> - **`PLEX_TOKEN` gives full access to your Plex server.** [Keep the settings file private](SETUP.md#changing-settings).
> - **In Docker, leave `PORT`, `LISTEN` and `DATA_DIR` alone.** Change the port mapping's *host* side and the
>   volume instead.
> - **`ALLOW_FROM=any` lets every address use Kizuna,** which has no login.
> - **The legacy TheTVDB key gives less** ([footnotes](#which-source-answers)), and TheTVDB may switch it off at any
>   time.

### 🔑 Accounts & keys

Where to get each key: [SETUP step 1](SETUP.md#-1-get-your-keys). A key left empty turns its source off.

| Variable | Default | What it does |
|---|---|---|
| `ANIDB_CLIENT` | – | Your registered AniDB client name. Needed for `/anidb` |
| `ANIDB_CLIENTVER` | `1` | The client version you registered |
| `TVDB_API_KEY` | – | TheTVDB v4 key. Needed for `/tvdb` |
| `TVDB_PIN` | – | Your TheTVDB subscriber PIN, for a free key. A company key works without it |
| `TVDB_LEGACY_API_KEY` | – | An older TheTVDB key. Used when `TVDB_API_KEY` is empty or rejected |
| `TMDB_API_KEY` | – | TheMovieDb v3 key |
| `MAL_CLIENT_ID` | – | MyAnimeList Client ID |
| `PLEX_TOKEN` | – | A [Plex token](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/) of the server's owner ([where it goes](SECURITY.md#-your-data)). Turns on the [`plex` source](GUIDE.md#-plex-as-a-source) and, with `PLEX_URL`, the [opening and ending titles](#-opening-and-ending-titles) |

### 🎯 Matching & titles

| Variable | Default | What it does |
|---|---|---|
| `MIN_SCORE` | `80` | Lowest title-search score, 0–100, for an automatic match. Plex's *Fix Match* list ignores it |
| `AUTO_DEFAULT` | `anidb` | `anidb` or `tvdb`: the rules `/auto` uses for a show without an AniDB or TVDB tag. If that mode isn't set up, the other is used |
| `MAPPING_SOURCES` | `animelists,fribb` | [Id mapping](README.md#-id-mapping) lists, in order. A later list only fills ids the earlier ones lack. Can't be `none` |
| `TITLE_LANGS` | `main,x-jat,en` | Show title language order. `main` is AniDB's main title; the rest are AniDB language codes: `x-jat` (romaji), `en`, `ja`, `de`… |
| `TITLE_OFFICIAL_ONLY` | `false` | `true`: only AniDB's main and official titles are used, never synonyms |
| `ORIGINAL_TITLE_LANGS` | `ja` | Language order for Plex's *original title*. Left empty when it equals the title; `none` turns it off |
| `EPISODE_LANGS` | `en,x-jat` | AniDB episode title language order |

### 🔀 Sources per field

Each list names the sources to ask, in order: `anidb`, `tvdb`, `tmdb`, `mal`, `plex`, `ann`, from the ones the
field allows. The first that has a value wins; `none` turns the field off ([more](#-how-sources-are-used)).

- **A source you list nowhere is not asked at all,** even with its key set. The exceptions are AniDB and TheTVDB,
  where Kizuna needs them to find a show's episodes.
- **`plex` is last in the default lists,** so by default it only fills what the others leave empty.

| Variable | Default | What it does |
|---|---|---|
| `TITLE_SOURCES` | `anidb,tvdb,tmdb,plex,ann` | Show title. AniDB's follows `TITLE_LANGS` |
| `SUMMARY_SOURCES` | `anidb,tvdb,tmdb,mal,plex,ann` | Summary |
| `GENRE_SOURCES` | `anidb,tvdb,tmdb,mal,plex,ann` | Genres |
| `CAST_SOURCES` | `anidb,tvdb,tmdb,plex,ann` | Voice actors with their characters. ANN: Japanese cast, no photos |
| `CREW_SOURCES` | `anidb,tvdb,tmdb,plex,ann` | The show's directors and writers. Episodes have [their own list](#episode-directors-and-writers) |
| `STUDIO_SOURCES` | `anidb,tvdb,tmdb,mal,plex,ann` | Studio |
| `DATE_SOURCES` | `anidb,tvdb,tmdb,mal,plex,ann` | First aired or release date |
| `RATING_SOURCES` | `anidb,tvdb,tmdb,mal,plex,ann` | Content rating. AniDB only marks 18+, as `X`; MAL uses US ratings |
| `RATING_COUNTRY` | `US` | Rating country order, e.g. `DE` or `GB,US`. Non-US ratings are written the Plex way: `de/12` |
| `POSTER_SOURCES` | `tvdb,tmdb,anidb,mal,plex,ann` | Posters |
| `BACKGROUND_SOURCES` | `tvdb,tmdb,plex` | Backgrounds |
| `LOGO_SOURCES` | `tvdb,tmdb,plex` | Clear logos |
| `ART_ALL_SOURCES` | `false` | `true`: images from every listed source, to pick from in Plex's *Edit* dialog; the first source's best is the default. `false`: only the first source that has any |
| `SEASON_ART_FALLBACK` | `true` | A season without art of a kind gets that site's series art. `false`: the next source is asked instead ([season art](#season-art)) |
| `EPISODE_TITLES` | `anidb,tvdb,tmdb,plex` | Episode titles. A real title beats a placeholder such as *Episode 12* or *TBA*. `tvdb,tmdb,anidb` puts TheTVDB's English titles first |
| `EPISODE_FILL_SOURCES` | `tvdb,plex` | `/anidb` only: episode thumbnails, plus the summaries, dates and runtimes AniDB lacks. `tvdb`, `plex` or `none` |
| `EPISODE_CREW_SOURCES` | `tvdb,tmdb,plex` | [Episode directors and writers](#episode-directors-and-writers). `tvdb`, `tmdb`, `plex` or `none` |

### 🎵 Opening and ending titles

Set both to give Plex's opening and ending extras readable titles, such as *Opening 1* instead of `OP1`
([how it works](GUIDE.md#readable-titles)).

| Variable | Default | What it does |
|---|---|---|
| `PLEX_URL` | – | Your Plex server's address as Kizuna reaches it: `http://192.168.1.10:32400`, or `http://plex:32400` on a shared Docker network. Nothing after the port |
| `PLEX_TOKEN` | – | See [Accounts & keys](#-accounts--keys) |

### 📝 Logging

| Variable | Default | What it does |
|---|---|---|
| `LOG_LEVEL` | `info` | `info` or `debug` ([what each logs](GUIDE.md#-logs)) |
| `LOG_RETENTION_DAYS` | `7` | Days of old log files kept. `0` keeps them forever |
| `TZ` | UTC (image) · system time zone (binary) | Time zone for log timestamps, e.g. `Europe/London` |
| `MATCH_REPORTS` | `false` | `true`: write a [match report](GUIDE.md#-match-reports) per show to `reports/` in the data folder |

### 💾 Storage & cache

| Variable | Default | What it does |
|---|---|---|
| `CACHE_MIN_FREE` | `1GB` | When the cache's disk has less free space than this, the oldest cache files are deleted ([cleanup](#-cache-cleanup)). `0` never cleans up |
| `CACHE_FREE_TARGET` | `2GB` | …until this much is free again. Sizes like `500MB`, `2GB`, `1.5TB`; a plain number is MB |

### 🖥️ Server

For the binary or host networking. In Docker, leave them alone ([why](#%EF%B8%8F-all-settings)).

| Variable | Default | What it does |
|---|---|---|
| `PORT` | `32600` | Port Kizuna listens on |
| `LISTEN` | – | Full listen address, e.g. `127.0.0.1:32600` for local connections only. Overrides `PORT` |
| `DATA_DIR` | `/data` (image) · `./data` (binary) | Where the cache, logs, match reports and mapping corrections go |

### 🛡️ Access

Two checks limit who may talk to Kizuna ([why](SECURITY.md#%EF%B8%8F-how-kizuna-is-protected)). A refused request
gets `403` and a log line naming the setting to change. The defaults fit Plex and Kizuna on the same LAN, Docker
host or Tailscale network.

| Variable | Default | What it does |
|---|---|---|
| `ALLOW_FROM` | `private` | Addresses that may use Kizuna, comma-separated: `private` (home and Docker networks, Tailscale), single IPs such as `192.168.1.10`, networks such as `192.168.1.0/24`. `any` lets everyone in. The machine itself is always allowed |
| `ALLOWED_HOSTS` | – | Extra names Kizuna may be reached by: `kizuna.example.com`, or `.example.com` for every name under it. `any` turns the check off. Always allowed: IP addresses, `localhost`, one-word names, and names ending in `.local`, `.lan`, `.home`, `.home.arpa`, `.internal`, `.localdomain` |

---

## 📖 How sources are used

### Which source answers

For each field, Kizuna goes through the list in order and uses the first source that has a value. A source without
its key is skipped.

| Field | AniDB | TheTVDB | TheMovieDb | MyAnimeList | Plex | ANN |
|---|:-:|:-:|:-:|:-:|:-:|:-:|
| Title | ⭐ | ✅ | ✅ | | ✅ | ✅ |
| Summary | ⭐ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Genres | ⭐ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Cast | ⭐ | ✅ | ✅ | | ✅ | ✅ |
| Crew | ⭐ | ✅¹ | ✅ | | ✅ | ✅ |
| Studio | ⭐ | ✅¹ | ✅ | ✅ | ✅ | ✅ |
| Content rating | ⭐ | ✅² | ✅ | ✅ | ✅ | ✅ |
| Date | ⭐ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Poster | ✅ | ⭐ | ✅ | ✅ | ✅ | ✅ |
| Background, logo | | ⭐³ | ✅ | | ✅ | |
| Episode list, `/tvdb` | | ⭐ only | | | | |
| Episode titles | ⭐ | ✅ | ✅ | | ✅ | |
| Episode thumbnails, `/anidb` | | ⭐ | | | ✅ | |
| Episode directors, writers | | ⭐ | ✅ | | ✅ | |

<sub>⭐ first by default &nbsp;·&nbsp; ✅ can be used &nbsp;·&nbsp; with a legacy TheTVDB key: ¹ none, ² US rating
only, ³ backgrounds only, no logos; no season summaries or season backgrounds, and episodes get the series'
runtime</sub>

> [!NOTE]
> **A list where no source is set up leaves its field empty.** If you set that list yourself, Kizuna warns at
> startup: set it to `none` if you want it empty.

### Later entries in `/anidb`

In `/anidb` each AniDB entry is its own show, but TheTVDB, TheMovieDb and Plex describe the whole series. So their
data only goes to the entry that starts the series; the other entries take the next source in the list.

| `/anidb` show | TheTVDB / TheMovieDb give its… |
|---|---|
| **First entry**: *Shingeki no Kyojin* | title, summary, genres, cast, crew, studio, rating, date |
| **Later entry**: *Shingeki no Kyojin Season 2* | summary only: that of its season, when the [id mapping](README.md#-id-mapping) gives one |
| **Movie or OVA** among the series' specials | nothing from TheTVDB; TheMovieDb's movie data when it has the movie |

Which entry starts a series follows the mapping, per site: [the rule](MAPPING.md#-how-episodes-are-paired).

### Episode directors and writers

`EPISODE_CREW_SOURCES` decides where they come from: by default TheTVDB, then TheMovieDb, then Plex. Directors and
writers are picked separately, so an episode can get its directors from one source and its writers from another.

- **A source you leave out gives none,** whatever the other lists say.
- **Put a source first to prefer it:** `tmdb,tvdb`.
- **`none`:** no episode directors or writers.

> [!TIP]
> With a v4 TheTVDB key, some episodes of a long show may lack them after its first scan: refresh the show a few
> minutes later. A list without `tvdb` avoids that wait.

### Image languages

- **TheMovieDb:** English, Japanese or text-free art only.
- **TheTVDB:** those languages first, best rated first, then the others. Clear logos: English or text-free only.
- **Banners** aren't used: Plex has no place for them.

### Season art

A show and each of its seasons get the art of the season they stand for, where the site has it: TheTVDB season
posters and backgrounds, TheMovieDb season posters, Plex season posters, backgrounds and logos. AniDB, MyAnimeList
and ANN have one poster per entry.

| Plex item | TheTVDB / TheMovieDb art of |
|---|---|
| `/anidb` show that starts its series | the series, then season 1 |
| `/anidb` show, a later entry (*Shingeki no Kyojin Season 3*) | the season the [id mapping](MAPPING.md) puts it in |
| `/anidb` show, a movie or special among the series' specials | season 0 (TheMovieDb: the movie's own art) |
| `/anidb` show in absolute order, or not mapped to a season | the series |
| `/anidb` *Season 1* / *Specials* | the entry's season, as above / season 0 |
| `/tvdb` show | the series |
| `/tvdb` *Season N* / *Specials* | TheTVDB's season N / 0; TheMovieDb's season with the same episodes, else its series |

With `ART_ALL_SOURCES=true`, each source gives its season's art, then its series' (with `SEASON_ART_FALLBACK`),
before the next source. A [match report](GUIDE.md#-match-reports) shows what came from where.

---

## 🧹 Cache cleanup

The cache has no size limit of its own. When free space drops below `CACHE_MIN_FREE`, Kizuna deletes cache files
until `CACHE_FREE_TARGET` is free, in this order:

1. **TheTVDB, TheMovieDb, MyAnimeList, Plex, ANN:** oldest first. Quick to download again.
2. **AniDB anime:** oldest first. Slow to get back: AniDB allows 200 a day.

The AniDB title list and the mapping lists are never deleted. If the target still isn't reached, something else is
filling the disk, and Kizuna logs a warning.

| Cached | For |
|---|---|
| AniDB anime | see [AniDB fair use](GUIDE.md#-anidb-fair-use) |
| TheTVDB · TheMovieDb · Plex | 2–7 days |
| MyAnimeList | 30 days |
| Anime News Network | 7 days |
| AniDB title list · mapping lists | refreshed daily |

> [!TIP]
> When a source is down, Kizuna answers from its old cached copy: old data beats none.

## ⚠️ When a value is wrong

Kizuna warns at startup, quoting the value as written:

| You set | Kizuna |
|---|---|
| a variable to nothing | uses the default. For a key that means its source is off |
| a source the field doesn't allow, or a typo such as `TITLE_SOURCES=anidb,tvbd` | drops it. A list with nothing left uses the default; only `none` turns a field off |
| a value it can't read or that is out of range, such as `TITLE_OFFICIAL_ONLY=yes` or `MIN_SCORE=150` | uses the default. On/off settings take `true` or `false`; sizes take `MB`, `GB`, `TB` |
| a bad `TZ` | keeps the default time zone |
| a bad `ALLOW_FROM` or `ALLOWED_HOSTS` entry | ignores it. An `ALLOW_FROM` with nothing valid left falls back to `private` |
| an unknown language or country code | no warning: it never matches, so the next one is used |
| neither `ANIDB_CLIENT` nor a TheTVDB key, or a port already in use | **doesn't start** ([troubleshooting](SETUP.md#kizuna-doesnt-start)) |
