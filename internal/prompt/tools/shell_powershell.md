Notes for PowerShell:
- Single quotes are literal (double a single quote to include one); double quotes expand variables; the backtick escapes.
- Use @' ... '@ here-strings for text over several lines.
- Windows PowerShell 5.1 has no &&: to stop at a failure, check $LASTEXITCODE (or $?) before the next command.
- Bash syntax doesn't work.
