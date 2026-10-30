package browser

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"debbie/extension"
	"debbie/internal/app"
	"debbie/internal/config"
)

// Installer installs the extension and its native messaging host (SPEC
// §20.7). Tests replace its places and the registration.
type Installer struct {
	Exe     string // the binary that becomes the host
	Data    string // the extension and the host go under it
	KeyFile string // the extension's private key, which fixes its ID

	Register   func(name, manifest string) error
	Unregister func(name string) error
	Registered func(name string) (string, error) // the manifest path registered; "" for none
	Where      string                            // where the registration goes, for messages
}

// NewInstaller returns an installer for this user and this binary.
func NewInstaller() (*Installer, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	data, err := dataDir()
	if err != nil {
		return nil, err
	}
	key, err := keyPath()
	if err != nil {
		return nil, err
	}
	return &Installer{Exe: exe, Data: data, KeyFile: key,
		Register: registerHost, Unregister: unregisterHost, Registered: registeredHost,
		Where: hostRegistration(HostName)}, nil
}

// ExtensionDir is where the extension is written, for Chrome to load.
func (in *Installer) ExtensionDir() string { return filepath.Join(in.Data, "extension") }

// HostDir holds the host program and its manifest.
func (in *Installer) HostDir() string { return filepath.Join(in.Data, "host") }

// HostExe is the host program: a copy of the binary, so that rebuilding or
// replacing the binary works while Chrome runs the host.
func (in *Installer) HostExe() string {
	exe := app.Name
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	return filepath.Join(in.HostDir(), exe)
}

// HostManifest is the native messaging host's manifest.
func (in *Installer) HostManifest() string { return filepath.Join(in.HostDir(), HostName+".json") }

// Installed is what Install did.
type Installed struct {
	ID        string   // the extension's ID
	Extension string   // the folder Chrome loads
	Sites     []string // the sites it allows to begin with
	// Former is the folder of the copy a former name of the program
	// installed (app.FormerNames), which Chrome may still have loaded: the
	// new folder takes its place. "" when there was none.
	Former string
}

// VersionName is the version of the extension this binary installs, with
// a hash of its files: "0.1.0 build 3f9a1c2b". The install writes it into
// the manifest's version_name, which the extension reports when it
// connects, so a Chrome still running an older copy shows (Chrome keeps
// running the old one until the extension is reloaded).
func VersionName() string {
	h := sha256.New()
	fs.WalkDir(extension.Files, ".", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return nil
		}
		data, _ := fs.ReadFile(extension.Files, path)
		fmt.Fprintf(h, "%s\x00%d\x00", path, len(data))
		h.Write(data)
		return nil
	})
	var m struct {
		Version string `json:"version"`
	}
	data, _ := fs.ReadFile(extension.Files, "manifest.json")
	json.Unmarshal(data, &m)
	return fmt.Sprintf("%s build %x", m.Version, h.Sum(nil)[:4])
}

// ExtensionName is the extension's name as Chrome shows it on its card in
// chrome://extensions: the manifest's name.
func ExtensionName() string {
	var m struct {
		Name string `json:"name"`
	}
	data, _ := fs.ReadFile(extension.Files, "manifest.json")
	json.Unmarshal(data, &m)
	return m.Name
}

// Install writes the extension and the host, and registers the host with
// Chrome. Running it again updates them; the extension's ID stays.
func (in *Installer) Install() (*Installed, error) {
	pub, err := in.publicKey(true)
	if err != nil {
		return nil, err
	}
	id := ExtensionID(pub)
	if err := in.writeExtension(pub); err != nil {
		return nil, fmt.Errorf("writing the extension: %w", err)
	}
	if err := in.copyHost(); err != nil {
		return nil, fmt.Errorf("copying the host program: %w", err)
	}
	manifest, err := json.MarshalIndent(map[string]any{
		"name":            HostName,
		"description":     app.Name + " browser bridge",
		"path":            in.HostExe(),
		"type":            "stdio",
		"allowed_origins": []string{"chrome-extension://" + id + "/"},
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(in.HostManifest(), append(manifest, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("writing the host manifest: %w", err)
	}
	if err := in.Register(HostName, in.HostManifest()); err != nil {
		return nil, fmt.Errorf("registering the host at %s: %w", in.Where, err)
	}
	return &Installed{ID: id, Extension: in.ExtensionDir(), Sites: defaultSites(), Former: in.dropFormer()}, nil
}

// dropFormer takes away what a former name of the program installed: its
// host's registration, the host and the extension's copy. The key, and so
// the extension's ID, carried over with the config directory, so Chrome
// loads the new folder in the old copy's place; the old copy finds no host
// any more. A host Chrome still runs stays until Chrome closes, and the
// folder of a session still running stays too. It returns the former
// extension folder, or "".
func (in *Installer) dropFormer() string {
	found := ""
	for _, name := range app.FormerNames {
		host := strings.ReplaceAll(name, "-", "_")
		if reg, err := in.Registered(host); err == nil && reg != "" {
			in.Unregister(host)
		}
		data := filepath.Join(filepath.Dir(in.Data), name)
		if fi, err := os.Stat(filepath.Join(data, "extension")); err == nil && fi.IsDir() {
			found = filepath.Join(data, "extension")
		}
		for _, sub := range []string{"extension", "host"} {
			os.RemoveAll(filepath.Join(data, sub))
		}
		os.Remove(data) // only when nothing is left in it
	}
	return found
}

// Uninstall unregisters the host and removes the extension and the host.
// The key stays, so a later install gets the same ID. A host that Chrome
// still runs can't be removed on Windows; the error says so.
func (in *Installer) Uninstall() error {
	var errs []error
	if err := in.Unregister(HostName); err != nil {
		errs = append(errs, fmt.Errorf("removing %s: %w", in.Where, err))
	}
	if err := os.RemoveAll(in.ExtensionDir()); err != nil {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(in.HostDir()); err != nil {
		errs = append(errs, fmt.Errorf("%w (Chrome may still run the host: close Chrome and run uninstall again)", err))
	}
	return errors.Join(errs...)
}

// Check is one part of the install, for "browser status".
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Status checks each part of the install.
func (in *Installer) Status() []Check {
	var checks []Check
	add := func(name string, ok bool, detail string) { checks = append(checks, Check{name, ok, detail}) }

	pub, err := in.publicKey(false)
	id := ""
	switch {
	case err != nil:
		add("key", false, err.Error())
	case pub == nil:
		add("key", false, "none yet: run "+app.Name+" browser install")
	default:
		id = ExtensionID(pub)
		add("key", true, "the extension's ID is "+id)
	}

	manifest, err := os.ReadFile(filepath.Join(in.ExtensionDir(), "manifest.json"))
	switch {
	case err != nil:
		add("extension", false, "not written: run "+app.Name+" browser install")
	case pub != nil && !bytes.Contains(manifest, []byte(base64.StdEncoding.EncodeToString(pub))):
		add("extension", false, in.ExtensionDir()+" has another key: run "+app.Name+" browser install")
	default:
		add("extension", true, in.ExtensionDir())
	}

	switch same, err := sameFile(in.Exe, in.HostExe()); {
	case errors.Is(err, fs.ErrNotExist):
		add("host", false, "not installed: run "+app.Name+" browser install")
	case err != nil:
		add("host", false, err.Error())
	case !same:
		add("host", false, in.HostExe()+" is another build than this binary: run "+app.Name+" browser install to update it")
	default:
		add("host", true, in.HostExe())
	}

	var hm struct {
		Path    string   `json:"path"`
		Origins []string `json:"allowed_origins"`
	}
	data, err := os.ReadFile(in.HostManifest())
	if err == nil {
		err = json.Unmarshal(data, &hm)
	}
	switch {
	case err != nil:
		add("host manifest", false, "missing or unreadable: run "+app.Name+" browser install")
	case hm.Path != in.HostExe() || len(hm.Origins) != 1 || id != "" && hm.Origins[0] != "chrome-extension://"+id+"/":
		add("host manifest", false, in.HostManifest()+" doesn't match: run "+app.Name+" browser install")
	default:
		add("host manifest", true, in.HostManifest())
	}

	switch reg, err := in.Registered(HostName); {
	case err != nil:
		add("registration", false, err.Error())
	case reg == "":
		add("registration", false, in.Where+" is missing: run "+app.Name+" browser install")
	case runtime.GOOS == "windows" && reg != in.HostManifest():
		add("registration", false, in.Where+" points to "+reg+": run "+app.Name+" browser install")
	default:
		add("registration", true, in.Where)
	}
	return checks
}

// publicKey returns the DER form of the extension's public key, making the
// key pair the first time when create is set; nil when there is none.
func (in *Installer) publicKey(create bool) ([]byte, error) {
	data, err := os.ReadFile(in.KeyFile)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, nil
		}
		return in.newKey()
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s isn't a PEM private key", in.KeyFile)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", in.KeyFile, err)
	}
	rk, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s isn't an RSA key", in.KeyFile)
	}
	return x509.MarshalPKIXPublicKey(&rk.PublicKey)
}

func (in *Installer) newKey() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := config.WritePrivate(in.KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		return nil, fmt.Errorf("saving the extension's key: %w", err)
	}
	return x509.MarshalPKIXPublicKey(&key.PublicKey)
}

// writeExtension writes the embedded extension, its manifest carrying the
// public key, and removes files a former version left.
func (in *Installer) writeExtension(pub []byte) error {
	dir := in.ExtensionDir()
	wrote := map[string]bool{}
	err := fs.WalkDir(extension.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, filepath.FromSlash(path)), 0o755)
		}
		data, err := fs.ReadFile(extension.Files, path)
		if err != nil {
			return err
		}
		if path == "manifest.json" {
			if data, err = withKey(data, pub); err != nil {
				return err
			}
		}
		wrote[filepath.FromSlash(path)] = true
		return os.WriteFile(filepath.Join(dir, filepath.FromSlash(path)), data, 0o644)
	})
	if err != nil {
		return err
	}
	// What a former version wrote and this one doesn't.
	var stale []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if rel, _ := filepath.Rel(dir, path); !wrote[rel] {
				stale = append(stale, path)
			}
		}
		return nil
	})
	for _, p := range stale {
		os.Remove(p)
	}
	return nil
}

// withKey puts the public key into the manifest as its first field, so the
// unpacked extension keeps its ID (developer.chrome.com, manifest "key"),
// and the build as its version_name.
func withKey(manifest, pub []byte) ([]byte, error) {
	rest, ok := bytes.CutPrefix(bytes.TrimSpace(manifest), []byte("{"))
	if !ok {
		return nil, errors.New("the extension's manifest isn't a JSON object")
	}
	out := fmt.Appendf(nil, "{\n  \"key\": %q,\n  \"version_name\": %q,%s\n",
		base64.StdEncoding.EncodeToString(pub), VersionName(), rest)
	if !json.Valid(out) {
		return nil, errors.New("the extension's manifest with its key isn't valid JSON")
	}
	return out, nil
}

// copyHost copies the binary to the host's place. A host Chrome runs can't
// be overwritten on Windows, but it can be renamed: the old copy moves
// aside, and is removed by a later install once nothing runs it.
func (in *Installer) copyHost() error {
	if err := os.MkdirAll(in.HostDir(), 0o755); err != nil {
		return err
	}
	target := in.HostExe()
	if same, err := sameFile(in.Exe, target); err == nil && same {
		return nil
	}
	src, err := os.Open(in.Exe)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(in.HostDir(), ".host-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		aside := fmt.Sprintf("%s.old-%d", target, time.Now().UnixNano())
		if os.Rename(target, aside) != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), target); err != nil {
			return err
		}
	}
	old, _ := filepath.Glob(target + ".old-*")
	for _, p := range old {
		os.Remove(p) // fails while Chrome still runs it
	}
	return nil
}

// sameFile reports whether two files hold the same bytes.
func sameFile(a, b string) (bool, error) {
	ha, err := fileHash(a)
	if err != nil {
		return false, err
	}
	hb, err := fileHash(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func fileHash(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// defaultSites are the sites the extension allows until the user changes
// them, as rules.js says.
func defaultSites() []string {
	data, err := fs.ReadFile(extension.Files, "rules.js")
	if err != nil {
		return nil
	}
	_, rest, ok := strings.Cut(string(data), "DEFAULT_SITES = [")
	if !ok {
		return nil
	}
	list, _, _ := strings.Cut(rest, "]")
	var sites []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.Trim(strings.TrimSpace(s), `"'`); s != "" {
			sites = append(sites, s)
		}
	}
	return sites
}
