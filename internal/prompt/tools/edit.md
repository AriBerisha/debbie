Replaces text in a file. `old_string` must match exactly one place in the file, unless `replace_all` is true.

- `path`: the file to change.
- `old_string`: the exact text to replace, with its indentation. Include a few lines around the change so that it matches only one place.
- `new_string`: the replacement, with the indentation it should have.
- `replace_all`: replace every match instead of exactly one.
- Write the strings literally: a newline is a real newline, not `\n`. Copy the text from the file without the line numbers that read_file adds.
- An empty `old_string` creates a new file. An empty `new_string` deletes the text.
- When nothing matches, the error shows the most similar lines; read the file again and retry.
