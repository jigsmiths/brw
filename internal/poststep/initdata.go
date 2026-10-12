package poststep

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// initDataDir creates a database's data directory the way brew's
// run_init_data_dir does, unless it is already initialised.
func (r *runner) initDataDir(s step) error {
	var marker, program string
	switch s.Using {
	case "postgresql_initdb":
		marker, program = "PG_VERSION", "initdb"
	case "mysql_initialize":
		marker, program = "mysql/general_log.CSM", "mysqld"
	case "mariadb_install_db":
		marker, program = "mysql/user.frm", "mysql_install_db"
	default:
		return fmt.Errorf("unknown data directory initialiser: %s", s.Using)
	}
	data, rel, err := r.target(s.Path)
	if err != nil {
		return err
	}
	if err := r.c.Root.MkdirAll(rel, 0o755); err != nil {
		return err
	}
	if os.Getenv("HOMEBREW_GITHUB_ACTIONS") != "" {
		return nil
	}
	if _, err := r.c.Root.Lstat(path.Join(rel, marker)); err == nil {
		return nil
	}

	keg := filepath.Join(r.c.Prefix, r.c.Keg)
	var args []string
	switch s.Using {
	case "postgresql_initdb":
		locale := s.Locale
		if locale == "" {
			locale = "en_US.UTF-8"
		}
		args = []string{"--locale=" + locale, "-E", "UTF-8", data}
	default:
		if r.c.User == "" {
			return errors.New("the user name is not known")
		}
		args = []string{"--user=" + r.c.User, "--basedir=" + keg, "--datadir=" + data, "--tmpdir=/tmp"}
		if s.Using == "mysql_initialize" {
			args = slices.Insert(args, 0, "--initialize-insecure")
		} else {
			args = slices.Insert(args, 0, "--verbose")
		}
	}

	exe := filepath.Join(keg, "bin", program)
	cmd := exec.Command(exe, args...)
	cmd.Env = os.Environ()
	if s.Using != "postgresql_initdb" {
		cmd.Env = slices.DeleteFunc(cmd.Env, func(e string) bool { return strings.HasPrefix(e, "TMPDIR=") })
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", exe, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
