# Workspace search tools

The built-in `glob` and `grep` tools provide deterministic, pageable workspace search. Both tools remain read-only and workspace-confined.

## Completion metadata and pagination

Legacy calls continue to return the existing plain-text format. A complete text result is unchanged. If a text page is incomplete, the tool appends a `[search_metadata {...}]` line containing:

- `matches`: results returned on this page
- `truncated`: always `true` when the marker is present
- `next_cursor`: an opaque continuation cursor

For a consistently structured response, pass `"output_format":"json"`. The result is a JSON object:

```json
{
  "matches": ["a.go", "pkg/b.go"],
  "truncated": true,
  "next_cursor": "...",
  "incomplete": false
}
```

If a grep input file cannot be searched within its safety bounds, `incomplete` is true and `omitted_files` identifies omitted files encountered on that page. Omission details are capped at 100 entries; `omitted_files_truncated` is true if more were encountered. This is distinct from `truncated`, which means another result page exists.

Pass `next_cursor` back as `cursor` with the same search arguments. Cursors are bound to the query and rejected if reused with different arguments. Pages are ordered by workspace-relative path and then line number, so an unchanged workspace does not duplicate or skip results. Cursors describe a position, not a snapshot: callers should restart a search after workspace mutations.

## Filters and ignore behavior

Both tools accept multiple `include` and `exclude` glob patterns. Includes use OR semantics; a path must match one include when any are supplied. Any matching exclude removes the path. `grep.glob` remains available as a compatibility alias for one additional include pattern.

By default, searches preserve the previous behavior:

- `.git`, `node_modules`, `vendor`, `.venv`, and `target` directories are skipped.
- `.gitignore` files are not applied.

Set `skip_default_dirs` to `false` to search the built-in skipped directories. Set `respect_gitignore` to `true` to apply workspace and nested `.gitignore` patterns, including negated (`!`) rules and Git's rule that a file cannot be re-included while a parent directory remains excluded. Ignore loading is capped at 10,000 rules; exceeding the cap returns an explicit tool error instead of a potentially incomplete result. Search never follows symlinks and retains the existing single-hardlink checks for file content reads.

## Grep result controls

`grep` supports:

- `before_context` and `after_context` (non-negative line counts)
- `mode: "matches"` (default), returning matching lines
- `mode: "files"`, returning each matching file once
- `mode: "count"`, returning per-file match counts

In structured match mode, each result has `path`, `line`, `text`, and optional `before`/`after` arrays. A page is capped at 1,000 results, before/after context at 10 lines each, displayed lines at 400 bytes, and each searched file at 10 MiB. Files containing a line over the scanner's 1 MiB safety bound are identified in text output and through structured incompleteness metadata. Matches safely scanned before an overlong line are retained.

## Migration guidance

No migration is required for callers that consume complete plain-text results. Callers that need correctness at result limits should either:

1. request JSON and follow `next_cursor` until `truncated` is false, or
2. detect the text metadata marker and continue with its cursor.

New integrations should prefer JSON because completion state is present even for the final page (`"truncated":false`).
