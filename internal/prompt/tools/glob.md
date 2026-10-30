Finds files whose paths match a glob pattern, most recently modified first. Files that .gitignore files exclude are skipped. At most 200 paths are returned.

- `pattern`: matched against paths relative to `path`, with `/` between directories. `*` and `?` stay within one directory name, `**` spans any number of directories, and `[abc]` is a character class. Examples: `**/*.go`, `src/**/test_*.py`, `*.md`.
- `path`: the directory to search; the workspace root when omitted.
