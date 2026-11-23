package sbom

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Dep is a runtime dependency of a built formula: the version installed and
// the bottle the formula has now.
type Dep struct {
	Name, FullName          string
	PkgVersion              string // installed
	FormulaPkgVersion       string // the formula's now
	License                 string
	BottleURL, BottleSHA256 string
}

// BuildParams describe a formula brw built from its download.
type BuildParams struct {
	Name, FullName, Version string
	URL, SHA256             string
	License                 string
	Compiler, Xcode         string
	SourceModified          time.Time
	Creator                 string
	Deps                    []Dep
}

var unsafeID = regexp.MustCompile(`[^a-zA-Z0-9-]`)

// spdxID is brew's SBOM.spdx_id.
func spdxID(id string) string {
	return "SPDXRef-" + unsafeID.ReplaceAllStringFunc(id, func(c string) string { return fmt.Sprintf(".%x.", c) })
}

var unsafePurl = regexp.MustCompile(`[^A-Za-z0-9\-._~:]`)

// purl is brew's SBOM.brew_purl.
func purl(fullName, version string) string {
	enc := func(s string) string {
		return unsafePurl.ReplaceAllStringFunc(s, func(c string) string {
			var b strings.Builder
			for _, x := range []byte(c) {
				fmt.Fprintf(&b, "%%%02X", x)
			}
			return b.String()
		})
	}
	ns, name := "", fullName
	if i := strings.LastIndex(fullName, "/"); i >= 0 {
		ns, name = fullName[:i], fullName[i+1:]
	}
	out := "pkg:brew/"
	if ns != "" {
		var parts []string
		for s := range strings.SplitSeq(ns, "/") {
			if s != "" {
				parts = append(parts, enc(s))
			}
		}
		out += strings.Join(parts, "/") + "/"
	}
	return out + enc(name) + "@" + enc(version)
}

func assert(v string) string {
	if v == "" {
		return "NOASSERTION"
	}
	return v
}

type checksum struct {
	Algorithm     string `json:"algorithm"`
	ChecksumValue string `json:"checksumValue"`
}

type externalRef struct {
	ReferenceCategory string `json:"referenceCategory"`
	ReferenceLocator  string `json:"referenceLocator"`
	ReferenceType     string `json:"referenceType"`
}

type relationship struct {
	SpdxElementID      string `json:"spdxElementId"`
	RelationshipType   string `json:"relationshipType"`
	RelatedSpdxElement string `json:"relatedSpdxElement"`
}

// Built is the SBOM brew writes for a formula it builds, as SBOM#to_spdx_sbom
// makes it, with Creator naming the tool.
func Built(p BuildParams) ([]byte, error) {
	archive := spdxID("Archive-" + p.Name + "-src")
	when := p.SourceModified.UTC().Format(time.RFC3339)

	type archivePkg struct {
		SPDXID           string        `json:"SPDXID"`
		Name             string        `json:"name"`
		VersionInfo      string        `json:"versionInfo"`
		FilesAnalyzed    bool          `json:"filesAnalyzed"`
		LicenseDeclared  string        `json:"licenseDeclared"`
		BuiltDate        string        `json:"builtDate"`
		LicenseConcluded string        `json:"licenseConcluded"`
		DownloadLocation string        `json:"downloadLocation"`
		CopyrightText    string        `json:"copyrightText"`
		ExternalRefs     []externalRef `json:"externalRefs"`
		Checksums        []checksum    `json:"checksums"`
	}
	type depPkg struct {
		SPDXID           string        `json:"SPDXID"`
		Name             string        `json:"name"`
		VersionInfo      string        `json:"versionInfo"`
		FilesAnalyzed    bool          `json:"filesAnalyzed"`
		LicenseDeclared  string        `json:"licenseDeclared"`
		LicenseConcluded string        `json:"licenseConcluded"`
		DownloadLocation string        `json:"downloadLocation"`
		CopyrightText    string        `json:"copyrightText"`
		Checksums        []checksum    `json:"checksums"`
		ExternalRefs     []externalRef `json:"externalRefs"`
	}
	type compilerPkg struct {
		SPDXID           string        `json:"SPDXID"`
		Name             string        `json:"name"`
		VersionInfo      string        `json:"versionInfo"`
		FilesAnalyzed    bool          `json:"filesAnalyzed"`
		LicenseDeclared  string        `json:"licenseDeclared"`
		LicenseConcluded string        `json:"licenseConcluded"`
		CopyrightText    string        `json:"copyrightText"`
		DownloadLocation string        `json:"downloadLocation"`
		Checksums        []checksum    `json:"checksums"`
		ExternalRefs     []externalRef `json:"externalRefs"`
	}
	type file struct {
		SPDXID    string     `json:"SPDXID"`
		FileName  string     `json:"fileName"`
		Checksums []checksum `json:"checksums"`
	}

	sums := []checksum{}
	files := []file{}
	if p.SHA256 != "" {
		sums = []checksum{{"SHA256", p.SHA256}}
		name := p.URL[strings.LastIndex(p.URL, "/")+1:]
		if name == "" {
			name = p.Name + "-" + p.Version
		}
		files = append(files, file{spdxID("File-" + p.Name), name, sums})
	}
	packages := []any{archivePkg{
		SPDXID: archive, Name: p.Name, VersionInfo: p.Version, LicenseDeclared: assert(""), BuiltDate: when,
		LicenseConcluded: assert(p.License), DownloadLocation: p.URL, CopyrightText: assert(""),
		ExternalRefs: []externalRef{{"PACKAGE-MANAGER", purl(p.FullName, p.Version), "purl"}}, Checksums: sums,
	}}
	describes := []string{archive}
	var relations []relationship
	for _, d := range p.Deps {
		url := ""
		if d.PkgVersion == d.FormulaPkgVersion {
			url = d.BottleURL
		}
		id := spdxID("Package-SPDXRef-" + d.Name + "-" + d.PkgVersion)
		packages = append(packages, depPkg{
			SPDXID: id, Name: d.Name, VersionInfo: d.PkgVersion, LicenseDeclared: assert(""),
			LicenseConcluded: assert(d.License), DownloadLocation: assert(url), CopyrightText: assert(""),
			Checksums:    []checksum{{"SHA256", assert(d.BottleSHA256)}},
			ExternalRefs: []externalRef{{"PACKAGE-MANAGER", purl(d.FullName, d.PkgVersion), "purl"}},
		})
		describes = append(describes, id)
		relations = append(relations, relationship{id, "RUNTIME_DEPENDENCY_OF", archive})
	}
	packages = append(packages, compilerPkg{
		SPDXID: "SPDXRef-Compiler", Name: p.Compiler, VersionInfo: assert(p.Xcode), LicenseDeclared: assert(""),
		LicenseConcluded: assert(""), CopyrightText: assert(""), DownloadLocation: assert(""),
		Checksums: []checksum{}, ExternalRefs: []externalRef{},
	})
	describes = append(describes, "SPDXRef-Compiler")
	if p.SHA256 != "" {
		relations = append(relations, relationship{spdxID("File-" + p.Name), "PACKAGE_OF", archive})
	}
	relations = append(relations, relationship{"SPDXRef-Compiler", "BUILD_TOOL_OF", archive})

	doc := struct {
		SPDXID       string `json:"SPDXID"`
		SpdxVersion  string `json:"spdxVersion"`
		Name         string `json:"name"`
		CreationInfo struct {
			Created  string   `json:"created"`
			Creators []string `json:"creators"`
		} `json:"creationInfo"`
		DataLicense       string         `json:"dataLicense"`
		DocumentNamespace string         `json:"documentNamespace"`
		DocumentDescribes []string       `json:"documentDescribes"`
		Files             []file         `json:"files"`
		Packages          []any          `json:"packages"`
		Relationships     []relationship `json:"relationships"`
	}{
		SPDXID: "SPDXRef-DOCUMENT", SpdxVersion: "SPDX-2.3", Name: "SBOM-SPDX-" + p.Name + "-" + p.Version,
		DataLicense: "CC0-1.0", DocumentNamespace: "https://formulae.brew.sh/spdx/" + p.Name + "-" + p.Version + ".json",
		DocumentDescribes: describes, Files: files, Packages: packages, Relationships: relations,
	}
	doc.CreationInfo.Created = when
	doc.CreationInfo.Creators = []string{p.Creator}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
