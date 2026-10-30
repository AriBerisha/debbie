Searches file contents with a regular expression and returns the matching lines as `path:line:text`. The syntax is Go's RE2: no backreferences or lookaround. Matching ignores case unless `case_sensitive` is true. Binary files and files that .gitignore files exclude are skipped. At most 200 matching lines are returned, each cut at 300 characters; narrow the search with `path`, `include` or a more specific pattern.

- `pattern`: the regular expression.
- `path`: a directory or file to search; the workspace root when omitted.
- `include`: a glob that paths must match, such as `*.go` or `src/**/*.ts`. `exclude`: a glob for paths to skip.
