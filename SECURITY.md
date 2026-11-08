<div align="center">

<a href="README.md"><img src="assets/logo.png" alt="Kizuna logo" width="72"></a>

# Kizuna · Security

[📮 Report](#-reporting-a-vulnerability) &nbsp;·&nbsp; [🎯 What counts](#-what-counts-as-a-vulnerability) &nbsp;·&nbsp; [📦 Versions](#-supported-versions) &nbsp;·&nbsp; [🛡️ How Kizuna is protected](#%EF%B8%8F-how-kizuna-is-protected)

</div>

## 📮 Reporting a vulnerability

**Don't open a public issue.** Report it privately:

1. Open the repository's **Security** tab.
2. Click **Report a vulnerability** (GitHub's private vulnerability reporting).
3. Describe the problem, how to trigger it, and your Kizuna version (`curl http://<host>:32600/health` shows it).

Once a fix is released, the report can be published, with credit to you if you want it.

## 🎯 What counts as a vulnerability

Kizuna has **no login, by design** (see [below](#%EF%B8%8F-how-kizuna-is-protected)). Anyone it lets in can query it.

| | Report it |
|---|---|
| ✅ | an API key, AniDB client name or Plex token in a log, an answer or an error page |
| ✅ | a request from outside `ALLOW_FROM`, or for a host name outside `ALLOWED_HOSTS`, that gets through |
| ✅ | a request that makes Kizuna read or write files outside its data folder |
| ✅ | a request that crashes Kizuna, or makes it use unbounded memory or disk |
| ✅ | a way to make Kizuna break AniDB's rate limits (that gets *your* IP banned) |
| ✅ | the migration script touching any file other than a `.plexmatch` in a show folder of the library you gave it |
| ❌ | "anyone on my LAN can query it": that's the design |
| ❌ | a problem that needs Kizuna's port forwarded to the internet, or behind a public reverse proxy: that's unsupported |

## 📦 Supported versions

Only the **latest release** gets security fixes.

| Image tag | Is |
|---|---|
| `:latest` | the newest release: security fixes ship here as a patch release (`1.2.3` → `1.2.4`) |
| `:1.2.3`, `:1.2` | a release; `:1.2` moves to each new patch of 1.2 |
| `:dev`, `:1.2.3-beta.1` | a beta: not supported, though it usually gets the fix first |

Update with:

```bash
docker compose pull && docker compose up -d
```

Check that an image was built by Kizuna's own workflow from this repository (needs the [GitHub CLI](https://cli.github.com)):

```bash
gh attestation verify oci://ghcr.io/kurtzpt/kizuna:latest -R KurtzPT/kizuna
```

## 🛡️ How Kizuna is protected

Plex can't send credentials to a custom metadata provider, so Kizuna can't ask for a login. Instead it limits who
may talk to it, and what it exposes:

| Protection | Stops | Where |
|---|---|---|
| **Private clients only** | an accidental port forward: requests from public addresses get `403` | `ALLOW_FROM`, default `private` |
| **Known host names only** | DNS rebinding: a web page reaching Kizuna through your browser | `ALLOWED_HOSTS` |
| **Secrets hidden** | API keys, the AniDB client name and Plex tokens never appear in answers; logs show them as `***` | built in |
| **Locked-down container** | non-root user, read-only filesystem, no Linux capabilities, no privilege gain, no shell in the image | `compose.yml` |
| **AniDB fair use** | a burst of requests can't break AniDB's limits and get your IP banned | built in, [fixed](README.md#-anidb-fair-use) |
| **Pinned CI** | GitHub Actions run at exact commits, never a tag that can be moved | `.github/workflows` |
| **Signed images** | a tampered or look-alike image: every published image has a signed build provenance attestation | `docker.yml`, check below |

How the two access checks work, and how to change them: [SETUP.md → Access](SETUP.md#%EF%B8%8F-access).

> [!WARNING]
> These reduce the risk; they don't make Kizuna safe on the internet. Keep port `32600` on your local network.
> **Never** forward it or put it behind a public reverse proxy.
