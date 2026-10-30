// Package install puts the program where a terminal finds it, for the
// user alone and without administrator rights (SPEC §16.1): the binary in a
// folder of the user's own, which it adds to their PATH, so that typing
// "debbie" starts it. Uninstall takes both away again.
package install

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"debbie/internal/app"
)

// Installer installs the binary Exe into Dir and puts Dir on the PATH.
type Installer struct {
	Exe  string // the binary to install: the one running
	Dir  string // the folder it goes in
	GOOS string // whose ways to follow: "windows", "darwin", "linux"…

	// Where the PATH is kept. Windows keeps the user's in the registry; a
	// Unix shell sets it in a start-up file in Home, the one for Shell.
	Home        string
	Shell       string                 // $SHELL
	Path        string                 // $PATH as this process has it
	UserPath    func() (string, error) // Windows: the user's PATH
	SetUserPath func(string) error     // Windows: sets it, and tells programs it changed
}

// Done is what an install or an uninstall did, a line each.
type Done struct {
	Lines    []string
	NewShell bool // the PATH changed: terminals opened before don't have it
}

func (d *Done) add(format string, a ...any) { d.Lines = append(d.Lines, fmt.Sprintf(format, a...)) }

// New returns an installer for the binary running, into the usual folder:
// %LOCALAPPDATA%\Programs\debbie on Windows, ~/.local/bin elsewhere
// ($XDG_BIN_HOME when it is set).
func New() (*Installer, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	in := &Installer{Exe: exe, GOOS: runtime.GOOS, Home: home, Shell: os.Getenv("SHELL"), Path: os.Getenv("PATH")}
	// The user's PATH is written back as it was read: REG_EXPAND_SZ, as
	// Windows keeps it, unless it was plain REG_SZ.
	expand := true
	in.UserPath = func() (string, error) {
		v, e, err := readUserPath()
		if err == nil {
			expand = e
		}
		return v, err
	}
	in.SetUserPath = func(v string) error { return writeUserPath(v, expand) }
	switch {
	case in.GOOS == "windows":
		local := os.Getenv("LOCALAPPDATA")
		if !filepath.IsAbs(local) {
			local = filepath.Join(home, "AppData", "Local")
		}
		in.Dir = filepath.Join(local, "Programs", app.Name)
	case filepath.IsAbs(os.Getenv("XDG_BIN_HOME")):
		in.Dir = os.Getenv("XDG_BIN_HOME")
	default:
		in.Dir = filepath.Join(home, ".local", "bin")
	}
	return in, nil
}

// Command is the installed program's path.
func (in *Installer) Command() string {
	name := app.Name
	if in.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(in.Dir, name)
}

// Install copies the binary into Dir and, with addPath, puts Dir on the
// PATH. Running it again updates the copy; the PATH changes once.
func (in *Installer) Install(addPath bool) (Done, error) {
	var done Done
	if err := os.MkdirAll(in.Dir, 0o755); err != nil {
		return done, err
	}
	dst := in.Command()
	if same(in.Exe, dst) {
		done.add("%s is installed in %s already.", app.Name, in.Dir)
	} else {
		if err := in.place(dst); err != nil {
			return done, fmt.Errorf("installing %s: %w", dst, err)
		}
		done.add("Installed %s", dst)
	}
	if !addPath {
		return done, nil
	}
	return done, in.addToPath(&done)
}

// place copies the binary to dst through a new file renamed over it. A
// copy that is running can't be replaced on Windows, but it can be
// renamed: it moves aside, and goes at the next install, once nothing
// runs it.
func (in *Installer) place(dst string) error {
	data, err := os.ReadFile(in.Exe)
	if err != nil {
		return err
	}
	in.clearAside(dst)
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	err = os.Rename(tmp, dst)
	if err != nil && in.GOOS == "windows" {
		if os.Rename(dst, aside(dst)) == nil {
			err = os.Rename(tmp, dst)
		}
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// aside is the name a running copy moves to.
func aside(p string) string { return fmt.Sprintf("%s.old-%d", p, time.Now().UnixNano()) }

// clearAside removes the copies of p moved aside before; those still
// running stay.
func (in *Installer) clearAside(p string) {
	entries, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(p)+".old-") {
			os.Remove(filepath.Join(filepath.Dir(p), e.Name()))
		}
	}
}

// same reports whether a and b are the same file.
func same(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// marker starts the lines an install adds to a shell's start-up file.
const marker = "# Added by " + app.Name + " install: puts its folder on the PATH."

// addToPath puts Dir on the PATH: the user's PATH in the registry on
// Windows; elsewhere a line in the start-up file of the user's shell,
// unless the PATH has Dir already, as ~/.local/bin often is.
func (in *Installer) addToPath(done *Done) error {
	if in.GOOS == "windows" {
		cur, err := in.UserPath()
		if err != nil {
			return fmt.Errorf("reading your PATH: %w", err)
		}
		if hasDir(cur, in.Dir, true) {
			done.add("%s is on your PATH already.", in.Dir)
			return nil
		}
		next := in.Dir
		if t := strings.TrimRight(cur, ";"); t != "" {
			next = t + ";" + in.Dir
		}
		if err := in.SetUserPath(next); err != nil {
			return fmt.Errorf("adding %s to your PATH: %w", in.Dir, err)
		}
		done.add(`Added %s to your PATH (HKCU\Environment).`, in.Dir)
		done.NewShell = true
		return nil
	}
	if hasDir(in.Path, in.Dir, false) {
		done.add("%s is on your PATH already.", in.Dir)
		return nil
	}
	file, line := in.startupFile()
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	done.NewShell = true
	if strings.Contains(string(data), marker) {
		done.add("%s puts %s on your PATH already.", file, in.Dir)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	sep := ""
	switch {
	case len(data) > 0 && !strings.HasSuffix(string(data), "\n"):
		sep = "\n\n"
	case len(data) > 0:
		sep = "\n"
	}
	_, err = fmt.Fprintf(f, "%s%s\n%s\n", sep, marker, line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	done.add("Added %s to your PATH in %s.", in.Dir, file)
	return nil
}

// startupFile is the start-up file of the user's shell that sets the PATH,
// and the line that adds Dir to it.
func (in *Installer) startupFile() (file, line string) {
	dir := in.Dir
	if rel, err := filepath.Rel(in.Home, in.Dir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		dir = "$HOME/" + filepath.ToSlash(rel)
	}
	export := `export PATH="` + dir + `:$PATH"`
	switch filepath.Base(in.Shell) {
	case "fish":
		return filepath.Join(in.Home, ".config", "fish", "conf.d", app.Name+".fish"), `fish_add_path "` + dir + `"`
	case "zsh":
		return filepath.Join(in.Home, ".zshrc"), export
	case "bash":
		if in.GOOS == "darwin" { // Terminal starts login shells, which read .bash_profile
			return filepath.Join(in.Home, ".bash_profile"), export
		}
		return filepath.Join(in.Home, ".bashrc"), export
	}
	return filepath.Join(in.Home, ".profile"), export
}

// startupFiles are every start-up file an install may have written to.
func (in *Installer) startupFiles() []string {
	return []string{
		filepath.Join(in.Home, ".zshrc"), filepath.Join(in.Home, ".bashrc"), filepath.Join(in.Home, ".bash_profile"),
		filepath.Join(in.Home, ".profile"), filepath.Join(in.Home, ".config", "fish", "conf.d", app.Name+".fish"),
	}
}

// hasDir reports whether a PATH list holds dir: on Windows ignoring case
// and expanding %VARIABLES%, as Windows does.
func hasDir(list, dir string, windows bool) bool {
	sep := ":"
	if windows {
		sep = ";"
	}
	want := cleanDir(dir, windows)
	for _, p := range strings.Split(list, sep) {
		if windows {
			p = expandPercent(p)
		}
		if strings.TrimSpace(p) != "" && cleanDir(p, windows) == want {
			return true
		}
	}
	return false
}

func cleanDir(p string, windows bool) string {
	p = strings.TrimRight(filepath.Clean(strings.Trim(strings.TrimSpace(p), `"`)), `\/`)
	if windows {
		p = strings.ToLower(p)
	}
	return p
}

// expandPercent expands %NAME% as Windows does in a REG_EXPAND_SZ value.
func expandPercent(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '%')
		if i < 0 {
			return b.String() + s
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			return b.String() + s
		}
		name := s[i+1 : i+1+j]
		b.WriteString(s[:i])
		if v, ok := os.LookupEnv(name); ok && name != "" {
			b.WriteString(v)
		} else {
			b.WriteString(s[i : i+2+j])
		}
		s = s[i+2+j:]
	}
}

// Uninstall removes the installed program and takes Dir off the PATH. On
// Windows the folder is the program's own and goes too. The settings and
// the sessions stay.
func (in *Installer) Uninstall() (Done, error) {
	var done Done
	cmd := in.Command()
	switch err := os.Remove(cmd); {
	case err == nil:
		done.add("Removed %s", cmd)
	case errors.Is(err, fs.ErrNotExist):
		done.add("%s isn't installed in %s.", app.Name, in.Dir)
	case in.GOOS == "windows" && os.Rename(cmd, aside(cmd)) == nil:
		done.add("Removed %s; the copy running now goes when it ends.", cmd)
	default:
		return done, err
	}
	in.clearAside(cmd)
	if in.GOOS == "windows" {
		os.Remove(in.Dir) // when nothing is left in it
		cur, err := in.UserPath()
		if err != nil {
			return done, fmt.Errorf("reading your PATH: %w", err)
		}
		if hasDir(cur, in.Dir, true) {
			var keep []string
			for _, p := range strings.Split(cur, ";") {
				if !hasDir(p, in.Dir, true) {
					keep = append(keep, p)
				}
			}
			if err := in.SetUserPath(strings.Join(keep, ";")); err != nil {
				return done, fmt.Errorf("taking %s off your PATH: %w", in.Dir, err)
			}
			done.add("Took %s off your PATH.", in.Dir)
		}
		return done, nil
	}
	for _, file := range in.startupFiles() {
		data, err := os.ReadFile(file)
		if err != nil || !strings.Contains(string(data), marker) {
			continue
		}
		if filepath.Ext(file) == ".fish" {
			os.Remove(file)
		} else if err := os.WriteFile(file, []byte(withoutMarked(string(data))), 0o644); err != nil {
			return done, err
		}
		done.add("Took %s off your PATH in %s.", in.Dir, file)
	}
	return done, nil
}

// withoutMarked removes the lines an install added: the marker, the line
// after it, and the blank line before it.
func withoutMarked(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		if lines[i] == marker {
			if n := len(out); n > 0 && out[n-1] == "" {
				out = out[:n-1]
			}
			i++ // the line it marks
			continue
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n")
}
