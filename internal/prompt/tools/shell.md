Runs a command with {{shell}} in the workspace root and returns its combined output and exit code.

- `command`: the command to run.
- `timeout_seconds`: how long it may run, at most 600; 120 when omitted. A command that runs out of time is stopped along with every process it started.
- Commands get no input, so anything interactive fails. Don't start servers or anything else that keeps running; there is no background mode.
- Output is capped at 30,000 characters, keeping the start and the end. Prefer narrow commands, and the read_file, grep and glob tools for reading and searching files.

{{notes}}
