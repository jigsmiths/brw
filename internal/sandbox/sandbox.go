// Package sandbox builds brew's macOS seatbelt profile for the commands of install steps.
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	Exec        = "/usr/bin/sandbox-exec"
	executeBits = 0o111
)

type kind int

const (
	literal kind = iota
	subpath
	regex
	anything
)

type rule struct {
	allow     bool
	operation string
	kind      kind
	path      string
	modifier  string
}

type Sandbox struct{ rules []rule }

func (s *Sandbox) add(allow bool, operation string, k kind, path string) {
	if k == subpath || k == literal {
		path = expandRealpath(path)
	}
	s.rules = append(s.rules, rule{allow: allow, operation: operation, kind: k, path: path})
}

func expandRealpath(path string) string {
	path, _ = filepath.Abs(path)
	rest := ""
	for p := path; ; p = filepath.Dir(p) {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		if p == filepath.Dir(p) {
			return path
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

func (s *Sandbox) AllowRead(path string)          { s.add(true, "file-read*", subpath, path) }
func (s *Sandbox) DenyReadPath(path string)       { s.add(false, "file-read*", subpath, path) }
func (s *Sandbox) AllowWritePath(path string)     { s.add(true, "file-write*", subpath, path) }
func (s *Sandbox) DenyWritePath(path string)      { s.add(false, "file-write*", subpath, path) }
func (s *Sandbox) DenyWriteLiteral(path string)   { s.add(false, "file-write*", literal, path) }
func (s *Sandbox) DenyAllNetwork()                { s.add(false, "network*", anything, "") }
func (s *Sandbox) allowWriteRegex(pattern string) { s.add(true, "file-write*", regex, pattern) }

func (s *Sandbox) AllowWriteSystemTemp(homebrewTemp string) {
	s.AllowWritePath("/private/tmp")
	s.AllowWritePath("/private/var/tmp")
	s.allowWriteRegex("^/private/var/folders/[^/]+/[^/]+/[C,T]/")
	s.AllowWritePath(homebrewTemp)
}

var credentialPaths = []string{
	".ssh", ".gitconfig", ".git-credentials", ".config/gh", ".netrc",
	".aws", ".azure", ".boto", ".docker", ".config/fish", ".config/gcloud", ".config/huggingface",
	".config/pip", ".config/pypoetry", ".config/rclone", ".config/containers/auth.json",
	".config/composer/auth.json", ".config/sops/age/keys.txt", ".gnupg", ".gsutil", ".kube",
	".npmrc", ".yarnrc", ".yarnrc.yml", ".pnpmrc", ".bunfig.toml", ".pypirc", ".pip", ".poetry",
	".local/share/pypoetry", ".gem/credentials", ".bundle/config", ".cargo/credentials",
	".cargo/credentials.toml", ".composer/auth.json", ".condarc", ".m2/settings.xml",
	".gradle/gradle.properties", ".sbt/1.0/credentials.sbt", ".terraform.d/credentials.tfrc.json",
	".pulumi/credentials.json", ".oci/config", ".huggingface/token", ".cache/huggingface/token",
	".claude", ".claude.json", ".kiro", ".bash_login", ".bash_logout", ".bash_profile", ".bashrc",
	".bash_history", ".profile", ".zlogin", ".zlogout", ".zprofile", ".zshenv", ".zshrc",
	".zsh_history", ".python_history", ".mysql_history", ".psql_history", ".env", ".env.local",
	"Documents", "Movies", "Music", "Pictures", "Library/Keychains", "Library/Mobile Documents",
	"Library/CloudStorage", "Dropbox", "Google Drive", "OneDrive",
}

func (s *Sandbox) DenyReadHome(home string, readable []string, warn func(string)) {
	home = realpath(home)
	var paths []string
	for _, p := range readable {
		if p == "" {
			continue
		}
		paths = append(paths, filepath.Clean(p))
		if r, err := filepath.EvalSymlinks(p); err == nil {
			paths = append(paths, r)
		}
	}
	if !slices.ContainsFunc(paths, func(p string) bool { return Within(home, p) }) {
		s.DenyReadPath(home)
		return
	}
	for _, rel := range credentialPaths {
		path, err := filepath.EvalSymlinks(filepath.Join(home, rel))
		if err != nil || !Within(home, path) {
			continue
		}
		if i := slices.IndexFunc(paths, func(p string) bool { return Within(path, p) }); i >= 0 {
			warn(fmt.Sprintf("The sandbox cannot prevent formulae from reading:\n  %s\nbecause this required path is inside it:\n  %s\nFormulae may access personal data in this directory.", path, paths[i]))
			continue
		}
		s.DenyReadPath(path)
	}
}

func (s *Sandbox) DenyWriteHomebrewRepository(brewFile, prefix, repository string) {
	s.DenyWriteLiteral(brewFile)
	if Within(realpath(repository), realpath(prefix)) {
		s.DenyWritePath(filepath.Join(repository, "Library"))
		s.DenyWritePath(filepath.Join(repository, ".git"))
		return
	}
	s.DenyWritePath(repository)
}

func realpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func Within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Sandbox) NetworkAllowed() bool {
	return !slices.ContainsFunc(s.rules, func(r rule) bool { return !r.allow && r.operation == "network*" && r.kind == anything })
}

func quote(path string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) }

func (r rule) String() string {
	var b strings.Builder
	b.WriteString("(")
	if r.allow {
		b.WriteString("allow ")
	} else {
		b.WriteString("deny ")
	}
	b.WriteString(r.operation)
	if r.allow && r.operation == "network*" {
		b.WriteString(" network-outbound")
	}
	switch r.kind {
	case regex:
		fmt.Fprintf(&b, ` (regex #"%s")`, r.path)
	case subpath:
		fmt.Fprintf(&b, ` (subpath "%s")`, quote(r.path))
	case literal:
		fmt.Fprintf(&b, ` (literal "%s")`, quote(r.path))
	}
	if r.modifier != "" {
		fmt.Fprintf(&b, " (with %s)", r.modifier)
	}
	b.WriteString(")")
	return b.String()
}

func (s *Sandbox) Profile() string {
	network := s.NetworkAllowed()
	var b strings.Builder
	b.WriteString("(version 1)\n(debug deny) ; log all denied operations to /var/log/system.log\n(deny network-outbound (to unix-socket))\n")
	if network {
		b.WriteString("(allow network-outbound (to unix-socket (path-literal \"/private/var/run/mDNSResponder\")))\n")
	}
	for _, r := range s.rules {
		b.WriteString(r.String() + "\n")
	}
	b.WriteString(`(allow file-write*
    (literal "/dev/ptmx")
    (literal "/dev/dtracehelper")
    (literal "/dev/null")
    (literal "/dev/random")
    (literal "/dev/zero")
    (regex #"^/dev/fd/[0-9]+$")
    (regex #"^/dev/tty[a-z0-9]*$")
    )
(deny file-write*) ; deny non-allowlist file write operations
(deny file-write-setugid) ; deny non-allowlist file write SUID/SGID operations
(deny file-write-mode) ; deny non-allowlist file write mode operations
(deny mach-lookup)
(allow mach-lookup
    (xpc-service-name "com.apple.MTLCompilerService")
    (global-name "com.apple.mobileassetd.v2")
    (global-name "com.apple.sysmond")
    (global-name "com.apple.lsd.mapdb")
    (global-name "com.apple.bsd.dirhelper")
    (global-name "com.apple.system.opendirectoryd.libinfo")
    (global-name "com.apple.system.opendirectoryd.membership")
    (global-name "com.apple.PowerManagement.control")
    (global-name "com.apple.SecurityServer")
    (global-name "com.apple.networkd")
    (global-name "com.apple.ocspd")
    (global-name "com.apple.trustd.agent")
`)
	if network {
		b.WriteString("    (global-name \"com.apple.trustd\")\n    (global-name \"com.apple.TrustEvaluationAgent\")\n")
	}
	b.WriteString(`    (global-name "com.apple.SystemConfiguration.DNSConfiguration")
    (global-name "com.apple.SystemConfiguration.configd")
    )
(deny lsopen)
(deny appleevent-send)
(allow process-exec
    (literal "/bin/ps")
    (with no-sandbox)
    ) ; allow certain processes running without sandbox
(allow default) ; allow everything else
`)
	return b.String()
}

func Available() bool {
	fi, err := os.Stat(Exec)
	return err == nil && fi.Mode()&executeBits != 0 && !fi.IsDir()
}
