<!--
Thanks for contributing! Before you start, read CONTRIBUTING.md:
https://github.com/KurtzPT/kizuna/blob/dev/CONTRIBUTING.md

- Open this pull request against `dev`, not `main`.
- It's squash-merged: the title and body become the commit.
- Write the title for users: it becomes a line in the release notes,
  e.g. "Special-only shows keep their episodes on the legacy TheTVDB key".
- Delete the sections that don't apply.
-->

## Why

<!-- The problem, and why this fixes it. Link the issue: "Fixes #123". -->

## What changed

<!-- The approach in a few lines, and anything a reviewer could easily miss. -->

## Kind of change

<!--
Tick all that apply, for the reviewer: nothing reads these boxes. The area labels (metadata, migration,
running, documentation) are added from the files you change; a maintainer sets the type label: bug,
enhancement, or internal (nothing a user can see, left out of the notes). The release notes are grouped
by label.
-->

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
Write "None" if nothing changes for them. Anything else goes into the release notes' "Breaking
changes" section.
-->

## How it was tested

<!-- New or changed tests, and anything you checked by hand: mode, show, what Plex showed. -->

## Checklist

- [ ] Targets `dev`, and is **one change**
- [ ] [Checks](https://github.com/KurtzPT/kizuna/blob/dev/CONTRIBUTING.md#-checks) pass locally
- [ ] Tests added or updated, and they stay offline
- [ ] Follows the [ground rules](https://github.com/KurtzPT/kizuna/blob/dev/CONTRIBUTING.md#-ground-rules)
- [ ] Docs updated: the page that describes the change ([where](https://github.com/KurtzPT/kizuna/blob/dev/CONTRIBUTING.md#%EF%B8%8F-code-map)), and `.env.sample` for a setting
