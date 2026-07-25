#!/usr/bin/env python3
"""Renders one merged lcov file into a browsable coverage report.

Invoked by `./repo.sh coverage` (see repo.sh) as

  python3 coverage_html.py <merged.lcov> <out.html> [--root DIR] [--title T]
                           [--markdown OUT.md] [--html-link URL]

Two renderings, ONE model (build_report below), because a summary that
disagrees with the report it links to is worse than having no summary:

  <out.html>   a single self-contained interactive page (see below).
  --markdown   GitHub-flavored Markdown for $GITHUB_STEP_SUMMARY. The job
               summary renderer strips <script> and <style>, so the HTML page
               cannot be inlined there at all - the summary is tables plus
               text bars, and --html-link points at the uploaded artifact
               holding the real page.

Why not genhtml: genhtml is Perl, ships with lcov, and is not in
tools.lock.toml - adding it would mean pinning a Perl interpreter plus the
lcov distribution for a report generator, which is a large hermetic-toolchain
cost for a view of data this repo already owns. It also emits a DIRECTORY of
cross-linked pages, which is awkward to upload as a CI artifact and to open
from a buck-out path. This writes ONE file with the sources embedded, so it
opens over file:// with no server, survives being copied anywhere, and needs
no network (the report has no external stylesheet, script, or font).

Determinism: the output is a pure function of (lcov text, source bytes,
title). Files and lines are emitted in sorted order and nothing stamps a
timestamp, hostname, or absolute path into the page, so an unchanged tree
renders a byte-identical report.

Source lines fall into three states, which the page distinguishes because
conflating the last two is how a coverage report lies:

  covered      - the merged lcov has a DA: record for the line, count > 0
  uncovered    - a DA: record, count == 0 (executable, never executed)
  uninstrumented - NO DA: record at all (comment, blank, declaration, or -
                 importantly - a file/region no lane instrumented). These are
                 neither hits nor misses and are excluded from every
                 percentage, exactly as they are in the merged lcov itself.

A file listed in the lcov whose source cannot be read (deleted, generated
into buck-out, or otherwise not in the tree) is still reported with its
percentages; only its source pane is replaced with a notice. Coverage
totals never silently drop a file.
"""
import html
import json
import os
import sys


def parse_lcov(path):
  """Returns {repo_relative_path: {line_number: hit_count}} from an lcov file.

  Only SF:/DA: are read. LF:/LH: are deliberately ignored and recomputed
  from the DA: records here: they are a summary of the same data, and
  trusting them would let a malformed writer report a total that disagrees
  with the lines the page actually paints.
  """
  files = {}
  current = None
  with open(path) as stream:
    for raw in stream:
      line = raw.strip()
      if line.startswith("SF:"):
        current = files.setdefault(line[len("SF:"):], {})
      elif line.startswith("DA:") and current is not None:
        number_text, _, count_text = line[len("DA:"):].partition(",")
        count_text = count_text.split(",")[0]
        try:
          number = int(number_text)
          count = int(count_text)
        except ValueError:
          continue
        current[number] = max(current.get(number, 0), count)
      elif line == "end_of_record":
        current = None
  return files


def _read_source(root, relative):
  # keepends=False via splitlines(): the page renders each line into its own
  # row, so trailing newline handling is the renderer's business, not the
  # data's. errors="replace" rather than a hard failure - a report that
  # refuses to render because one file is not valid UTF-8 is worse than one
  # that shows a replacement character on that line.
  try:
    with open(os.path.join(root, relative), encoding="utf-8", errors="replace") as stream:
      return stream.read().splitlines()
  except OSError:
    return None


def build_report(files, root):
  """Shapes parsed lcov data plus on-disk sources into the page's JSON model."""
  entries = []
  for relative in sorted(files):
    hits = files[relative]
    found = len(hits)
    hit = sum(1 for count in hits.values() if count > 0)
    source = _read_source(root, relative)
    entries.append({
      "path": relative,
      "found": found,
      "hit": hit,
      # Sparse map keyed by line number as a STRING: JSON object keys are
      # strings, and round-tripping them through int() in JS just to look up
      # a line buys nothing. Absent key == uninstrumented line.
      "counts": {str(number): hits[number] for number in sorted(hits)},
      "source": source,
    })
  total_found = sum(entry["found"] for entry in entries)
  total_hit = sum(entry["hit"] for entry in entries)
  return {
    "files": entries,
    "totalFound": total_found,
    "totalHit": total_hit,
  }


# Embedded as the text content of a <script type="application/json"> block
# rather than a JS literal, so no source line can terminate the script or be
# read as code. Only "</" needs neutralizing: the HTML tokenizer ends the
# block on the literal characters "</script", and it does so BEFORE any JSON
# parsing happens, so an embedded source file containing "</script>" (this
# repo has TypeScript and HTML in it) would otherwise truncate the payload
# and break the whole page.
def _embed_json(data):
  return json.dumps(data, sort_keys=True, separators=(",", ":")).replace("</", "<\\/")


_STYLE = """
:root {
  color-scheme: light dark;
  --bg: #ffffff; --fg: #1a1a1a; --muted: #6b7280; --line: #e5e7eb;
  --panel: #f8f9fa; --hit-bg: #e7f6ec; --hit-bar: #18794e;
  --miss-bg: #fdecec; --miss-bar: #cf222e; --accent: #0969da;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #0d1117; --fg: #e6edf3; --muted: #8b949e; --line: #30363d;
    --panel: #161b22; --hit-bg: #12261e; --hit-bar: #3fb950;
    --miss-bg: #2d1214; --miss-bar: #f85149; --accent: #58a6ff;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0; background: var(--bg); color: var(--fg);
  font: 14px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
}
header {
  position: sticky; top: 0; z-index: 2; background: var(--bg);
  border-bottom: 1px solid var(--line); padding: 14px 20px;
  display: flex; gap: 16px; align-items: baseline; flex-wrap: wrap;
}
h1 { font-size: 16px; margin: 0; font-weight: 600; }
.total { font-variant-numeric: tabular-nums; color: var(--muted); }
.total strong { color: var(--fg); font-size: 18px; }
main { padding: 0 20px 60px; }
input[type=search] {
  flex: 1 1 220px; min-width: 160px; padding: 6px 10px; border-radius: 6px;
  border: 1px solid var(--line); background: var(--panel); color: var(--fg);
}
table { border-collapse: collapse; width: 100%; margin-top: 16px; }
th, td { text-align: left; padding: 6px 10px; border-bottom: 1px solid var(--line); }
th { cursor: pointer; user-select: none; font-size: 12px; color: var(--muted);
     text-transform: uppercase; letter-spacing: .04em; white-space: nowrap; }
th[aria-sort]::after { content: " \\2195"; opacity: .4; }
th[aria-sort=ascending]::after { content: " \\2191"; opacity: 1; }
th[aria-sort=descending]::after { content: " \\2193"; opacity: 1; }
td.num, th.num { text-align: right; font-variant-numeric: tabular-nums; }
tbody tr:hover { background: var(--panel); }
a { color: var(--accent); text-decoration: none; cursor: pointer; }
a:hover { text-decoration: underline; }
.dir td:first-child { font-weight: 600; }
.dir { background: var(--panel); }
.bar {
  display: inline-block; width: 90px; height: 8px; border-radius: 4px;
  background: var(--miss-bg); overflow: hidden; vertical-align: middle;
}
.bar > span { display: block; height: 100%; background: var(--hit-bar); }
.pct { display: inline-block; min-width: 52px; text-align: right;
       font-variant-numeric: tabular-nums; }
pre.src { margin: 16px 0 0; overflow-x: auto; border: 1px solid var(--line);
          border-radius: 8px; background: var(--panel); }
pre.src table { margin: 0; border: 0; }
pre.src td { border: 0; padding: 0 10px; white-space: pre;
             font: 12.5px/1.55 ui-monospace, SFMono-Regular, Menlo, monospace; }
td.ln { color: var(--muted); text-align: right; user-select: none;
        width: 1%; border-right: 1px solid var(--line); }
td.ct { color: var(--muted); text-align: right; user-select: none;
        width: 1%; font-variant-numeric: tabular-nums; }
tr.hit { background: var(--hit-bg); }
tr.miss { background: var(--miss-bg); }
tr.miss td.ct { color: var(--miss-bar); font-weight: 600; }
tr.hit td.ct { color: var(--hit-bar); }
.crumb { margin: 18px 0 0; color: var(--muted); }
.notice { padding: 24px; color: var(--muted); border: 1px dashed var(--line);
          border-radius: 8px; margin-top: 16px; }
.legend { color: var(--muted); font-size: 12px; margin-top: 10px; }
.legend span { display: inline-block; margin-right: 14px; }
.swatch { display: inline-block; width: 10px; height: 10px; border-radius: 2px;
          vertical-align: baseline; margin-right: 5px; }
kbd { border: 1px solid var(--line); border-bottom-width: 2px; border-radius: 4px;
      padding: 0 4px; font: 11px ui-monospace, monospace; }
"""

_SCRIPT = r"""
const DATA = JSON.parse(document.getElementById("coverage-data").textContent);
const pct = (hit, found) => found ? (100 * hit / found) : 100;
const fmt = (hit, found) => found ? pct(hit, found).toFixed(1) + "%" : "n/a";
const esc = (s) => s.replace(/[&<>]/g, (c) => ({"&": "&amp;", "<": "&lt;", ">": "&gt;"}[c]));

function bar(hit, found) {
  const value = pct(hit, found);
  return '<span class="bar"><span style="width:' + value.toFixed(1) + '%"></span></span>' +
         '<span class="pct">' + fmt(hit, found) + "</span>";
}

// Directory rollups are computed from the file rows rather than stored, so a
// directory's percentage can never disagree with the files inside it.
function rollup(files) {
  const dirs = new Map();
  for (const file of files) {
    const slash = file.path.lastIndexOf("/");
    const dir = slash === -1 ? "." : file.path.slice(0, slash);
    const acc = dirs.get(dir) || {dir, found: 0, hit: 0, files: []};
    acc.found += file.found;
    acc.hit += file.hit;
    acc.files.push(file);
    dirs.set(dir, acc);
  }
  return [...dirs.values()].sort((a, b) => a.dir.localeCompare(b.dir));
}

let sortKey = "path";
let sortDir = 1;
let filter = "";

function matching() {
  const needle = filter.toLowerCase();
  return DATA.files.filter((f) => f.path.toLowerCase().includes(needle));
}

function sortFiles(files) {
  const key = (f) => sortKey === "path" ? f.path
    : sortKey === "found" ? f.found
    : sortKey === "hit" ? f.hit
    : sortKey === "missed" ? f.found - f.hit
    : pct(f.hit, f.found);
  return [...files].sort((a, b) => {
    const x = key(a), y = key(b);
    const cmp = typeof x === "string" ? x.localeCompare(y) : x - y;
    return cmp * sortDir;
  });
}

function renderIndex() {
  const files = matching();
  const grouped = sortKey === "path";
  let rows = "";
  if (grouped) {
    for (const dir of rollup(files)) {
      rows += '<tr class="dir"><td>' + esc(dir.dir) + "/</td>" +
        '<td class="num">' + dir.found + "</td>" +
        '<td class="num">' + dir.hit + "</td>" +
        '<td class="num">' + (dir.found - dir.hit) + "</td>" +
        "<td>" + bar(dir.hit, dir.found) + "</td></tr>";
      for (const file of sortFiles(dir.files)) rows += fileRow(file, true);
    }
  } else {
    for (const file of sortFiles(files)) rows += fileRow(file, false);
  }
  document.getElementById("view").innerHTML =
    '<table><thead><tr>' + head() + "</tr></thead><tbody>" +
    (rows || '<tr><td colspan="5" class="notice">No file matches that filter.</td></tr>') +
    "</tbody></table>" +
    '<p class="legend">' +
    '<span><i class="swatch" style="background:var(--hit-bar)"></i>covered</span>' +
    '<span><i class="swatch" style="background:var(--miss-bar)"></i>uncovered</span>' +
    '<span><i class="swatch" style="background:var(--line)"></i>not instrumented ' +
    "(excluded from every percentage)</span></p>";
  for (const th of document.querySelectorAll("th[data-key]")) {
    th.onclick = () => {
      if (sortKey === th.dataset.key) sortDir = -sortDir; else { sortKey = th.dataset.key; sortDir = 1; }
      renderIndex();
    };
  }
  for (const link of document.querySelectorAll("a[data-path]")) {
    link.onclick = (event) => { event.preventDefault(); location.hash = "#" + link.dataset.path; };
  }
}

function head() {
  const cols = [["path", "File", ""], ["found", "Lines", "num"],
                ["hit", "Hit", "num"], ["missed", "Missed", "num"], ["pct", "Coverage", ""]];
  return cols.map(([key, label, cls]) => {
    const sorted = sortKey === key ? (sortDir === 1 ? "ascending" : "descending") : "none";
    return '<th class="' + cls + '" data-key="' + key + '" aria-sort="' + sorted + '">' + label + "</th>";
  }).join("");
}

function fileRow(file, indented) {
  const name = indented ? file.path.slice(file.path.lastIndexOf("/") + 1) : file.path;
  return "<tr><td>" + (indented ? "&nbsp;&nbsp;&nbsp;&nbsp;" : "") +
    '<a data-path="' + esc(file.path) + '" href="#' + esc(file.path) + '">' + esc(name) + "</a></td>" +
    '<td class="num">' + file.found + "</td>" +
    '<td class="num">' + file.hit + "</td>" +
    '<td class="num">' + (file.found - file.hit) + "</td>" +
    "<td>" + bar(file.hit, file.found) + "</td></tr>";
}

let missLines = [];
let missIndex = -1;

function renderFile(path) {
  const file = DATA.files.find((f) => f.path === path);
  if (!file) { location.hash = ""; return; }
  let body;
  if (file.source === null) {
    body = '<p class="notice">This file is in the coverage data but is not readable ' +
      "from the repository root the report was generated against. Its numbers above " +
      "are still counted in the total.</p>";
  } else {
    let rows = "";
    missLines = [];
    file.source.forEach((text, index) => {
      const number = index + 1;
      const count = file.counts[String(number)];
      const state = count === undefined ? "" : count > 0 ? "hit" : "miss";
      if (state === "miss") missLines.push(number);
      rows += '<tr class="' + state + '" id="L' + number + '">' +
        '<td class="ln">' + number + "</td>" +
        '<td class="ct">' + (count === undefined ? "" : count) + "</td>" +
        "<td>" + esc(text) + "</td></tr>";
    });
    missIndex = -1;
    body = '<pre class="src"><table><tbody>' + rows + "</tbody></table></pre>";
  }
  document.getElementById("view").innerHTML =
    '<p class="crumb"><a href="#" id="back">&larr; All files</a> &nbsp;/&nbsp; ' +
    esc(file.path) + " &nbsp;&nbsp; " + bar(file.hit, file.found) +
    " &nbsp; " + file.hit + " / " + file.found + " lines" +
    (missLines.length ? ' &nbsp;&nbsp; <span class="legend">press <kbd>n</kbd> / <kbd>p</kbd> ' +
      "to step through the " + missLines.length + " uncovered line" +
      (missLines.length === 1 ? "" : "s") + "</span>" : "") + "</p>" + body;
  document.getElementById("back").onclick = (event) => { event.preventDefault(); location.hash = ""; };
}

function jump(step) {
  if (!missLines.length) return;
  missIndex = (missIndex + step + missLines.length) % missLines.length;
  const row = document.getElementById("L" + missLines[missIndex]);
  if (row) row.scrollIntoView({block: "center"});
}

function route() {
  const path = decodeURIComponent(location.hash.slice(1));
  if (path) renderFile(path); else renderIndex();
}

document.addEventListener("keydown", (event) => {
  if (event.target.tagName === "INPUT") return;
  if (event.key === "n") jump(1);
  else if (event.key === "p") jump(-1);
  else if (event.key === "/") { event.preventDefault(); document.getElementById("filter").focus(); }
});
document.getElementById("filter").addEventListener("input", (event) => {
  filter = event.target.value;
  if (!location.hash.slice(1)) renderIndex();
});
window.addEventListener("hashchange", route);
document.getElementById("summary").innerHTML =
  "<strong>" + fmt(DATA.totalHit, DATA.totalFound) + "</strong> &nbsp;" +
  DATA.totalHit + " / " + DATA.totalFound + " lines &nbsp;&middot;&nbsp; " +
  DATA.files.length + " files";
route();
"""

_PAGE = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%(title)s</title>
<style>%(style)s</style>
</head>
<body>
<header>
  <h1>%(title)s</h1>
  <div class="total" id="summary"></div>
  <input type="search" id="filter" placeholder="Filter files ( / )" autocomplete="off">
</header>
<main><div id="view"></div></main>
<script type="application/json" id="coverage-data">%(data)s</script>
<script>%(script)s</script>
</body>
</html>
"""

def _bar(hit, found, width = 20):
  """A text coverage bar. GitHub's job-summary renderer allows no inline
  style and no images from an unproxied host, so the bar is drawn with block
  characters - which also keeps the summary readable as plain text in a
  terminal or a diff."""
  filled = int(round(width * (hit / found))) if found else width
  return "█" * filled + "░" * (width - filled)


def _pct_text(hit, found):
  return "%.1f%%" % (100.0 * hit / found) if found else "n/a"


def _uncovered_ranges(counts):
  """Collapses uncovered line numbers into "12-15, 40" style ranges."""
  missed = sorted(int(number) for number, count in counts.items() if count == 0)
  ranges = []
  for number in missed:
    if ranges and number == ranges[-1][1] + 1:
      ranges[-1][1] = number
    else:
      ranges.append([number, number])
  return ["%d" % lo if lo == hi else "%d-%d" % (lo, hi) for lo, hi in ranges]


# GitHub rejects a step summary over 1 MiB outright (the whole summary is
# dropped, not truncated), so the per-file table is capped. A silent cap
# would read as "these are all the files"; the renderer states what it left
# out instead. See docs/CI-RELEASE.md.
_MAX_SUMMARY_ROWS = 200
_MAX_RANGES = 8


def render_markdown(report, title, html_link = None):
  total_hit = report["totalHit"]
  total_found = report["totalFound"]
  lines = [
    "## %s" % title,
    "",
    "**%s** &nbsp;`%s`&nbsp; %d / %d lines across %d files"
    % (_pct_text(total_hit, total_found), _bar(total_hit, total_found),
       total_hit, total_found, len(report["files"])),
    "",
  ]
  if html_link:
    lines += ["[Open the browsable HTML report](%s) - annotated sources, "
              "per-file navigation, uncovered-line stepping." % html_link, ""]

  directories = {}
  for entry in report["files"]:
    slash = entry["path"].rfind("/")
    directory = entry["path"][:slash] if slash != -1 else "."
    accumulated = directories.setdefault(directory, [0, 0])
    accumulated[0] += entry["found"]
    accumulated[1] += entry["hit"]
  lines += ["| Directory | Lines | Hit | Missed | Coverage |",
            "| --- | ---: | ---: | ---: | --- |"]
  for directory in sorted(directories):
    found, hit = directories[directory]
    lines.append("| `%s/` | %d | %d | %d | `%s` %s |"
                 % (directory, found, hit, found - hit, _bar(hit, found), _pct_text(hit, found)))
  lines.append("")

  # Ordered worst-first: the reason to open a coverage summary is to find
  # what is not covered, and an alphabetical list buries that under whatever
  # happens to sort first.
  # Lowest percentage first, then the larger absolute gap, then path for a
  # stable order. Ranking by missed-line COUNT alone (the first attempt here)
  # puts a 3000-line file at 99% above a 4-line file at 0%, which inverts
  # what the reader is looking for.
  ranked = sorted(report["files"],
                  key = lambda entry: (entry["hit"] / entry["found"] if entry["found"] else 1.0,
                                       -(entry["found"] - entry["hit"]), entry["path"]))
  shown = ranked[:_MAX_SUMMARY_ROWS]
  lines += ["<details><summary>Per-file coverage (%d files, least covered first)</summary>"
            % len(report["files"]), "",
            "| File | Lines | Hit | Missed | Coverage | Uncovered lines |",
            "| --- | ---: | ---: | ---: | --- | --- |"]
  for entry in shown:
    ranges = _uncovered_ranges(entry["counts"])
    if len(ranges) > _MAX_RANGES:
      ranges = ranges[:_MAX_RANGES] + ["+%d more" % (len(ranges) - _MAX_RANGES)]
    lines.append("| `%s` | %d | %d | %d | `%s` %s | %s |"
                 % (entry["path"], entry["found"], entry["hit"],
                    entry["found"] - entry["hit"], _bar(entry["hit"], entry["found"]),
                    _pct_text(entry["hit"], entry["found"]),
                    ", ".join(ranges) if ranges else "-"))
  if len(ranked) > len(shown):
    lines.append("")
    lines.append("_%d further files are fully covered or omitted from this table; "
                 "the HTML report lists every file._" % (len(ranked) - len(shown)))
  lines += ["", "</details>", ""]
  return "\n".join(lines) + "\n"


_USAGE = ("usage: coverage_html.py <merged.lcov> <out.html> [--root DIR] [--title TEXT]\n"
          "                        [--markdown OUT.md] [--html-link URL]")


def main(argv):
  positional = []
  options = {"--root": ".", "--title": "Coverage report", "--markdown": None, "--html-link": None}
  index = 0
  while index < len(argv):
    arg = argv[index]
    if arg in options:
      index += 1
      if index >= len(argv):
        raise SystemExit("coverage_html.py: %s requires a value\n%s" % (arg, _USAGE))
      options[arg] = argv[index]
    else:
      positional.append(arg)
    index += 1
  root = options["--root"]
  title = options["--title"]

  if len(positional) != 2:
    raise SystemExit(
      "coverage_html.py: expected 2 positional arguments, got %d\n%s"
      % (len(positional), _USAGE)
    )
  lcov_path, out_path = positional
  report = build_report(parse_lcov(lcov_path), root)
  page = _PAGE % {
    "title": html.escape(title),
    "style": _STYLE,
    "script": _SCRIPT,
    "data": _embed_json(report),
  }
  os.makedirs(os.path.dirname(os.path.abspath(out_path)), exist_ok=True)
  with open(out_path, "w", encoding="utf-8") as stream:
    stream.write(page)
  print("coverage: html report %s (%d files)" % (out_path, len(report["files"])))

  if options["--markdown"]:
    markdown_path = options["--markdown"]
    os.makedirs(os.path.dirname(os.path.abspath(markdown_path)), exist_ok=True)
    with open(markdown_path, "w", encoding="utf-8") as stream:
      stream.write(render_markdown(report, title, options["--html-link"]))
    print("coverage: markdown summary %s" % (markdown_path,))


if __name__ == "__main__":
  main(sys.argv[1:])
