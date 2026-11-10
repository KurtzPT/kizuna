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
// among the script's own names, html() is everything it has written.
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
  const document = {
    title: "", body: element("body"),
    // Only what the page has written exists, as in a browser.
    getElementById: id => id === "app" || [...made.values()].some(e => e.innerHTML.includes(`id="${id}"`)) ? element(id) : null,
    querySelector() { return null; }, querySelectorAll() { return []; }, addEventListener() {},
  };
  const context = vm.createContext({document, location: {hash: ""}, history: {replaceState() {}}, navigator: {}, setTimeout, clearTimeout, console});
  vm.runInContext(script, context, {filename: "page.html <script>"});
  return {
    run: code => vm.runInContext(code, context),
    html: () => [...made.values()].map(e => e.innerHTML).join("\n"),
    title: () => document.title,
    text: () => app.textContent,
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
  check(/data-goto="item:Image\/(poster|background|logo)"/.test(ftab) && /data-goto="art:(Specials|Season \d+)\/(poster|background|logo)"/.test(ftab), "fields: an image field leads to its own kind, not the top of the list");
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
}

// ---- index ------------------------------------------------------------------
{
  const rows = fixture("index"), page = load("index", rows), html = page.html();
  checkLinks("index", html);
  checkEscaped("index", html);
  check(!/<img/i.test(html), "index: an <img>");
  check(html.includes("5 shows and 2 unmatched titles"), "index: the counts");
  check(html.includes("after 30 days without a request"), "index: when an unmatched page goes");
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

  // Thousands of rows: a table draws its first rows only.
  const many = Array.from({length: 3000}, (_, i) => ({schema: 3, key: "anidb-" + (i + 1), kind: "show", title: "Show " + i, modes: ["anidb"], last: "2026-11-10T20:00:00Z", complete: true, checks: 0, builds: 1, requests: 1}));
  const big = load("index", many), drawn = count(big.run(`document.getElementById("ix-shows-body").innerHTML`), /<tr>/g);
  check(drawn === 251, `big index: ${drawn - 1} rows drawn at load, want 250`);
  check(big.html().includes("Show the other 2750"), "big index: the rest is offered");
  const none = load("index", []).html();
  check(none.includes("No shows yet.") && none.includes("Nothing unmatched."), "empty index: both tables say so");
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
