<!--
Thanks for contributing! Before you start, read CONTRIBUTING.md:
https://github.com/KurtzPT/kizuna/blob/dev/CONTRIBUTING.md

- Open this pull request against `dev`, not `main`.
- It's squash-merged: the title and the "Why" section become the commit.
  Write the title as a short summary, e.g. "tvdb: pair recaps filed as specials".
- Delete the sections that don't apply.
-->

## Why

<!-- The problem, and why this fixes it. Link the issue: "Fixes #123". -->

## What changed

<!-- The approach in a few lines, and anything a reviewer could easily miss. -->

## Kind of change

<!-- Tick all that apply: the pull request is labelled from them, and the release notes are grouped by label. -->

- [ ] 🐛 Bug fix
- [ ] ✨ New feature or setting
- [ ] 📺 Matching or metadata (match, titles, seasons, artwork, specials)
- [ ] 🛠️ Migration script (`kizuna-migrate.sh`)
- [ ] 🐳 Running Kizuna (startup, health check, Plex connection, keys, AniDB limits)
- [ ] 📖 Documentation only

**Affects:** <!-- e.g. /anidb, /tvdb, /auto, all modes, migration script -->

## Before and after

<!--
For matching and metadata changes: name a real show, with its folder name or id tag,
and show what Kizuna gave before and after. A Plex screenshot, a few log lines with
LOG_LEVEL=debug, or the JSON from /<mode>/library/metadata/<id> all work. Remove keys,
tokens and private paths first.

Example: "Attack on Titan [tvdb-267440], /tvdb: S02E26 was titled 'Episode 1', now
'Beast Titan'."
-->

## Impact on existing libraries

<!--
Will users see different matches, titles or episode numbers after updating? Do they need
to refresh metadata, re-run the migration script or change a setting? Does a default change?
Write "None" if nothing changes for them.
-->

## How it was tested

<!-- New or changed tests, and anything you checked by hand: mode, show, what Plex showed. -->

## Checklist

- [ ] Targets `dev`, and is **one change**
- [ ] [Checks](https://github.com/KurtzPT/kizuna/blob/dev/CONTRIBUTING.md#-checks) pass locally
- [ ] Tests added or updated, and they stay offline
- [ ] Follows the [ground rules](https://github.com/KurtzPT/kizuna/blob/dev/CONTRIBUTING.md#-ground-rules) (standard library only, AniDB limits untouched, requests through `internal/fetch`, no secret logged)
- [ ] Docs updated: a new or changed setting in `.env.sample` and [SETTINGS.md](https://github.com/KurtzPT/kizuna/blob/dev/SETTINGS.md), a migration rule in [MIGRATION.md](https://github.com/KurtzPT/kizuna/blob/dev/MIGRATION.md)
