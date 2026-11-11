<div align="center">

<a href="README.md"><img src="assets/logo.png" alt="Kizuna logo" width="72"></a>

# Kizuna · Security

[Home](README.md) &nbsp;·&nbsp; [Setup](SETUP.md) &nbsp;·&nbsp; [Guide](GUIDE.md) &nbsp;·&nbsp; [Settings](SETTINGS.md) &nbsp;·&nbsp; [Mapping](MAPPING.md) &nbsp;·&nbsp; [Migration](MIGRATION.md)

<sub>On this page: [Report](#-reporting-a-vulnerability) · [What counts](#-what-counts-as-a-vulnerability) · [Versions](#-supported-versions) · [How Kizuna is protected](#%EF%B8%8F-how-kizuna-is-protected) · [Your data](#-your-data)</sub>

</div>

---

## 📮 Reporting a vulnerability

> [!CAUTION]
> **Never open a public issue for a security problem.** Report it privately.

1. Open the repository's **Security** tab and click **Report a vulnerability**.
2. Describe the problem, how to trigger it, and your Kizuna version (`curl http://<host>:32600/health` shows it).

Once a fix is released, the report can be published, with credit to you if you want it.

## 🎯 What counts as a vulnerability

Kizuna has **no login, by design** ([why](#%EF%B8%8F-how-kizuna-is-protected)): anyone it lets in can query it.

| | Report it |
|---|---|
| ✅ | an API key, AniDB client name or Plex token in a log, an answer or an error page |
| ✅ | a request from outside `ALLOW_FROM`, or for a host name outside `ALLOWED_HOSTS`, that gets through |
| ✅ | a request that makes Kizuna read or write files outside its data folder |
| ✅ | a request that crashes Kizuna, or makes it use unbounded memory or disk |
| ✅ | a way to make Kizuna break AniDB's rate limits (that gets *your* IP banned) |
| ✅ | the migration script touching anything but the `.plexmatch` files in the library you gave it |
| ❌ | "anyone on my LAN can query it": that's the design |
| ❌ | anything that needs Kizuna reachable from the internet (port forward, public reverse proxy): unsupported |

## 📦 Supported versions

| Version | Security fixes |
|---|---|
| **Latest release** (`:latest`) | ✅ as a patch release (`1.2.3` → `1.2.4`) |
| Older releases | ❌ update to the latest |
| Betas (`:1.2.3-beta.1`), `:dev` | ❌ not supported, though they usually get the fix first |

> [!NOTE]
> There is no stable release yet. Until v0.1.0, fixes go into the next beta.

> [!TIP]
> Update with `docker compose pull && docker compose up -d`. All tags: [SETUP.md](SETUP.md#-2-run-kizuna).

## 🛡️ How Kizuna is protected

Plex can't send credentials to a custom metadata provider, so Kizuna can't ask for a login. Instead it limits who
may talk to it, and what it exposes:

| Protection | Stops | Where |
|---|---|---|
| **Private clients only** | an accidental port forward: a request from a public address gets `403`. Kizuna checks the connection's own address, never `X-Forwarded-For`, which a client can fake | [`ALLOW_FROM`](SETTINGS.md#%EF%B8%8F-access), default `private` |
| **Known host names only** | DNS rebinding: a web page that points its own domain at your LAN to reach Kizuna through your browser. Kizuna doesn't know that name, so it refuses | [`ALLOWED_HOSTS`](SETTINGS.md#%EF%B8%8F-access) |
| **Secrets hidden** | API keys, the AniDB client name and Plex tokens never appear in answers or logs | built in |
| **Locked-down container** | non-root user, read-only filesystem, no Linux capabilities, no privilege gain, no shell in the image | `compose.yml`, Unraid template, `Dockerfile` |
| **AniDB fair use** | a burst of requests breaking AniDB's limits and getting your IP banned | built in, [fixed](GUIDE.md#-anidb-fair-use) |
| **Checked code** | GitHub Actions pinned to exact commits; CodeQL and `govulncheck` on every change, `govulncheck` weekly on the published images; Dependabot updates | `.github/workflows` |
| **Signed images** | a tampered or look-alike image: every published image has a signed build provenance attestation | `docker.yml` |

> [!NOTE]
> **Behind a reverse proxy**, every request comes from the proxy's address, so `ALLOW_FROM` can't tell clients
> apart. The host name check still refuses a public domain you haven't listed.

> [!WARNING]
> These checks reduce the risk; they don't make Kizuna safe on the internet. Keep port `32600` on your local
> network: **never** forward it or put it behind a public reverse proxy.

## 🔒 Your data

> [!IMPORTANT]
> **What Kizuna handles belongs to you alone.** It has no telemetry and sends nothing to its developers or anyone
> else. Your settings, API keys, cache, logs and match reports stay in your data folder.

Kizuna only connects to:

| Where | Why | What it sends |
|---|---|---|
| AniDB, TheTVDB, TheMovieDb, MyAnimeList, Anime News Network | to look up the shows in your library | the show's id or title, and that service's own key or client name, never another's |
| Your Plex server (`PLEX_URL`), only if you set it | to name openings and endings | your `PLEX_TOKEN`, only to that server |
| GitHub | to download the public id mapping and AniDB title lists | nothing about you or your library |
