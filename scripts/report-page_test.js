// Tests of the match report page's script (internal/report/page.html):
//   node scripts/report-page_test.js
// Plain Node, no packages, no network. Each page kind is built the way
// internal/report/page.go builds it (the data spliced into the page as JSON
// escaped for a <script>), then the page's own script runs against a small
// stand-in for the document: enough for it to render, not a browser. What
// the page writes is checked as text.
"use strict";
const fs = require("fs"), path = require("path"), vm = require("vm");

const root = path.join(__dirname, "..");
const template = fs.readFileSync(path.join(root, "internal", "report", "page.html"), "utf8");
const fixture = name => JSON.parse(fs.readFileSync(path.join(__dirname, "testdata", "report", name + ".json"), "utf8"));

let failed = 0, checks = 0;
function check(ok, what) {
  checks++;
  if (!ok) { failed++; console.error("FAIL: " + what); }
}

// goJSON is json.Marshal's escaping: <, >, & and U+2028/9 as \u00XX.
const goJSON = v => JSON.stringify(v).replace(/[<>&\u2028\u2029]/g, c => "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0"));

// build returns the HTML file for a page, as report.render makes it.
function build(kind, doc) {
  const slot = "/*KIZUNA_DATA*/null";
  if (!template.includes(slot)) throw new Error("page.html has no data slot");
  const head = goJSON({kind, generated: "2026-11-10T21:02:25Z", version: "1.2.3-test"});
  return template.replace(slot, () => head.slice(0, -1) + "," + goJSON(kind) + ":" + goJSON(doc) + "}");
}

// load runs a page's script and returns the page: run(code) evaluates code
// among the script's own names, html() is everything it has written. fire
// sends an event to the page's document listeners. Timers wait until flush():
// a test says when time has passed, so nothing depends on the clock.
function load(kind, doc) {
  const html = kind ? build(kind, doc) : template; // no kind: the page as it is, without data
  // The first </script> must be the script's own end: nothing in the data
  // may close it early.
  const from = html.indexOf("<script>") + "<script>".length, to = html.indexOf("</script>", from);
  const script = html.slice(from, to);
  const made = new Map();
  const element = id => {
    if (!made.has(id)) made.set(id, {
      id, innerHTML: "", textContent: "", dataset: {}, style: {}, hidden: false,
      classList: {add() {}, remove() {}, toggle() { return false; }, contains() { return false; }},
      setAttribute() {}, getAttribute() { return null; }, removeAttribute() {}, addEventListener() {}, focus() {}, select() {}, remove() {},
      querySelector() { return null; }, querySelectorAll() { return []; }, closest() { return null; },
      insertAdjacentHTML(where, text) { this.innerHTML += text; },
    });
    return made.get(id);
  };
  const app = element("app");
  const listeners = {}, timers = new Map();
  let timerId = 0;
  const document = {
    title: "", body: element("body"),
    // Only what the page has written exists, as in a browser.
    getElementById: id => id === "app" || [...made.values()].some(e => e.innerHTML.includes(`id="${id}"`)) ? element(id) : null,
    querySelector() { return null; }, querySelectorAll() { return []; }, addEventListener(type, f) { (listeners[type] ||= []).push(f); },
  };
  const setTimeout = f => { timers.set(++timerId, f); return timerId; }, clearTimeout = id => { timers.delete(id); };
  const context = vm.createContext({document, location: {hash: ""}, history: {replaceState() {}}, navigator: {}, setTimeout, clearTimeout, console});
  vm.runInContext(script, context, {filename: "page.html <script>"});
  return {
    run: code => vm.runInContext(code, context),
    html: () => [...made.values()].map(e => e.innerHTML).join("\n"),
    title: () => document.title,
    text: () => app.textContent,
    document, // a test may give it a querySelector of its own
    fire: (type, event) => (listeners[type] || []).forEach(f => f(event)),
    waiting: () => timers.size,
    flush: () => { for (const [id, f] of [...timers]) { timers.delete(id); f(); } },
    // draws(id) counts from now how often the element's HTML is replaced.
    draws: id => {
      const el = element(id);
      let v = el.innerHTML, n = 0;
      Object.defineProperty(el, "innerHTML", {get: () => v, set: x => { n++; v = x; }, configurable: true});
      return () => n;
    },
  };
}

// Every href and src the page wrote: a web address, a place in the page, or
// another report page beside it.
function checkLinks(what, html) {
  // Inside tags only: text from the data has its < escaped, so it is in none.
  const attrs = [...html.matchAll(/<[a-z][^>]*>/gi)].flatMap(tag => [...tag[0].matchAll(/\s(href|src)\s*=\s*("([^"]*)"|'([^']*)'|[^\s>]+)/gi)]);
  check(attrs.length > 0, `${what}: no href or src found at all (the check would be empty)`);
  for (const m of attrs) {
    const v = m[3] ?? m[4] ?? m[2];
    check(/^https?:\/\//i.test(v) || /^#[\w-]*$/.test(v) || /^[\w%.~-]+\.html$/.test(v), `${what}: ${m[1]}="${v}" is not http(s), an anchor or a relative .html link`);
  }
}
// Hostile text in the data shows as text: never as a tag, an attribute or a
// script address.
function checkEscaped(what, html) {
  check(!/<\/script/i.test(html), `${what}: a </script> from the data was written as markup`);
  check(!/<(b|i|u)>(x|y|z|one|Weepers)/i.test(html), `${what}: a tag from the data was written as markup`);
  check(!/<img[^>]+onerror/i.test(html), `${what}: an <img onerror> from the data was written as markup`);
  check(!/onmouseover="/i.test(html), `${what}: a quote from the data ended an attribute`);
  for (const m of html.matchAll(/<[a-z][^>]*>/gi)) check(!/javascript:/i.test(m[0]), `${what}: javascript: inside a tag: ${m[0].slice(0, 120)}`);
}
const count = (text, re) => (text.match(re) || []).length;

// ---- show page --------------------------------------------------------------
{
  const doc = fixture("show"), page = load("show", doc);
  const closed = page.html();
  check(page.title().startsWith("Lighthouse Keepers"), "show: the title is the show's");
  for (const id of ["s-checks", "s-match", "s-sources", "s-fields", "s-episodes", "s-art", "s-output", "s-trace", "s-requests"]) {
    check(closed.includes(`id="${id}"`), `show: section ${id} is missing`);
    check(closed.includes(`href="#${id}"`) || id === "s-fields", `show: nothing links to ${id}`);
  }
  check(closed.includes("up to 50 are kept"), "show: the requests section says how many are kept");
  checkLinks("show", closed);
  checkEscaped("show", closed);
  check(closed.includes("&lt;/script&gt;&lt;b&gt;x"), "show: the hostile title is shown, escaped");
  // Nothing is fetched until the reader opens an art level or an episode.
  check(!/<img/i.test(closed), "show: an <img> before anything is opened");
  check(!closed.includes("EPISODE-SUMMARY-TEXT"), "show: an episode's detail is in the page before it is opened");

  // Seasons: 5 episodes or fewer start open, every row is there.
  check(count(closed, /class="ep-n"/g) === 5, `show: ${count(closed, /class="ep-n"/g)} episode rows, want 5`);
  // The tallies of a list share their columns: every season has a cell per source.
  const tallies = [...closed.matchAll(/<span class="fold-th" title="Where this season[^>]*>(.*?)<\/span><\/button>/gs)].map(m => count(m[1], /class="th tally/g));
  check(tallies.length === 3 && tallies.every(n => n === tallies[0] && n >= 2), `show: season tallies ${tallies}, want the same cells in each of 3 seasons`);

  // Opened: every episode's detail and every art level.
  const opened = page.run(`EP.eps.map(e => episodeDetail(e)).join("") + ART.map(artBody).join("")`);
  checkLinks("show, opened", opened);
  checkEscaped("show, opened", opened);
  const imgs = [...opened.matchAll(/<img[^>]*>/g)].map(m => m[0]);
  check(imgs.length === 5, `show, opened: ${imgs.length} images, want 5 (1 thumbnail and 4 pictures with a web address)`);
  check(imgs.every(i => /\ssrc="https:\/\//.test(i) && /loading="lazy"/.test(i) && /referrerpolicy="no-referrer"/.test(i)), "show, opened: an image is not https, lazy and without a referrer");
  check(opened.includes("https://image.tmdb.org/t/p/w342/p2.jpg") && opened.includes('href="https://image.tmdb.org/t/p/original/p2.jpg"'), "show, opened: TheMovieDb's poster is shown small and linked whole");
  check(count(opened, /javascript:alert\(1\)/g) >= 2, "show, opened: the javascript: addresses are shown as text");
  check(opened.includes("EPISODE-SUMMARY-TEXT one."), "show, opened: the episode's summary");
  // An episode's producers, with their source; the episodes' crew photo count.
  const producers = page.run(`episodeDetail({s: 1, e: 1, title: "T", producers: ["PRODUCER-ONE", "<b>x"], src: {producers: "plex"}})`);
  check(/Producers.*PRODUCER-ONE, &lt;b&gt;x/s.test(producers) && producers.includes('data-s="plex"'), "show, opened: an episode's producers and their source");
  const counted = page.run(`renderEpisodes({episodes: [{s: 1, e: 1}], crewPhotos: "crew photos: 1 of 2 (plex 1)"}, true)`);
  check(counted.includes("Crew photos: 1 of 2 (plex 1)"), "show: the episodes' crew photo count");
  check(!page.run(`renderEpisodes({episodes: [{s: 1, e: 1}]}, true)`).includes("rew photos"), "show: a crew photo count without crew");
  check(count(closed + opened, /target="_blank"/g) === count(closed + opened, /target="_blank" rel="noopener noreferrer"/g), "show: a new-tab link without noopener noreferrer");

  // Sent to Plex: the threads are the recorded itemSrc, nothing guessed.
  const item = page.run("renderItem(DATA.show.builds[0])");
  const row = key => (item.match(new RegExp(`<span class="rec-k mono">${key}</span>(.*?)<div class="rec-v">`, "s")) || [])[1];
  check(/data-s="anidb"/.test(row("title")), "show item: title has AniDB's thread");
  check(/data-s="tvdb"/.test(row("summary")) && /data-s="tvdb"/.test(row("thumb")), "show item: summary and thumb have TheTVDB's thread");
  for (const key of ["ratingKey", "guid", "year", "art", "originalTitle", "duration", "Genre \\(2\\)"]) check(row(key) !== undefined && !/class="th/.test(row(key)), `show item: ${key} has a thread, but no source was recorded for it`);
  const withRows = r => page.run(`(() => { ITEM_OPEN.add("Image"); ITEM_OPEN.add("Role"); const h = renderItem(${r}); ITEM_OPEN.clear(); return h; })()`);
  const shown = withRows("DATA.show.builds[0]");
  check(count(shown, /<li class="shot" data-s="(tvdb|tmdb)">/g) === 3 && count(shown, /<li class="shot">/g) === 3 && count(shown, /source not recorded/g) === 3, "show item: 3 images with their source's colour and 3 marked as not recorded");
  check(shown.includes("“Lighthouse Keepers”") && shown.includes('<span class="ord">1</span><span class="nm">CAST-NAME One</span><span class="as">as Keeper</span>'), "show item: an image's alt and a role's order are shown");
  check(!/<img/.test(item) && /data-row="Image"/.test(item) && /data-row="Role"/.test(item) && /class="peekl" data-kind="poster" href="https:/.test(item), "show item: no picture loads before its row is opened or its link hovered");
  const pics = [...shown.matchAll(/<img[^>]*>/g)].map(m => m[0]);
  check(pics.length > 0 && pics.every(i => /\ssrc="https:\/\//.test(i) && /loading="lazy"/.test(i) && /referrerpolicy="no-referrer"/.test(i)), "show item, pictures shown: an image is not https, lazy and without a referrer");
  checkLinks("show item, pictures shown", shown);
  checkEscaped("show item, pictures shown", shown);
  // What each source offered: long values summed up, Plex's copy one click away.
  const fv = (f, v) => page.run(`fieldValue(${JSON.stringify(f)}, ${JSON.stringify(v)})`);
  check(/19 people/.test(fv("Cast", "19: Ann &lt;b&gt; as X, Bo as Y, …")) && /Ann &amp;lt;b&amp;gt; and 18 more/.test(fv("Cast", "19: Ann &lt;b&gt; as X, Bo as Y, …")), "fields: the cast is a count and the first name, escaped");
  check(fv("Crew", "Directors: D One. Writers: W One, W Two").includes("1 director, 2 writers"), "fields: the crew is counted by role");
  check(fv("Genres", "A, B, C").includes("3 genres"), "fields: the genres are counted");
  const ftab = page.run("renderFields(DATA.show.builds[0])");
  const cells = [...ftab.matchAll(/<td class="(used|offer|skip|nil)"[^]*?<\/td>/g)];
  check(cells.some(c => c[1] === "used" && /data-goto="item:/.test(c[0])) && cells.every(c => c[1] === "used" || !/data-goto=/.test(c[0])), "fields: only the value Plex got links to its copy");
  const fg = f => page.run(`(fieldGoto(DATA.show.builds[0], ${JSON.stringify(f)}) || {}).to`);
  check(/data-goto="item:Image\/poster"/.test(ftab) && fg("Season 1 Posters") === "art:Season 1/poster" && fg("Specials logos") === "art:Specials/logo" && fg("Studio") === undefined, "fields: an image field leads to its own kind, not the top of the list");
  checkLinks("fields", ftab);
  checkEscaped("fields", ftab);
  // A build recorded before itemSrc: no threads, no failure.
  const bare = withRows("{...DATA.show.builds[0], itemSrc: undefined}");
  check(!/class="th/.test(bare) && bare.includes("coverPoster"), "show item without itemSrc: it still renders, without threads");

  // The older, failed build.
  page.run("curBuild = DATA.show.builds[1].id; renderShow()");
  const older = page.html();
  check(older.includes("The build failed") && older.includes("kept for the newest build only"), "show, older build: the failure and the newest-only note");
  checkLinks("show, older build", older);
  checkEscaped("show, older build", older);
  page.run("curBuild = DATA.show.builds[0].id; renderShow()");

  // The episode search.
  const found = q => { page.run(`EP.q = ${JSON.stringify(q)}; EP.only = false; renderEpList()`); return [...page.run(`document.getElementById("ep-list").innerHTML`).matchAll(/class="ep-n">(S\d+E\d+)</g)].map(m => m[1]).join(" "); };
  check(found("s01e08") === "S01E08", `search s01e08 found "${found("s01e08")}"`);
  check(found("1x08") === "S01E08", `search 1x08 found "${found("1x08")}"`);
  check(found("8") === "S01E08 S02E08", `search 8 found "${found("8")}"`);
  check(found("s2") === "S02E08", `search s2 found "${found("s2")}"`);
  check(found("promise") === "S01E08", `search promise found "${found("promise")}"`);
  check(found("ebb") === "S02E08", `search ebb (another source's title) found "${found("ebb")}"`);
  check(found("zzz") === "" && page.run(`document.getElementById("ep-list").innerHTML`).includes("No episode matches"), "search zzz: nothing, and the list says so");
  check(found("</script><b>x") === "S01E08", "search for markup finds the title that has it");
  checkEscaped("show, search", page.run(`EP.q = "<b>x"; renderEpList(); document.getElementById("ep-list").innerHTML`));
  page.run(`EP.q = ""; EP.only = true; renderEpList()`);
  check([...page.run(`document.getElementById("ep-list").innerHTML`).matchAll(/class="ep-n">(S\d+E\d+)</g)].map(m => m[1]).join(" ") === "S01E08 S01E12", "the Problems filter shows the 2 episodes with one");

  // The search draws once typing pauses. A click that draws the list before
  // then already has the new query: the waiting draw is dropped, so the list
  // is drawn once and the row just clicked stays.
  {
    const typed = () => {
      const p = load("show", doc), draws = p.draws("ep-list");
      p.fire("input", {target: {id: "ep-find", value: "8", isConnected: true}});
      check(p.waiting() === 1 && draws() === 0, "episode search: a key draws at once, not after the pause");
      return {p, draws, rows: () => [...p.run(`document.getElementById("ep-list").innerHTML`).matchAll(/class="ep-n">(S\d+E\d+)</g)].map(m => m[1]).join(" ")};
    };
    let {p, draws, rows} = typed();
    p.flush();
    check(draws() === 1 && rows() === "S01E08 S02E08", `episode search: the paused search draws once (${draws()} draws, rows ${rows()})`);
    ({p, draws, rows} = typed());
    p.fire("keydown", {key: "Enter", target: {id: "ep-find"}});
    check(draws() === 1 && p.waiting() === 0, "episode search: Enter draws at once, and nothing waits after it");
    const clicks = {
      "the Problems filter": [".seg button[data-f]", {dataset: {f: "issue"}, parentNode: {querySelectorAll: () => []}}],
      "a season header": [".fold-h", {parentNode: {dataset: {season: "1"}}}],
      "Open all seasons": ["[data-allseasons]", {}],
    };
    for (const [what, [sel, el]] of Object.entries(clicks)) {
      ({p, draws, rows} = typed());
      p.fire("click", {target: {closest: x => x === sel ? el : null}, preventDefault() {}});
      p.flush();
      check(draws() === 1 && p.waiting() === 0, `episode search, then ${what} before the pause: the list is drawn ${draws()} times, want once`);
    }
    // An episode row opens in place: with a search waiting, the list is drawn
    // at the click, with the row open, and nothing waits to draw it again.
    ({p, draws, rows} = typed());
    // The row as the page had it before the click, for the page to open in place.
    const i = p.run("EP.eps.findIndex(e => e.s === 1 && e.e === 8)");
    const row = {dataset: {i: String(i)}, insertAdjacentHTML() {}, querySelector: () => ({remove() {}}), lastElementChild: {classList: {add() {}}}};
    const eh = {parentNode: row, setAttribute() {}};
    p.fire("click", {target: {closest: x => x === ".ep-h" ? eh : null}, preventDefault() {}});
    const atClick = draws(), left = p.waiting();
    p.flush();
    const list = p.run(`document.getElementById("ep-list").innerHTML`);
    check(atClick === 1 && left === 0 && draws() === 1 && rows() === "S01E08 S02E08" && count(list, /class="ep-x/g) === 1 && /aria-expanded="true">\s*<span class="ep-n">S01E08/.test(list),
      `episode search, then an episode row before the pause: ${atClick} draws at the click and ${left} waiting, want 1 draw with the row open and none waiting`);

    // An open row clicked again with a search waiting: it closes, drawn once.
    {
      const q = load("show", doc), n = q.draws("ep-list");
      const click = () => q.fire("click", {target: {closest: x => x === ".ep-h" ? eh : null}, preventDefault() {}});
      click(); // opens in place: nothing waits, nothing is drawn
      const openedInPlace = n() === 0 && q.run(`EP.openEps.has(${i})`);
      q.fire("input", {target: {id: "ep-find", value: "8", isConnected: true}});
      click();
      q.flush();
      const l = q.run(`document.getElementById("ep-list").innerHTML`);
      check(openedInPlace && n() === 1 && count(l, /class="ep-x/g) === 0 && q.run("EP.openEps.size") === 0,
        `episode search, then an open row clicked again before the pause: ${n()} draws, ${count(l, /class="ep-x/g)} details, ${q.run("EP.openEps.size")} open, want 1 draw and the row closed`);
    }
    // A row the new query leaves out: the list is drawn without it, and says so.
    {
      const q = load("show", doc);
      q.fire("input", {target: {id: "ep-find", value: "zzz", isConnected: true}});
      let threw = "";
      try { q.fire("click", {target: {closest: x => x === ".ep-h" ? eh : null}, preventDefault() {}}); q.flush(); } catch (err) { threw = err.message; }
      check(!threw && q.run(`document.getElementById("ep-list").innerHTML`).includes("No episode matches"), `episode search that leaves the clicked row out: ${threw || "the list doesn't say nothing matches"}`);
    }
    // The opened row is found again in the new list, focused, and marked fresh.
    {
      const q = load("show", doc), asked = [], focused = [], marked = [];
      const again = {querySelector: s => s === ".ep-h" ? {focus: o => focused.push(o)} : null, lastElementChild: {classList: {add: c => marked.push(c)}}};
      q.document.querySelector = s => { asked.push(s); return s === `.ep[data-i="${i}"]` ? again : null; };
      q.fire("input", {target: {id: "ep-find", value: "8", isConnected: true}});
      q.fire("click", {target: {closest: x => x === ".ep-h" ? eh : null}, preventDefault() {}});
      check(asked.includes(`.ep[data-i="${i}"]`) && focused.length === 1 && focused[0] && focused[0].preventScroll === true && marked.join() === "fresh",
        `episode search, then an episode row: asked ${JSON.stringify(asked)}, focused ${focused.length}, marked ${JSON.stringify(marked)}; want the row's button focused and its detail fresh`);
    }
  }

  // Every kind of show page draws at its first load, not only after another
  // render: what that draw uses must be declared before it.
  for (const [what, builds, want] of [["no builds", [], "Kizuna has not built this show yet"], ["only a failed build", [doc.builds[1]], "The build failed"]]) {
    let html = "";
    try { html = load("show", {...doc, builds}).html(); } catch (err) { html = "threw: " + err.message; }
    check(html.includes(want), `show with ${what}, first load: ${html.startsWith("threw") ? html : `"${want}" is missing`}`);
  }

  // More than 40 episodes: the seasons start closed, and a search opens what matches.
  const long = JSON.parse(JSON.stringify(doc));
  long.builds[0].episodes = Array.from({length: 1100}, (_, i) => ({s: 1 + Math.floor(i / 275), e: i % 275 + 1, title: "Episode title " + i, from: "anidb", thumb: "https://img.example/" + i + ".jpg"}));
  const big = load("show", long), bigHTML = big.html();
  check(count(bigHTML, /class="ep-n"/g) === 0 && count(bigHTML, /class="fold-h" aria-expanded="false"/g) >= 4, "long show: seasons start closed, no episode row is made");
  check(!/<img/i.test(bigHTML), "long show: an <img> at load");
  big.run(`EP.q = "s03e100"; renderEpList()`);
  check(count(big.run(`document.getElementById("ep-list").innerHTML`), /class="ep-n"/g) === 1, "long show: a search opens the season with the match");

  // The bug report.
  for (const n of [0, 1]) {
    const md = page.run(`bugReport(DATA.show, DATA.show.builds[${n}])`), what = `bug report, build ${n}`;
    check(md.startsWith("### Kizuna match report: Lighthouse Keepers"), `${what}: the heading`);
    check(md.includes("- Kizuna version: 1.2.3-test"), `${what}: the version`);
    check(!/http/i.test(md), `${what}: a web address: ${(md.match(/.*http.*/i) || [""])[0].slice(0, 120)}`);
    check(!md.includes("/media/") && !md.includes("Season 01") && !/C:\\/.test(md) && !md.includes("Season 09"), `${what}: a folder of the reader's library`);
    check(!md.includes("/library/"), `${what}: a Plex request URL`);
    check(!md.includes("SUMMARY-TEXT"), `${what}: a summary`);
    check(!md.includes("CAST-NAME"), `${what}: the cast`);
    check(!md.includes("SECRET"), `${what}: a key from an address in a check`);
    check(!/<\/script|<b>/.test(md), `${what}: markup from the data, not escaped`);
    check(md.includes("- File: Lighthouse Keepers - S09E01.mkv"), `${what}: the latest match's bare file name`);
    if (n === 0) {
      check(md.includes("- File: Lighthouse Keepers - S01E01.mkv"), `${what}: the match's bare file name`);
      check(md.includes("| S01E08 | AniDB episode 8 of anime 7 | The Promise &lt;/script&gt;&lt;b&gt;x | AniDB | no air date |"), `${what}: the episode row with its pairing, title, source and issue`);
      check(md.includes("- Warning: A source failed: tmdb details. The other sources filled in. See [api.themoviedb.org] for it."), `${what}: a check, its address cut to the host`);
      check(md.includes("| Summary | TVDB | AniDB, TVDB |") && md.includes("| 7 | Lighthouse Keepers | 86 |") && md.includes("- Specials: 1 episode"), `${what}: the fields, candidates and seasons`);
    } else {
      check(md.includes("failed") && md.includes("- Error: Get \"[api.anidb.example]\": context deadline exceeded"), `${what}: the error, its address cut to the host`);
    }
  }
}

// ---- unmatched page ---------------------------------------------------------
{
  const doc = fixture("unmatched"), page = load("unmatched", doc), html = page.html();
  check(page.title().startsWith("No match: Lighthouse Weepers"), "unmatched: the title");
  for (const id of ["f-cands", "f-fix", "f-req", "f-seen"]) check(html.includes(`id="${id}"`) && html.includes(`href="#${id}"`), `unmatched: section ${id} or its link is missing`);
  checkLinks("unmatched", html);
  checkEscaped("unmatched", html);
  check(!/<img/i.test(html), "unmatched: an <img>");
  check(html.includes("scored 61") && html.includes("below MIN_SCORE (80)"), "unmatched: why, for a best score below MIN_SCORE");
  check(html.includes("Add an id tag to the folder name") && html.includes("Lower MIN_SCORE") && html.includes("Use Fix Match in Plex"), "unmatched: the three ways to fix a low score");
  // The example tag goes after the title without its own tags.
  check(html.includes('<code class="ex">Lighthouse Weepers &lt;/script&gt;&lt;b&gt;x [anidb-7]</code>'), "unmatched: the example folder name is the query and the best candidate's tag");
  check(html.includes("after 30 days without a request"), "unmatched: when the page goes");
  check(html.includes("4 times"), "unmatched: a repeated request is counted");

  const why = by => { const d = JSON.parse(JSON.stringify(doc)); Object.assign(d.match, by); const h = load("unmatched", d).html(); checkLinks("unmatched " + JSON.stringify(by.by), h); checkEscaped("unmatched " + JSON.stringify(by.by), h); return h; };
  let h = why({by: {id: "tags", text: "[anidb-1] [tvdb-2]"}, candidates: []});
  check(h.includes("more than one id tag") && h.includes("Keep one id tag") && !h.includes("Lower MIN_SCORE") && h.includes("The title search didn't run"), "unmatched, two tags: the reason, the first step, no MIN_SCORE step");
  check(h.includes("[anidb-12345]"), "unmatched, two tags: a made-up tag in the example when there is no candidate");
  h = why({by: {id: "off", text: "[tvdb-2]", off: "tvdb", need: "TVDB_API_KEY"}, candidates: []});
  check(h.includes("needs the <code>/tvdb</code> provider, which is off") && h.includes("Set <code>TVDB_API_KEY</code>") && !h.includes("Add an id tag to the folder name"), "unmatched, tag whose provider is off: the reason and the setting");
  h = why({by: {search: "none"}, candidates: []});
  check(h.includes("found nothing similar") && h.includes("Add an id tag to the folder name") && !h.includes("Lower MIN_SCORE"), "unmatched, nothing similar: the reason and the tag step");
  h = why({type: "season", by: {search: "found", best: 100, missing: "season 9"}});
  check(h.includes("it has no season 9") && h.includes("Check the season and episode numbers") && !h.includes("Lower MIN_SCORE"), "unmatched, no such season: the reason and the first step");

  // An id tag that names a show Kizuna can't find: no search ran after it.
  const tagDoc = fixture("unmatched-tag"), tagHTML = load("unmatched", tagDoc).html();
  checkLinks("unmatched-tag", tagHTML);
  checkEscaped("unmatched-tag", tagHTML);
  check(tagHTML.includes("Kizuna answered with no match: tag [anidb-999999992]; anidb-999999992 isn") && tagHTML.includes("title list (a new entry appears with the next daily list)."),"unmatched, a tag for an unknown show: the reason");
  check(tagHTML.includes("The title search didn't run: the id tag decided first.") && !tagHTML.includes("found nothing similar"), "unmatched, a tag for an unknown show: the search didn't run");
  check(tagHTML.includes("Check the id in the tag") && tagHTML.includes("<code>[anidb-999999992]</code>") && tagHTML.includes("added to AniDB in the last day") &&
    !tagHTML.includes("Add an id tag to the folder name") && !tagHTML.includes("[anidb-12345]") && !tagHTML.includes("Lower MIN_SCORE") && tagHTML.includes("Use Fix Match in Plex"),
    "unmatched, a tag for an unknown show: check the tag, no second tag offered");
  const tagWhy = m => { const d = JSON.parse(JSON.stringify(tagDoc)); Object.assign(d.match, m); return load("unmatched", d).html(); };
  h = tagWhy({mode: "tvdb", how: "tag [tvdb-999999994]; tvdb-999999994 has no episodes on TheTVDB", by: {id: "tag", text: "[tvdb-999999994]"}});
  check(h.includes("tvdb-999999994 has no episodes on TheTVDB.") && h.includes("Check the id in the tag") && h.includes("<code>[tvdb-999999994]</code>") &&
    !h.includes("added to AniDB") && h.includes("the id tag decided first"), "unmatched, a TheTVDB tag for an unknown show: no AniDB hint");
  h = tagWhy({by: {id: "guid", text: "tv.plex.agents.custom.kizuna.anidb://show/anidb-999999992"}});
  check(h.includes("The title search didn't run: the GUID Plex already had decided first.") && h.includes("Add an id tag to the folder name") && !h.includes("Check the id in the tag"),
    "unmatched, a GUID for an unknown show: the search didn't run, the tag step");
}

// ---- index ------------------------------------------------------------------
{
  const rows = fixture("index"), page = load("index", rows), html = page.html();
  checkLinks("index", html);
  checkEscaped("index", html);
  check(!/<img/i.test(html), "index: an <img>");
  check(html.includes("5 shows and 2 unmatched titles"), "index: the counts");
  check(html.includes("or after 30 days without a request"), "index: when an unmatched page goes");
  check(html.includes("A show's page goes after 90 days without a request for the show, and comes back at its next refresh."), "index: when a show's page goes");
  check(html.includes('href="anidb-7.html"') && html.includes('href="unmatched-lighthouse-weepers-0d77e353.html"'), "index: the links to the pages");
  check(html.includes('href="anidb-40%22onmouseover%3D%22x.html"'), "index: a key is encoded in its link");
  const keys = () => [...(page.run(`document.getElementById("ix-shows-body").innerHTML`) + page.run(`document.getElementById("ix-un-body").innerHTML`)).matchAll(/<a href="([^"]+)\.html">/g)].map(m => decodeURIComponent(m[1]).replace(/^unmatched-(\w+).*/, "un-$1").replace(/"onmouseover.*/, "")).join(" ");
  const filter = f => { page.run(`IX.f = ${JSON.stringify({q: "", state: "", mode: "", ...f})}; ixDraw()`); return keys(); };
  check(filter({}) === "anidb-7 anidb-20 anidb-30 tvdb-900 anidb-40 un-lighthouse un-javascript", `index, no filter: ${filter({})}`);
  check(filter({state: "failed"}) === "anidb-30 un-lighthouse un-javascript", `index, failed: ${filter({state: "failed"})}`);
  check(filter({state: "incomplete"}) === "anidb-7 un-lighthouse un-javascript", `index, incomplete: ${filter({state: "incomplete"})}`);
  check(filter({state: "check"}) === "anidb-7 tvdb-900 un-lighthouse un-javascript", `index, to check: ${filter({state: "check"})}`);
  check(filter({mode: "anidb"}) === "anidb-20 anidb-30 un-lighthouse", `index, /anidb: ${filter({mode: "anidb"})}`);
  check(filter({mode: "tvdb"}) === "anidb-7 tvdb-900", `index, /tvdb: ${filter({mode: "tvdb"})}`);
  check(filter({mode: "auto"}) === "anidb-7 un-javascript", `index, /auto: ${filter({mode: "auto"})}`);
  check(filter({state: "check", mode: "tvdb", q: "FOG"}) === "tvdb-900", `index, to check + /tvdb + text: ${filter({state: "check", mode: "tvdb", q: "FOG"})}`);
  check(filter({q: "tvdb-500"}) === "anidb-7", `index, text in a root: ${filter({q: "tvdb-500"})}`);
  check(filter({q: "灯台"}) === "anidb-7", `index, text in the original title: ${filter({q: "灯台"})}`);
  check(page.run(`document.getElementById("ix-count").innerHTML`).startsWith("1 of 5 shows and 0 of 2 unmatched titles shown."), "index: how many rows pass the filters");
  check(filter({q: "zzz"}) === "" && page.html().includes("No show passes these filters.") && count(page.html(), /data-ixclear/g) === 3, "index, nothing passes: both tables say so and offer to clear the filters");
  check(filter({state: "failed", mode: "tvdb"}) === "" && page.html().includes("No show passes these filters."), "index, failed + /tvdb: nothing");
  checkEscaped("index, filtered", page.html());

  // The index filter waits the same way, and a click's draw drops the waiting one.
  {
    const p = load("index", rows), draws = p.draws("ix-shows-body");
    p.fire("input", {target: {id: "ix-find", value: "fog", isConnected: true}});
    check(p.waiting() === 1 && draws() === 0, "index filter: a key draws at once, not after the pause");
    const btn = {dataset: {ixs: "check"}, parentNode: {querySelectorAll: () => []}};
    p.fire("click", {target: {closest: x => x === "[data-ixs]" ? btn : null}, preventDefault() {}});
    p.flush();
    check(draws() === 1 && p.waiting() === 0, `index filter, then a state button before the pause: the table is drawn ${draws()} times, want once`);
    check(/^1 of 5 shows/.test(p.run(`document.getElementById("ix-count").innerHTML`)), "index filter, then a state button: both filters apply");
  }

  // Thousands of rows: a table draws its first rows only.
  const many = Array.from({length: 3000}, (_, i) => ({schema: 3, key: "anidb-" + (i + 1), kind: "show", title: "Show " + i, modes: ["anidb"], last: "2026-11-10T20:00:00Z", complete: true, checks: 0, builds: 1, requests: 1}));
  const big = load("index", many), drawn = count(big.run(`document.getElementById("ix-shows-body").innerHTML`), /<tr>/g);
  check(drawn === 251, `big index: ${drawn - 1} rows drawn at load, want 250`);
  check(big.html().includes("Show the other 2750"), "big index: the rest is offered");
  const none = load("index", []).html();
  check(none.includes("No shows yet.") && none.includes("Nothing unmatched."), "empty index: both tables say so");
}

// ---- keys that name a property every object has -----------------------------
// A saved build can be hand-edited. A key like "constructor" or "__proto__"
// shows as any unknown key does: nothing inherited is found for it.
{
  const odd = ["constructor", "__proto__", "toString", "hasOwnProperty"];
  const d = JSON.parse(JSON.stringify(fixture("show"))), b = d.builds[0];
  // The id sites on a page of their own: "__proto__" threw while the page drew.
  {
    const withIds = JSON.parse(JSON.stringify(d));
    withIds.ids = [...odd, "banner"].map(site => ({site, value: site + "-1"}));
    let ids = "";
    try { ids = load("show", withIds).html().match(/<ul class="ids">.*?<\/ul>/s)[0]; } catch (err) { check(false, "odd keys: the page with odd id sites threw at load: " + err.message); }
    for (const k of odd) check(ids.includes(`<li>${k}<span class="v">${k}-1</span></li>`), `odd keys: the id site ${k} is not shown as an unknown site`);
  }
  b.art = [...odd, "banner"].map(kind => ({level: "Level " + kind, kind, images: [{url: "https://image.tmdb.org/t/p/original/k.jpg", src: kind}, {url: "https://img.example/k.jpg", src: "tvdb"}]}));
  b.fields = [{name: "Title", order: ["anidb", ...odd], asked: ["anidb", ...odd], offered: {anidb: "T"}, used: ["anidb"]}];
  b.checks = odd.map(section => ({level: "warn", title: "Check " + section, text: "Text.", section}));
  b.trace = odd.map((kind, n) => ({src: kind, label: "row " + kind, kind, start: n, end: n + 1}));
  b.episodes = [{s: 1, e: 1, title: "One", from: "anidb"}, ...odd.map((s, n) => ({s, e: n + 1, title: "Odd " + s, from: s}))];
  b.item.Image = [...odd, "banner"].map(type => ({type, url: "https://img.example/" + type + ".jpg"}));
  let page = null;
  try { page = load("show", d); } catch (err) { check(false, "odd keys: the page threw at load: " + err.message); }
  if (page) {
    const html = page.html();
    // like is how an unknown word shows: "banner" with the odd word in its place.
    const like = (h, k) => h.replaceAll("banner", k).replaceAll("Banner", k[0].toUpperCase() + k.slice(1));
    for (const k of odd) {
      check(page.run(`th(${JSON.stringify(k)})`) === like(page.run(`th("banner")`), k), `odd keys: the source ${k} is not shown as an unknown source`);
      check(page.run(`answerText({kind: ${JSON.stringify(k)}})`) === k && page.run(`rootWords(${JSON.stringify(k + "-5")})`) === k + "-5", `odd keys: the answer or root ${k} is not shown as it is`);
      check(page.run(`fieldGoto(DATA.show.builds[0], ${JSON.stringify(k)})`) === null, `odd keys: the field ${k} leads somewhere`);
    }
    let arts = null;
    try { arts = page.run("ART.map(artBody)"); } catch (err) { check(false, "odd keys: opening an art level threw: " + err.message); }
    if (arts) {
      const banner = arts[odd.length];
      odd.forEach((k, n) => check(arts[n] === like(banner, k), `odd keys: the art kind ${k} does not open as an unknown kind:\n${arts[n].slice(0, 300)}\nwant\n${like(banner, k).slice(0, 300)}`));
    }
    // A closed level: its name and counts (the tallies share their columns across levels).
    const folds = [...html.matchAll(/<div class="fold" data-art="\d+">.*?<span class="fold-t">(.*?)<\/span><span class="fold-m fold-c">(.*?)<\/span>/gs)].map(m => m[1] + "|" + m[2]);
    odd.forEach((k, n) => check(folds[n] === like(folds[odd.length], k), `odd keys: the closed art level ${k} does not count as an unknown kind: ${folds[n]}`));
    const sentAs = t => page.run(`sentImages([{type: ${JSON.stringify(t)}, url: "https://img.example/x.jpg"}], {item: {}})`);
    for (const k of odd) check(sentAs(k) === like(sentAs("banner"), k), `odd keys: the Image type ${k} is not shown as an unknown type: ${sentAs(k)}`);
    const sent = page.run(`(() => { ITEM_OPEN.add("Image"); const h = renderItem(DATA.show.builds[0]); ITEM_OPEN.clear(); return h; })()`);
    check(page.run("renderItem(DATA.show.builds[0])").includes('<span class="dim">5 other images</span>'), "odd keys: the closed Image row does not count the odd types as other images");
    const fields = page.run("renderFields(DATA.show.builds[0])");
    check(count(fields, /<td class="nil">/g) === odd.length && !/native code|object Object/.test(fields), "odd keys: a source with no value in a field shows something");
    check(!/Go to|native code/.test(page.run("renderChecks(DATA.show.builds[0])")), "odd keys: a check's odd section links somewhere");
    check(!/native code|object Object/.test(page.run("renderTrace(DATA.show.builds[0])")), "odd keys: the timeline shows an inherited property");
    const list = page.run(`document.getElementById("ep-list").innerHTML`);
    check(count(list, /class="ep-n"/g) === odd.length + 1, `odd keys: ${count(list, /class="ep-n"/g)} episode rows, want ${odd.length + 1} (an odd season's rows are drawn)`);
    checkEscaped("odd keys", html + (arts || []).join("") + sent);
  }
}

// ---- the page file ----------------------------------------------------------
{
  check(/<meta name="referrer" content="no-referrer">/.test(template), "page.html: the no-referrer meta");
  const markup = template.slice(0, template.indexOf("<script>"));
  check(!/<(script|link|img|iframe)[^>]+(src|href)\s*=/i.test(markup) && !/@import|url\(/i.test(markup), "page.html: it loads something from outside");
  check(count(template, /<script/g) === 1, "page.html: more than one script");
  check(!/console\.log|debugger/.test(template), "page.html: a console.log or debugger is left in");
  check(load().text() === "This page has no data.", "page.html without data: it says so");
}

if (failed) {
  console.error(`${failed} of ${checks} checks failed`);
  process.exit(1);
}
console.log(`ok: ${checks} checks`);
