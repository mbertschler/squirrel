#!/usr/bin/env python3
"""Report audit-document findings whose issue closed without annotation.

Tier 2 of #193. `design/friction-log.md` and `SAFETY-AUDIT.md` record
findings that each reference the issue tracking their fix. A finding goes
stale the moment that issue closes and nobody strikes the entry through:
the document keeps reading as a list of open problems long after they
shipped, which is exactly how nineteen friction-log entries and ~8 audit
findings ended up describing a squirrel that no longer exists.

That is checkable, but not by a unit test — it needs issue state. This
script scans the documents for `#N` references, asks GitHub which are
closed, and reports every block that links a closed issue while carrying
no resolution marker. It reports; it never edits a document. Judging
whether a finding is really resolved stays human.

Requires the `gh` CLI (authenticated) on PATH, as on a GitHub runner.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import tempfile
from datetime import datetime, timezone

# Documents whose findings carry issue references.
DOCUMENTS = ["SAFETY-AUDIT.md"]
DOCUMENT_GLOBS = ["design/*.md"]

# The tracking issue this script owns. Matched by exact title so a run
# updates its own report instead of opening a new one every week.
REPORT_TITLE = "Docs audit: findings whose issue has closed without annotation"

# An issue reference: `#123`, but not a fragment (`docs#3`) or a path.
ISSUE_RE = re.compile(r"(?:^|[^\w/#])#(\d{1,6})\b")

# A markdown ATX heading. The trailing space matters: a paragraph opening
# with `#179 landed the filters` is prose, not a heading.
HEADING_RE = re.compile(r"^(#{1,6})\s+(.*)$")

# A list-item opener. Each item is its own finding in the SAFETY-AUDIT
# summary index, so items break blocks the way blank lines do.
LIST_ITEM_RE = re.compile(r"^\s{0,3}(?:[-*+]|\d+\.)\s")

# Markers that say "this finding is settled": a strikethrough, a tick, the
# SAFETY-AUDIT status line, or a resolution word near the reference itself
# ("fixed in #171", "*Resolved (#158, …)*", "Partially addressed in #176").
RESOLVED_RE = re.compile(
    r"~~|✅|\*\*status:\*\*\s*resolved|"
    r"\b(?:fixed|resolved|closed|shipped|landed|addressed|superseded)\b[^\n]{0,40}#\d",
    re.IGNORECASE,
)


class Block:
    """One paragraph or list item, with the heading it sits under."""

    def __init__(self, path: str, line: int, heading: str, scope: str):
        self.path = path
        self.line = line
        self.heading = heading
        # scope is the unit a resolution marker annotates: a whole `###`
        # finding section where the document has them (SAFETY-AUDIT keeps
        # `**Status:** Resolved` in a paragraph of its own, away from the
        # `**Issue:** #N` line), the block itself where it does not
        # (friction-log entries are peer paragraphs under one `##`
        # checkpoint, so section scope would annotate its neighbours).
        self.scope = scope
        self.scope_annotated = False
        self.lines: list[str] = []

    @property
    def text(self) -> str:
        return "\n".join(self.lines)

    def issues(self) -> set[int]:
        return {int(n) for n in ISSUE_RE.findall(self.text)}

    def annotated(self) -> bool:
        return self.scope_annotated

    def excerpt(self, limit: int = 220) -> str:
        flat = " ".join(self.text.split())
        return flat if len(flat) <= limit else flat[: limit - 1] + "…"


def parse_blocks(path: str, text: str) -> list[Block]:
    """Split a markdown document into findings and mark the settled ones.

    A block breaks on a blank line, a heading, or a new list item. Fenced
    code is skipped: a `#N` inside an example is not a finding reference.
    """
    blocks: list[Block] = []
    heading, scope = "", f"{path}:preamble"
    fenced = False
    current: Block | None = None
    for number, line in enumerate(text.splitlines(), start=1):
        if line.startswith("```"):
            fenced = not fenced
            current = None
            continue
        if fenced:
            continue
        if not line.strip():
            current = None
            continue
        match = HEADING_RE.match(line)
        if match:
            heading = line
            # Only a level-3-or-deeper heading delimits a single finding.
            scope = f"{path}#{number}" if len(match.group(1)) >= 3 else ""
            current = None
            continue
        if current is None or LIST_ITEM_RE.match(line):
            current = Block(path, number, heading, scope or f"{path}:{number}")
            blocks.append(current)
        current.lines.append(line)

    settled: set[str] = set()
    for block in blocks:
        if RESOLVED_RE.search(block.text) or RESOLVED_RE.search(block.heading):
            settled.add(block.scope)
    for block in blocks:
        block.scope_annotated = block.scope in settled
    return blocks


def collect_blocks(root: str) -> list[Block]:
    import glob

    paths: list[str] = []
    for name in DOCUMENTS:
        paths.append(os.path.join(root, name))
    for pattern in DOCUMENT_GLOBS:
        paths.extend(sorted(glob.glob(os.path.join(root, pattern))))

    blocks: list[Block] = []
    for path in paths:
        if not os.path.isfile(path):
            continue
        with open(path, encoding="utf-8") as fh:
            blocks.extend(parse_blocks(os.path.relpath(path, root), fh.read()))
    return blocks


def gh(*args: str, check: bool = True) -> str:
    result = subprocess.run(["gh", *args], capture_output=True, text=True)
    if check and result.returncode != 0:
        raise RuntimeError(f"gh {' '.join(args)}: {result.stderr.strip()}")
    return result.stdout


def closed_issues(repo: str, numbers: set[int]) -> set[int]:
    """Return the subset of numbers whose issue or PR is closed.

    One GraphQL query with an alias per number keeps a weekly run to a
    single API call. An unknown number (a reference to another repo, a
    typo) resolves to null and is treated as "not closed" — the audit
    reports stale documentation, not broken links, so a partial response
    with errors alongside is used as-is rather than failing the run. A
    response carrying no repository block at all is not partial, though:
    the query failed outright, and that raises rather than reading as an
    all-clear.
    """
    if not numbers:
        return set()
    owner, name = repo.split("/", 1)
    fields = " ".join(
        f'i{n}: issueOrPullRequest(number: {n}) {{ ... on Issue {{ state }} '
        f"... on PullRequest {{ state }} }}"
        for n in sorted(numbers)
    )
    query = f'query {{ repository(owner: "{owner}", name: "{name}") {{ {fields} }} }}'
    out = gh("api", "graphql", "-f", f"query={query}", check=False)
    if not out.strip():
        raise RuntimeError("gh api graphql returned nothing")
    data = json.loads(out)
    repository = (data.get("data") or {}).get("repository")
    if repository is None:
        # No repository block at all means the query failed as a whole
        # (rate limit, auth, a malformed alias), not that individual
        # numbers resolved to null. Reading that as "nothing is closed"
        # would file an empty report and close the tracking issue — a
        # false all-clear, which is the exact failure this audit exists
        # to catch. Fail the run and leave last week's report standing.
        raise RuntimeError(
            f"gh api graphql returned no repository data: {data.get('errors')}"
        )
    closed = set()
    for key, value in repository.items():
        if value and value.get("state") in ("CLOSED", "MERGED"):
            closed.add(int(key[1:]))
    return closed


def render(findings: list[tuple[Block, list[int]]], repo: str) -> str:
    stamp = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M UTC")
    run = os.environ.get("GITHUB_RUN_ID")
    lines = [
        "Findings in the audit documents that reference a **closed** issue but",
        "carry no resolution marker (`~~…~~`, `**Status:** Resolved`,",
        "`fixed in #N`). Each is either genuinely shipped — annotate it — or",
        "still open, in which case the issue closed too early.",
        "",
        "This report is regenerated on every run of the `docs-audit` workflow;",
        "it never edits a document. See AGENTS.md § Documentation.",
        "",
    ]
    by_file: dict[str, list[tuple[Block, list[int]]]] = {}
    for block, issues in findings:
        by_file.setdefault(block.path, []).append((block, issues))
    for path in sorted(by_file):
        lines.append(f"### `{path}`")
        lines.append("")
        for block, issues in by_file[path]:
            refs = ", ".join(f"#{n}" for n in sorted(issues))
            lines.append(f"- [ ] **L{block.line}** — closed: {refs}")
            if block.heading:
                lines.append(f"  under `{block.heading.strip()}`")
            lines.append(f"  > {block.excerpt()}")
        lines.append("")
    footer = f"_Generated {stamp}"
    if run:
        footer += f" by [this run](https://github.com/{repo}/actions/runs/{run})"
    lines.append(footer + "._")
    return "\n".join(lines)


def find_report_issue() -> int | None:
    listing = json.loads(
        gh("issue", "list", "--state", "open", "--limit", "100", "--json", "number,title")
    )
    for item in listing:
        if item["title"].strip().lower() == REPORT_TITLE.lower():
            return int(item["number"])
    return None


def publish(body: str, existing: int | None, clean: bool) -> None:
    with tempfile.NamedTemporaryFile("w", suffix=".md", delete=False) as fh:
        fh.write(body)
        path = fh.name
    try:
        if clean:
            if existing is not None:
                gh("issue", "comment", str(existing), "--body-file", path)
                gh("issue", "close", str(existing))
            return
        if existing is None:
            gh("issue", "create", "--title", REPORT_TITLE, "--body-file", path)
        else:
            gh("issue", "edit", str(existing), "--body-file", path)
    finally:
        os.unlink(path)


def main() -> int:
    repo = os.environ.get("GITHUB_REPOSITORY", "mbertschler/squirrel")
    root = os.environ.get("GITHUB_WORKSPACE", ".")
    blocks = collect_blocks(root)
    if not blocks:
        print("no documents scanned — check DOCUMENTS/DOCUMENT_GLOBS", file=sys.stderr)
        return 1

    referenced: set[int] = set()
    for block in blocks:
        if not block.annotated():
            referenced |= block.issues()
    closed = closed_issues(repo, referenced)

    findings = []
    for block in blocks:
        if block.annotated():
            continue
        stale = sorted(block.issues() & closed)
        if stale:
            findings.append((block, stale))

    print(f"scanned {len(blocks)} blocks, {len(referenced)} issue references, "
          f"{len(findings)} stale")
    for block, issues in findings:
        print(f"{block.path}:{block.line}: closed {issues}")

    if os.environ.get("DOCS_AUDIT_DRY_RUN"):
        print(render(findings, repo))
        return 0

    existing = find_report_issue()
    if findings:
        publish(render(findings, repo), existing, clean=False)
    else:
        publish(
            "No audit-document finding references a closed issue without a "
            "resolution marker. Closing this report; the `docs-audit` "
            "workflow opens a new one if that changes.",
            existing,
            clean=True,
        )
    return 0


if __name__ == "__main__":
    sys.exit(main())
