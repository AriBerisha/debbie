Reads a text file and returns its lines, numbered, or shows you an image.

- `path`: the file, relative to the workspace root (absolute paths work too).
- `offset`: the line number to start at, counting from 1. `limit`: how many lines to return.
- Without them, the first 2000 lines are returned. Lines longer than 2000 characters are cut. The output says which lines it shows and how many the file has; read on with `offset` and `limit`.
- A PNG, JPEG, GIF or WebP image comes back as a picture if this model can see images, scaled down when it is very large; if it can't, the call says so.
- Other binary files are refused. Read a file before you edit it.
