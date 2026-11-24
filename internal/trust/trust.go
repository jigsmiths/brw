// Package trust reads and writes brew's list of trusted taps and casks, so
// that brw loads from a tap only what brew would.
package trust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

type Kind string

// The kinds brw reads and writes, by brew's keys.
const (
	KindTap     Kind = "trustedtaps"
	KindFormula Kind = "trustedformulae"
	KindCask    Kind = "trustedcasks"
)

// Path is the trust store brew uses: in $XDG_CONFIG_HOME/homebrew, else
// $HOMEBREW_XDG_CONFIG_HOME/homebrew, else ~/.homebrew. brew sets
// HOMEBREW_USER_CONFIG_HOME itself, so that variable is not read.
func Path() string {
	for _, v := range []string{"XDG_CONFIG_HOME", "HOMEBREW_XDG_CONFIG_HOME"} {
		if d := os.Getenv(v); d != "" {
			return filepath.Join(d, "homebrew", "trust.json")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".homebrew", "trust.json")
}

// Required says whether taps must be trusted, as brew requires by default.
func Required() bool { return os.Getenv("HOMEBREW_NO_REQUIRE_TAP_TRUST") == "" }

// Store is brew's trust store, with its keys in the order it had them.
type Store struct {
	path    string
	keys    []string
	entries map[string][]string
}

// Load reads the store at path. A missing or garbled store trusts nothing,
// as brew reads it.
func Load(path string) (*Store, error) {
	s := &Store{path: path, entries: map[string][]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return s, nil
	}
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return &Store{path: path, entries: map[string][]string{}}, nil
		}
		key, _ := tok.(string)
		var values []string
		if err := d.Decode(&values); err != nil {
			return &Store{path: path, entries: map[string][]string{}}, nil
		}
		for i, v := range values {
			values[i] = strings.ToLower(v)
		}
		s.keys = append(s.keys, key)
		s.entries[key] = values
	}
	return s, nil
}

var (
	githubPrefix = regexp.MustCompile(`^(?:[a-z][a-z0-9+.-]*://)?(?:[^@/]+@)?github\.com[:/]`)
	remoteHost   = regexp.MustCompile(`^(?:[a-z][a-z0-9+.-]*://)?(?:[^@/]+@)?([^/:]+)`)
	schemeLike   = regexp.MustCompile(`^[^/]+:.`)
)

// normalizeRemote follows brew's Tap.normalize_remote, so that the forms of
// one repository's remote compare equal.
func normalizeRemote(remote string) string {
	remote = strings.ToLower(strings.TrimSpace(remote))
	remote = githubPrefix.ReplaceAllString(remote, "https://github.com/")
	m := remoteHost.FindStringSubmatch(remote)
	if m == nil || !slices.Contains([]string{"codeberg.org", "github.com", "gitlab.com"}, m[1]) {
		return remote
	}
	return strings.TrimSuffix(strings.TrimRight(remote, "/"), ".git")
}

func SameRemote(a, b string) bool { return normalizeRemote(a) == normalizeRemote(b) }

func defaultRemote(name string) string {
	user, repo, _ := strings.Cut(name, "/")
	return "https://github.com/" + user + "/homebrew-" + repo
}

func custom(name, remote string) bool {
	return remote != "" && normalizeRemote(remote) != normalizeRemote(defaultRemote(name))
}

// Reference is how the store names a tap: user/repo on its default remote,
// else its remote.
func Reference(name, remote string) string {
	if !custom(name, remote) {
		return strings.ToLower(name)
	}
	return strings.ToLower(remote)
}

func remoteReference(entry string) bool {
	return strings.HasPrefix(entry, "/") || strings.HasPrefix(entry, ".") || strings.HasPrefix(entry, "~") ||
		schemeLike.MatchString(entry)
}

// TapTrusted says whether the whole tap is trusted. homebrew's own taps
// always are.
func (s *Store) TapTrusted(name, remote string) bool {
	name = strings.ToLower(name)
	if strings.HasPrefix(name, "homebrew/") && !custom(name, remote) {
		return true
	}
	for _, e := range s.entries[string(KindTap)] {
		if remoteReference(e) {
			if remote != "" && normalizeRemote(e) == normalizeRemote(remote) {
				return true
			}
		} else if e == name && !custom(name, remote) {
			return true
		}
	}
	return false
}

func (s *Store) CaskTrusted(name, remote, token string) bool {
	return s.itemTrusted(KindCask, name, remote, token)
}

func (s *Store) FormulaTrusted(name, remote, formula string) bool {
	return s.itemTrusted(KindFormula, name, remote, formula)
}

// PartlyTrusted says whether any single formula or cask of the tap is
// trusted, which brew counts as the tap being partly trusted.
func (s *Store) PartlyTrusted(name, remote string) bool {
	return s.hasFrom(KindCask, name, remote) || s.hasFrom(KindFormula, name, remote)
}

func (s *Store) itemTrusted(kind Kind, name, remote, token string) bool {
	if s.TapTrusted(name, remote) {
		return true
	}
	name, token = strings.ToLower(name), strings.ToLower(token)
	entries := s.entries[string(kind)]
	if slices.Contains(entries, name+"/"+token) || slices.Contains(entries, Reference(name, remote)+"/"+token) {
		return true
	}
	if !custom(name, remote) {
		return false
	}
	return slices.ContainsFunc(entries, func(e string) bool {
		prefix, ok := strings.CutSuffix(e, "/"+token)
		return ok && normalizeRemote(prefix) == normalizeRemote(remote)
	})
}

func (s *Store) hasFrom(kind Kind, name, remote string) bool {
	ref := Reference(name, remote) + "/"
	full := strings.ToLower(name) + "/"
	return slices.ContainsFunc(s.entries[string(kind)], func(e string) bool {
		return strings.HasPrefix(e, ref) || strings.HasPrefix(e, full)
	})
}

func (s *Store) Entries(k Kind) []string { return slices.Clone(s.entries[string(k)]) }

// Trust adds an entry and writes the store. added is false when it was there.
func (s *Store) Trust(k Kind, name string) (added bool, err error) {
	name = strings.ToLower(name)
	key := string(k)
	if slices.Contains(s.entries[key], name) {
		return false, nil
	}
	if _, ok := s.entries[key]; !ok {
		s.keys = append(s.keys, key)
	}
	s.entries[key] = append(s.entries[key], name)
	slices.Sort(s.entries[key])
	return true, s.write()
}

// Untrust removes an entry and writes the store. removed is false when it
// was not there.
func (s *Store) Untrust(k Kind, name string) (removed bool, err error) {
	name = strings.ToLower(name)
	key := string(k)
	i := slices.Index(s.entries[key], name)
	if i < 0 {
		return false, nil
	}
	s.entries[key] = slices.Delete(s.entries[key], i, i+1)
	if len(s.entries[key]) == 0 {
		delete(s.entries, key)
		s.keys = slices.DeleteFunc(s.keys, func(k string) bool { return k == key })
	}
	return true, s.write()
}

// write writes the store as brew does: pretty JSON, private, never through a
// symlink or into a folder others can write. An empty store is removed.
func (s *Store) write() error {
	dir := filepath.Dir(s.path)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil { //fsx:allow brew's trust store, outside the prefix
			return err
		}
	}
	if err := secure(dir, "trust store directory"); err != nil {
		return err
	}
	if fi, err := os.Lstat(s.path); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write the trust store through the symlink %s", s.path)
		}
		if err := secure(s.path, "trust store"); err != nil {
			return err
		}
	}
	if len(s.keys) == 0 {
		err := os.Remove(s.path) //fsx:allow brew's trust store, outside the prefix
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, key := range s.keys {
		k, _ := json.Marshal(key)
		v, _ := json.MarshalIndent(s.entries[key], "  ", "  ")
		if len(s.entries[key]) == 0 {
			v = []byte("[]")
		}
		fmt.Fprintf(&b, "  %s: %s", k, v)
		if i < len(s.keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	tmp, err := os.CreateTemp(dir, ".trust-*.json") //fsx:allow brew's trust store, outside the prefix
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() //fsx:allow the temporary file, once renamed or failed
	if _, err := tmp.Write(b.Bytes()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path) //fsx:allow brew's trust store, outside the prefix
}

func secure(path, what string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("refusing to write insecure trust store: %s %s is not owned by the current user", what, path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("refusing to write insecure trust store: %s %s is group or world writable", what, path)
	}
	return nil
}
