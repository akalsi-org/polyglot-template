"""Tests for tools/coverage_html.py, the browsable coverage report renderer.

Run by `./repo.sh infra-test` (unittest discover over test/), like
test_py_cover.py and test_lint.py.
"""
import contextlib
import io
import json
import re
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "tools"))

import coverage_html  # noqa: E402


LCOV = """SF:a/one.py
DA:1,3
DA:2,0
DA:5,1
LF:3
LH:2
end_of_record
SF:b/two.ts
DA:1,0
LF:1
LH:0
end_of_record
"""


class ParseLcovTest(unittest.TestCase):
  def test_parses_files_and_counts(self):
    with tempfile.TemporaryDirectory() as scratch:
      path = Path(scratch) / "merged.lcov"
      path.write_text(LCOV)
      files = coverage_html.parse_lcov(str(path))
    self.assertEqual(files, {"a/one.py": {1: 3, 2: 0, 5: 1}, "b/two.ts": {1: 0}})

  def test_repeated_records_merge_with_max(self):
    # The merged lcov this consumes already merges per-lane counts, but a
    # file appearing twice must not silently lose its higher count.
    with tempfile.TemporaryDirectory() as scratch:
      path = Path(scratch) / "merged.lcov"
      path.write_text("SF:x.py\nDA:1,0\nend_of_record\nSF:x.py\nDA:1,4\nend_of_record\n")
      self.assertEqual(coverage_html.parse_lcov(str(path)), {"x.py": {1: 4}})


class BuildReportTest(unittest.TestCase):
  def _report(self, scratch):
    root = Path(scratch)
    (root / "a").mkdir()
    (root / "a" / "one.py").write_text("one\ntwo\nthree\nfour\nfive\n")
    lcov = root / "merged.lcov"
    lcov.write_text(LCOV)
    return coverage_html.build_report(coverage_html.parse_lcov(str(lcov)), str(root))

  def test_totals_count_only_instrumented_lines(self):
    with tempfile.TemporaryDirectory() as scratch:
      report = self._report(scratch)
    # 4 instrumented lines across both files, 2 of them hit. The source file
    # has 5 lines; the two without DA: records are not in the denominator.
    self.assertEqual((report["totalFound"], report["totalHit"]), (4, 2))

  def test_unreadable_source_still_counts(self):
    with tempfile.TemporaryDirectory() as scratch:
      report = self._report(scratch)
    missing = [entry for entry in report["files"] if entry["path"] == "b/two.ts"][0]
    self.assertIsNone(missing["source"])
    self.assertEqual((missing["found"], missing["hit"]), (1, 0))


class MarkdownTest(unittest.TestCase):
  def _markdown(self):
    with tempfile.TemporaryDirectory() as scratch:
      lcov = Path(scratch) / "merged.lcov"
      lcov.write_text(LCOV)
      report = coverage_html.build_report(coverage_html.parse_lcov(str(lcov)), scratch)
    return coverage_html.render_markdown(report, "Coverage report", "https://example/report")

  def test_headline_link_and_tables(self):
    markdown = self._markdown()
    self.assertIn("**50.0%**", markdown)
    self.assertIn("[Open the browsable HTML report](https://example/report)", markdown)
    self.assertIn("| `a/` | 3 | 2 | 1 |", markdown)
    self.assertIn("<details><summary>", markdown)

  def test_least_covered_file_is_listed_first(self):
    markdown = self._markdown()
    rows = [line for line in markdown.splitlines() if line.startswith("| `") and ".py`" in line
            or line.startswith("| `") and ".ts`" in line]
    self.assertTrue(rows[0].startswith("| `b/two.ts`"), rows)

  def test_uncovered_lines_are_collapsed_into_ranges(self):
    self.assertEqual(coverage_html._uncovered_ranges({"1": 0, "2": 0, "3": 1, "9": 0}),
                     ["1-2", "9"])


class ExcerptTest(unittest.TestCase):
  def _entry(self, counts, source):
    found = len(counts)
    return {
      "path": "a/one.py",
      "found": found,
      "hit": sum(1 for value in counts.values() if value > 0),
      "counts": {str(number): value for number, value in counts.items()},
      "source": source,
    }

  def test_uncovered_lines_are_marked_and_given_context(self):
    entry = self._entry({1: 1, 5: 0}, [f"line{n}" for n in range(1, 11)])
    hunks = coverage_html._annotated_hunks(entry)
    self.assertIn("- 5 | line5", hunks)
    self.assertIn("  2 | line2", hunks)     # context before
    self.assertIn("  8 | line8", hunks)     # context after
    self.assertNotIn("line9", hunks)        # beyond the context window

  def test_adjacent_regions_merge_into_one_hunk(self):
    entry = self._entry({2: 0, 4: 0}, [f"line{n}" for n in range(1, 11)])
    self.assertNotIn("@@", coverage_html._annotated_hunks(entry))

  def test_distant_regions_stay_separate(self):
    entry = self._entry({2: 0, 40: 0}, [f"line{n}" for n in range(1, 51)])
    self.assertIn("@@", coverage_html._annotated_hunks(entry))

  def test_fully_covered_or_sourceless_files_are_skipped(self):
    source = ["line1", "line2"]
    self.assertIsNone(coverage_html._annotated_hunks(self._entry({1: 1}, source)))
    self.assertIsNone(coverage_html._annotated_hunks(self._entry({1: 0}, None)))

  def test_excerpts_never_exceed_the_summary_budget(self):
    # Many large files: the renderer must drop blocks and say how many, not
    # silently emit a summary GitHub will reject wholesale.
    entries = []
    for index in range(60):
      counts = {number: 0 for number in range(1, 400)}
      entry = self._entry(counts, [f"a long source line number {n}" for n in range(1, 400)])
      entry["path"] = f"big/file{index}.py"
      entries.append(entry)
    rendered = "\n".join(coverage_html._render_excerpts(entries))
    self.assertLess(len(rendered), coverage_html._MAX_EXCERPT_BYTES + 5000)
    self.assertIn("omitted to stay within GitHub's job-summary size limit", rendered)


class HtmlTest(unittest.TestCase):
  def _render(self, source_text):
    with tempfile.TemporaryDirectory() as scratch:
      root = Path(scratch)
      (root / "a").mkdir()
      (root / "a" / "one.py").write_text(source_text)
      lcov = root / "merged.lcov"
      lcov.write_text(LCOV)
      out = root / "out" / "coverage.html"
      # main() reports the written paths on stdout for repo.sh; swallow it so
      # unittest output stays readable.
      with contextlib.redirect_stdout(io.StringIO()):
        coverage_html.main([str(lcov), str(out), "--root", str(root)])
      return out.read_text()

  def _payload(self, page):
    match = re.search(r'id="coverage-data">(.*?)</script>', page, re.S)
    return json.loads(match.group(1).replace("<\\/", "</"))

  def test_report_is_one_self_contained_file(self):
    page = self._render("one\ntwo\nthree\nfour\nfive\n")
    self.assertNotIn("<link", page)
    self.assertNotIn("src=", page)
    self.assertNotIn("http://", page)

  def test_source_closing_script_tag_cannot_truncate_the_payload(self):
    # A TypeScript/HTML source line containing "</script>" would otherwise end
    # the JSON block early and break the entire page.
    page = self._render('const bad = "</script>";\nsecond\n')
    payload = self._payload(page)
    entry = [f for f in payload["files"] if f["path"] == "a/one.py"][0]
    self.assertEqual(entry["source"][0], 'const bad = "</script>";')

  def test_output_is_deterministic(self):
    first = self._render("one\ntwo\n")
    second = self._render("one\ntwo\n")
    self.assertEqual(first, second)


if __name__ == "__main__":
  unittest.main()
