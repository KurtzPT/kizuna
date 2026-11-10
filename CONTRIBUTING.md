<div align="center">

<a href="README.md"><img src="assets/logo.png" alt="Kizuna logo" width="72"></a>

# Kizuna · Contributing

**Bug fixes, mapping fixes, new features and docs are all welcome.**

[🐛 Issues](#-reporting-a-problem) &nbsp;·&nbsp; [🛠️ Setup](#%EF%B8%8F-development-setup) &nbsp;·&nbsp; [📐 Rules](#-ground-rules) &nbsp;·&nbsp; [✅ Checks](#-checks) &nbsp;·&nbsp; [🔀 Pull requests](#-pull-requests) &nbsp;·&nbsp; [🗺️ Code map](#%EF%B8%8F-code-map)

</div>

---

## 🐛 Reporting a problem

Not writing code? Open an [issue](https://github.com/KurtzPT/kizuna/issues/new/choose) and pick the template that
fits. **Check the [troubleshooting table](SETUP.md#-troubleshooting) first.** Each form asks for the folder and file
names, your mode and a `LOG_LEVEL=debug` excerpt for that one show ([Logs](SETUP.md#-logs)). Keys and Plex tokens are
already `***` in the logs, but remove any paths you don't want public.

> [!IMPORTANT]
> **Security problems never go in a public issue.** Follow [SECURITY.md](SECURITY.md).

> [!TIP]
> **A show maps to the wrong TVDB season or episode?** The mapping usually comes from
> [Anime-Lists](https://github.com/Anime-Lists/anime-lists), not from Kizuna's code. Fix it for yourself right away
> with [`anime-list-custom.xml`](MAPPING.md), and send the fix upstream so it's fixed for
> everyone.

## 🛠️ Development setup

**You need:** Go (the version in [`go.mod`](go.mod) or newer). Docker and
[ShellCheck](https://www.shellcheck.net) are optional.

Fork the repository, then work from the **`dev`** branch, where all new work lands:

```bash
git clone -b dev https://github.com/<you>/kizuna.git
cd kizuna
go test ./...
go build -o kizuna ./cmd/kizuna
```

Run it locally against a scratch data folder (all settings can be environment variables, see
[SETTINGS.md](SETTINGS.md)):

```bash
DATA_DIR=./data ANIDB_CLIENT=<your client> TVDB_API_KEY=... TMDB_API_KEY=... ./kizuna
curl http://localhost:32600/health
```

Or build the image the way CI does (the build runs the tests first):

```bash
docker build -t kizuna .
```

> [!WARNING]
> **Mind AniDB while developing.** Use your own registered client name, and don't loop refreshes or bulk-scan a big
> library against AniDB: the [fair-use limits](README.md#-anidb-fair-use) are per IP, and a ban hits your Plex too.
> The tests never call a real service, so `go test` is always safe.

## 📐 Ground rules

| | Rule | Why |
|---|---|---|
| 🪶 | **Standard library only.** No new modules in `go.mod` | one static binary, a ~10 MB image, nothing to audit |
| 🤝 | **Don't touch the AniDB fair-use limits** in `internal/sources/anidb_fairuse.go` | they're fixed on purpose: breaking them gets users' IPs banned |
| 🌐 | **Every outside request goes through `internal/fetch`** | one place for caching, throttling, redaction and disk cleanup |
| 🔒 | **Never log or return a secret.** New keys or tokens must be redacted by `fetch.Redact` | logs get pasted into public issues |
| 🛡️ | **Keep the access checks** (`ALLOW_FROM`, `ALLOWED_HOSTS`) on every route | Kizuna has no login, see [SECURITY.md](SECURITY.md) |
| 🧪 | **Tests stay offline**: fixtures and `httptest`, never a real API | CI and contributors have no keys |
| 📖 | **Docs and code change together.** Update the doc that describes what you change ([where](#%EF%B8%8F-code-map)) | the docs are the only manual |
| 🔢 | **Leave the Go version alone.** Upgrades to `go.mod` and the `Dockerfile` are automated | keeps your pull request to one change |

The `anime-list-custom.xml` examples in [MAPPING.md](MAPPING.md) are checked by
`internal/mapping/docs_examples_test.go`. If you change one, change the other.

**Style:** `gofmt`, small packages under `internal/`, comments that say *why*. Docs follow the existing files: short
sentences, tables over long paragraphs, examples with real anime.

## ✅ Checks

CI ([`ci.yml`](.github/workflows/ci.yml)) runs these on every pull request. Run them before you push:

```bash
gofmt -l .                       # must print nothing (fix with gofmt -w .)
go vet ./...
GOOS=windows go vet ./...        # Kizuna also builds for Windows and macOS
GOOS=darwin go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
go test -race -count=1 ./...
go test -count=1 -run '^TestReportsOffAllocs$' ./internal/provider   # allocation budgets (skipped under -race)
for f in scripts/*.sh; do bash -n "$f"; done
shellcheck --severity=warning scripts/*.sh
bash scripts/kizuna-migrate_test.sh   # the migration script's behaviour tests (offline)
bash scripts/ghcr-cleanup_test.sh     # the image cleanup script's (offline, needs jq)
```

CI also fuzzes the parsers for a few seconds each (the `Fuzz*` tests; their seeds run with `go test`), runs
[actionlint](https://github.com/rhysd/actionlint) and [zizmor](https://docs.zizmor.sh) on the workflows, and CodeQL.
A fuzz crash prints the input that caused it: add it under `testdata/fuzz/<FuzzName>/` with the fix, so it stays a
test. Fuzzing uses every CPU core: `-parallel=2` keeps a desktop usable.

The [`docker`](.github/workflows/docker.yml) workflow also builds the `amd64` and `arm64` images for your pull request,
without publishing anything.

## 🔀 Pull requests

1. **Something big?** Open an issue first, so the idea can be agreed on before you write it.
2. Branch from **`dev`** and open the pull request **against `dev`**, not `main`. `main` only holds released
   versions. Keep it to **one change**, and add or update tests for it.
3. Run the [checks](#-checks).
4. Write the pull request **title as a short summary** and the **body as the why**. Pull requests are squash-merged,
   so the title and body become the commit.
5. Link the issue it fixes (`Fixes #123`).

By contributing, you agree that your work is released under the [GPL-3.0](LICENSE), like the rest of Kizuna.

## 🗺️ Code map

```text
cmd/kizuna/main.go              start-up, list refresh, "kizuna health" (Docker healthcheck)
internal/config                 settings: environment variables and the settings file
internal/fetch                  HTTP + disk cache + throttling + free-space cleanup (every source uses it)
internal/logfile                daily log file, old days deleted
internal/anidb                  offline AniDB title list: search and title-language picking
internal/mapping                AniDB ⇄ TVDB/TMDB/IMDb/MAL/AniList/Kitsu/ANN ids (Anime-Lists + Fribb)
internal/sources                AniDB HTTP API (fair-use rules), TheTVDB v4 (+ legacy v3), TheMovieDb, MyAnimeList, ANN
internal/provider               modes, matching, show model, Plex JSON, HTTP routes
internal/report                 match reports (MATCH_REPORTS): an HTML page per show, written in batches
scripts/kizuna-migrate.sh       .plexmatch hints for existing libraries (not part of the image)
scripts/kizuna-migrate_test.sh  its behaviour tests: sources it, runs it on a fixture library
scripts/ghcr-cleanup.sh         deletes old image builds from the registry (run by a workflow)
```

| To add… | Start in |
|---|---|
| a setting | `internal/config`, then [`.env.sample`](.env.sample) (a test checks that it lists every setting) and [SETTINGS.md](SETTINGS.md) |
| a matching or metadata rule | `internal/provider`, then [README.md](README.md) (how it works) or [SETUP.md](SETUP.md) / [SETTINGS.md](SETTINGS.md) (what a user sees or sets) |
| a metadata source | `internal/sources`, then the per-field source order in `internal/provider/fields.go` |
| an id prefix like `[anidb-…]` | `internal/provider/match.go` |
| something to the match reports | what is recorded: `internal/provider/report.go`; the page: `internal/report/page.html` |
| a migration rule | `scripts/kizuna-migrate.sh`, then [MIGRATION.md](MIGRATION.md) |
