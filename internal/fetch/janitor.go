package fetch

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const keepFreeEvery = 10 * time.Minute

// KeepFree watches the free space of the disk holding the cache. When it
// drops below minFree bytes, the oldest cached files are deleted until target
// bytes are free again (or the cache is empty). Files that are cheap to
// download again go first; AniDB anime entries, which cost one of the 200
// daily API calls each, only when that is not enough. The title dump and
// the mapping lists are never deleted (they are rewritten daily anyway and
// matching needs them). minFree = 0 turns it off. Runs until ctx is done.
func (c *Client) KeepFree(ctx context.Context, minFree, target uint64) {
	if minFree == 0 {
		return
	}
	target = max(target, minFree)
	for {
		c.keepFree(minFree, target)
		select {
		case <-ctx.Done():
			return
		case <-time.After(keepFreeEvery):
		}
	}
}

func (c *Client) keepFree(minFree, target uint64) {
	free, err := diskFree(c.dir)
	if err != nil {
		if !os.IsNotExist(err) { // no cache folder yet: nothing to free
			slog.Warn("cache: can't read the disk's free space; cache cleanup is off for now", "dir", c.dir, "err", err)
		}
		return
	}
	if free >= minFree {
		slog.Debug("cache: free space ok", "free_mb", free>>20, "min_mb", minFree>>20)
		return
	}
	n, freed := prune(c.dir, int64(target-free))
	after, _ := diskFree(c.dir)
	attrs := []any{"free_mb_before", free >> 20, "free_mb_after", after >> 20, "min_mb", minFree >> 20,
		"target_mb", target >> 20, "files_deleted", n, "mb_deleted", freed >> 20}
	if after < target {
		slog.Warn("cache: disk still low on space after removing old cache files (something else is filling it)", attrs...)
		return
	}
	slog.Info("cache: disk low on space; removed the oldest cache files", attrs...)
}

// keep: cache files prune never deletes. Their .etag files may go: that only
// means one full download instead of a revalidation.
var keep = map[string]bool{
	"anidb/animetitles.xml":              true,
	"mapping/anime-list-master.xml":      true,
	"mapping/fribb-anime-list-mini.json": true,
}

type cacheFile struct {
	path string
	size int64
	at   time.Time
	dear bool // costly to download again (AniDB anime)
}

// prune deletes cached files under dir until at least need bytes are gone:
// cheap files before AniDB entries, the oldest (last written) first.
// Returns how many files and bytes it deleted.
func prune(dir string, need int64) (int, int64) {
	var files []cacheFile
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil || keep[rel] {
			return nil
		}
		files = append(files, cacheFile{p, info.Size(), info.ModTime(), strings.HasPrefix(rel, "anidb/anime/")})
		return nil
	})
	sort.Slice(files, func(i, j int) bool {
		if files[i].dear != files[j].dear {
			return !files[i].dear
		}
		return files[i].at.Before(files[j].at)
	})
	var n int
	var freed int64
	for _, f := range files {
		if freed >= need {
			break
		}
		if os.Remove(f.path) == nil {
			n++
			freed += f.size
		}
	}
	return n, freed
}
