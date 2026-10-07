// Package version decides what version a freshly bootstrapped project starts
// at and rewrites that version into the files that carry it (pom.xml,
// package.json, .csproj, properties files, ...). Edits are surgical byte-range
// replacements, so every other byte of a file -- formatting, comments, line
// endings -- is left exactly as it was.
package version

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Initial is the version used when a repo has no usable previous tag.
const Initial = "1.0.0"

// ErrNoVersion means the file has no literal version this package can safely
// rewrite (it's absent, inherited, or a ${property}/$(property) reference).
var ErrNoVersion = errors.New("no literal version found")

var semverTag = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// Next returns the version to set given the repo's most recent tag ("" when it
// has none), plus a short human-readable reason. A tag of the form x.y.z moves
// to x.y.z+1; no tag, or a tag in any other shape, starts at Initial.
func Next(latestTag string) (version, reason string) {
	if latestTag == "" {
		return Initial, "no tags in the repo"
	}
	m := semverTag.FindStringSubmatch(latestTag)
	if m == nil {
		return Initial, fmt.Sprintf("latest tag %q is not x.y.z", latestTag)
	}
	patch, err := strconv.ParseUint(m[3], 10, 64)
	if err != nil || patch == ^uint64(0) {
		return Initial, fmt.Sprintf("latest tag %q has an out-of-range patch number", latestTag)
	}
	return fmt.Sprintf("%s.%s.%d", m[1], m[2], patch+1), fmt.Sprintf("latest tag %s, patch bumped", latestTag)
}

// Editor rewrites the version in content, returning the new content and the
// version that was there. It returns an error wrapping ErrNoVersion when the
// file has no literal version to replace.
type Editor func(content []byte, version string) (out []byte, old string, err error)

// Bundled returns the editor for a file this tool bundles itself (its
// placeholder version is replaced when the file is generated), or nil.
func Bundled(targetPath string) Editor {
	switch path.Base(targetPath) {
	case "version.txt":
		return lineEditor(regexp.MustCompile(`(?m)^(version:[ \t]*)([^\r\n]*?)([ \t]*)\r?$`))
	case "VERSION":
		return Properties(`project\.version`)
	case ".bumpversion.cfg":
		return lineEditor(regexp.MustCompile(`(?m)^(current_version[ \t]*=[ \t]*)([^\r\n]*?)([ \t]*)\r?$`))
	}
	return nil
}

// EditorFor returns the editor for an existing repo file that carries the
// project's version, or nil if the path isn't one. Root-level names match
// only at the repo root (a nested pom.xml or package.json belongs to a
// submodule or a dependency); .csproj files match anywhere. propertyFiles are
// extra paths (e.g. a template's PROPERTY_FILE) edited as `version=` files.
func EditorFor(filePath string, propertyFiles []string) Editor {
	switch filePath {
	case "pom.xml":
		return Pom
	case "package.json":
		return PackageJSON
	case "gradle.properties":
		return Properties(`(?:project\.)?version`)
	case "app.properties":
		return Properties(`version`)
	case "pyproject.toml":
		return Pyproject
	}
	if strings.HasSuffix(filePath, ".csproj") {
		return Csproj
	}
	for _, p := range propertyFiles {
		if filePath == p {
			return Properties(`version`)
		}
	}
	return nil
}

func lineEditor(re *regexp.Regexp) Editor {
	return func(content []byte, version string) ([]byte, string, error) {
		loc := re.FindSubmatchIndex(content)
		if loc == nil {
			return nil, "", ErrNoVersion
		}
		return splice(content, loc[4], loc[5], version)
	}
}

// Properties edits a `key=value` / `key: value` properties file, where key is
// a regexp (alternatives allowed) matched against the whole key.
func Properties(key string) Editor {
	return lineEditor(regexp.MustCompile(`(?m)^([ \t]*(?:` + key + `)[ \t]*[=:][ \t]*)([^\r\n]*?)([ \t]*)\r?$`))
}

var csprojVersion = regexp.MustCompile(`(<Version>\s*)([^<]*?)(\s*</Version>)`)
var csprojVersionPrefix = regexp.MustCompile(`(<VersionPrefix>\s*)([^<]*?)(\s*</VersionPrefix>)`)

// Csproj edits the <Version> element (falling back to <VersionPrefix>).
func Csproj(content []byte, version string) ([]byte, string, error) {
	out, old, err := lineEditor(csprojVersion)(content, version)
	if errors.Is(err, ErrNoVersion) {
		return lineEditor(csprojVersionPrefix)(content, version)
	}
	return out, old, err
}

// splice replaces content[start:end] with version, refusing a value that is a
// property reference rather than a literal.
func splice(content []byte, start, end int, version string) ([]byte, string, error) {
	old := string(content[start:end])
	if old == "" {
		return nil, "", fmt.Errorf("%w: version is empty", ErrNoVersion)
	}
	if strings.Contains(old, "${") || strings.Contains(old, "$(") {
		return nil, "", fmt.Errorf("%w: version is a property reference (%s)", ErrNoVersion, old)
	}
	out := make([]byte, 0, len(content)-(end-start)+len(version))
	out = append(out, content[:start]...)
	out = append(out, version...)
	out = append(out, content[end:]...)
	return out, old, nil
}

type span struct{ start, end int }

// trimmed narrows a span to exclude surrounding whitespace.
func (s span) trimmed(content []byte) span {
	for s.start < s.end && bytes.ContainsRune([]byte(" \t\r\n"), rune(content[s.start])) {
		s.start++
	}
	for s.end > s.start && bytes.ContainsRune([]byte(" \t\r\n"), rune(content[s.end-1])) {
		s.end--
	}
	return s
}

var propertyRef = regexp.MustCompile(`^\$\{([^}]+)\}$`)

// Pom edits the project's own <version> -- not the <parent>'s, not a
// dependency's. When that version is a ${property} reference, the property's
// value under <properties> is edited instead.
func Pom(content []byte, version string) ([]byte, string, error) {
	dec := xml.NewDecoder(bytes.NewReader(content))
	dec.Strict = false
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }

	var (
		depth       int
		inVersion   bool
		inProps     bool
		curProp     string
		versionSpan *span
		props       = map[string]span{}
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("parsing pom.xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 2 && t.Name.Local == "version":
				inVersion = true
			case depth == 2 && t.Name.Local == "properties":
				inProps = true
			case depth == 3 && inProps:
				curProp = t.Name.Local
			}
		case xml.CharData:
			end := int(dec.InputOffset())
			start := end - len(t)
			if start < 0 || !bytes.Equal(content[start:end], t) {
				continue // entities or CDATA: not a plain literal, leave it
			}
			switch {
			case depth == 2 && inVersion && versionSpan == nil:
				versionSpan = &span{start, end}
			case depth == 3 && curProp != "":
				if _, seen := props[curProp]; !seen {
					props[curProp] = span{start, end}
				}
			}
		case xml.EndElement:
			switch depth {
			case 2:
				inVersion, inProps = false, false
			case 3:
				curProp = ""
			}
			depth--
		}
	}

	if versionSpan == nil {
		return nil, "", fmt.Errorf("%w: pom.xml has no <version> of its own (inherited from <parent>?)", ErrNoVersion)
	}
	vs := versionSpan.trimmed(content)
	if m := propertyRef.FindSubmatch(content[vs.start:vs.end]); m != nil {
		ps, ok := props[string(m[1])]
		if !ok {
			return nil, "", fmt.Errorf("%w: <version> is ${%s} but no <%s> property is defined in pom.xml", ErrNoVersion, m[1], m[1])
		}
		ps = ps.trimmed(content)
		return splice(content, ps.start, ps.end, version)
	}
	return splice(content, vs.start, vs.end, version)
}

// PackageJSON edits the top-level "version" key.
func PackageJSON(content []byte, version string) ([]byte, string, error) {
	dec := json.NewDecoder(bytes.NewReader(content))
	tok, err := dec.Token()
	if d, ok := tok.(json.Delim); err != nil || !ok || d != '{' {
		return nil, "", fmt.Errorf("parsing package.json: not a JSON object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, "", fmt.Errorf("parsing package.json: %w", err)
		}
		if key, _ := keyTok.(string); key == "version" {
			valTok, err := dec.Token()
			if err != nil {
				return nil, "", fmt.Errorf("parsing package.json: %w", err)
			}
			if _, ok := valTok.(string); !ok {
				return nil, "", fmt.Errorf("%w: \"version\" is not a string", ErrNoVersion)
			}
			end := int(dec.InputOffset()) // just past the closing quote
			start := bytes.LastIndexByte(content[:end-1], '"') + 1
			if bytes.IndexByte(content[start:end-1], '\\') >= 0 {
				return nil, "", fmt.Errorf("%w: \"version\" contains escapes", ErrNoVersion)
			}
			return splice(content, start, end-1, version)
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, "", fmt.Errorf("parsing package.json: %w", err)
		}
	}
	return nil, "", fmt.Errorf("%w: package.json has no top-level \"version\"", ErrNoVersion)
}

var (
	tomlSection = regexp.MustCompile(`^[ \t]*\[([^\[\]]+)\][ \t]*$`)
	tomlVersion = regexp.MustCompile(`^([ \t]*version[ \t]*=[ \t]*["'])([^"']*)(["'])`)
)

// Pyproject edits `version = "..."` under [project] or [tool.poetry].
func Pyproject(content []byte, version string) ([]byte, string, error) {
	section := ""
	offset := 0
	for _, line := range bytes.SplitAfter(content, []byte("\n")) {
		trimmed := bytes.TrimRight(line, "\r\n")
		if m := tomlSection.FindSubmatch(trimmed); m != nil {
			section = strings.TrimSpace(string(m[1]))
		} else if section == "project" || section == "tool.poetry" {
			if loc := tomlVersion.FindSubmatchIndex(trimmed); loc != nil {
				return splice(content, offset+loc[4], offset+loc[5], version)
			}
		}
		offset += len(line)
	}
	return nil, "", fmt.Errorf("%w: pyproject.toml has no literal version under [project] or [tool.poetry] (dynamic?)", ErrNoVersion)
}
