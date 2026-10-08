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

// ErrAbsent is the narrower case of ErrNoVersion: the file declares no version
// at all, as opposed to declaring one this package can't safely rewrite (a
// property reference, a dynamic version). Editors add the missing version when
// they can, so callers only see ErrAbsent from editors that can't.
var ErrAbsent = errors.New("version not declared")

// absentError is an ErrNoVersion that is also an ErrAbsent, with its own
// message.
type absentError struct{ msg string }

func (e absentError) Error() string { return e.msg }
func (e absentError) Is(target error) bool {
	return target == ErrNoVersion || target == ErrAbsent
}

func absent(format string, args ...any) error {
	return absentError{fmt.Sprintf(format, args...)}
}

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

// Editor sets the version in content, returning the new content and the
// version that was there. When the file declares no version at all, it adds
// one where that file type keeps it and returns old == "". It returns an error
// wrapping ErrNoVersion when there is no literal version it can safely set.
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

// Options tune EditorFor to the template being applied.
type Options struct {
	// PropertyFiles are extra paths (e.g. a template's PROPERTY_FILE) edited
	// as `version=` files.
	PropertyFiles []string
	// CsprojElement is the element name written when a .csproj has no version
	// yet; "Version" when empty. Existing elements keep whatever case they have.
	CsprojElement string
}

// EditorFor returns the editor for an existing repo file that carries the
// project's version, or nil if the path isn't one. Root-level names match
// only at the repo root (a nested pom.xml or package.json belongs to a
// submodule or a dependency); .csproj files match anywhere.
//
// A file with no version of its own gets one added -- except gradle.properties,
// whose version more often lives in build.gradle, where a second one here
// would be silently shadowed.
func EditorFor(filePath string, o Options) Editor {
	switch filePath {
	case "pom.xml":
		return Pom
	case "package.json":
		return PackageJSON
	case "gradle.properties":
		return Properties(`(?:project\.)?version`)
	case "app.properties":
		return PropertiesAdding(`version`, "version")
	case "pyproject.toml":
		return Pyproject
	}
	if strings.HasSuffix(filePath, ".csproj") {
		return CsprojAdding(o.CsprojElement)
	}
	for _, p := range o.PropertyFiles {
		if filePath == p {
			return PropertiesAdding(`version`, "version")
		}
	}
	return nil
}

func lineEditor(re *regexp.Regexp) Editor {
	return func(content []byte, version string) ([]byte, string, error) {
		loc := re.FindSubmatchIndex(content)
		if loc == nil {
			return nil, "", absent("%s", ErrNoVersion.Error())
		}
		return splice(content, loc[4], loc[5], version)
	}
}

// Properties edits a `key=value` / `key: value` properties file, where key is
// a regexp (alternatives allowed) matched against the whole key. A file
// without the key is left alone (ErrAbsent); see PropertiesAdding.
func Properties(key string) Editor {
	return lineEditor(regexp.MustCompile(`(?m)^([ \t]*(?:` + key + `)[ \t]*[=:][ \t]*)([^\r\n]*?)([ \t]*)\r?$`))
}

// PropertiesAdding is Properties, but a file without the key gets an
// `addKey=version` line appended.
func PropertiesAdding(key, addKey string) Editor {
	edit := Properties(key)
	return func(content []byte, version string) ([]byte, string, error) {
		out, old, err := edit(content, version)
		if errors.Is(err, ErrAbsent) {
			return insertAt(content, len(content), func(nl string) string {
				prefix := ""
				if len(content) > 0 && content[len(content)-1] != '\n' {
					prefix = nl
				}
				return prefix + addKey + "=" + version + nl
			}), "", nil
		}
		return out, old, err
	}
}

var csprojVersion = regexp.MustCompile(`(?i)(<Version>\s*)([^<]*?)(\s*</Version>)`)
var csprojVersionPrefix = regexp.MustCompile(`(?i)(<VersionPrefix>\s*)([^<]*?)(\s*</VersionPrefix>)`)

// Csproj edits the <Version> element (falling back to <VersionPrefix>);
// element names are matched case-insensitively, as MSBuild does. A project
// with neither is left alone (ErrAbsent); see CsprojAdding.
func Csproj(content []byte, version string) ([]byte, string, error) {
	out, old, err := lineEditor(csprojVersion)(content, version)
	if errors.Is(err, ErrAbsent) {
		return lineEditor(csprojVersionPrefix)(content, version)
	}
	return out, old, err
}

// CsprojAdding is Csproj, but a project with no version gets an
// <element>version</element> in its first unconditional PropertyGroup.
func CsprojAdding(element string) Editor {
	if element == "" {
		element = "Version"
	}
	return func(content []byte, version string) ([]byte, string, error) {
		out, old, err := Csproj(content, version)
		if !errors.Is(err, ErrAbsent) {
			return out, old, err
		}
		added, ok := addCsprojElement(content, element, version)
		if !ok {
			return nil, "", absent("%s and has no <PropertyGroup> to add one to", ErrNoVersion.Error())
		}
		return added, "", nil
	}
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
		sawVersion  bool
		artifactEnd int
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
				inVersion, sawVersion = true, true
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
				if t.Name.Local == "artifactId" && artifactEnd == 0 {
					artifactEnd = int(dec.InputOffset())
				}
				inVersion, inProps = false, false
			case 3:
				curProp = ""
			}
			depth--
		}
	}

	if versionSpan == nil {
		if sawVersion {
			return nil, "", fmt.Errorf("%w: pom.xml's <version> is not a plain literal", ErrNoVersion)
		}
		// No <version> of its own: it inherits the parent's. Give it its own.
		if added, ok := addPomVersion(content, artifactEnd, version); ok {
			return added, "", nil
		}
		return nil, "", absent("%s: pom.xml has no <version> of its own and no <artifactId> to add one after", ErrNoVersion.Error())
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

// PackageJSON edits the top-level "version" key, adding one after "name" (or
// first, with no name) when there is none.
func PackageJSON(content []byte, version string) ([]byte, string, error) {
	dec := json.NewDecoder(bytes.NewReader(content))
	tok, err := dec.Token()
	if d, ok := tok.(json.Delim); err != nil || !ok || d != '{' {
		return nil, "", fmt.Errorf("parsing package.json: not a JSON object")
	}
	var firstKeyStart, nameEnd, nameKeyStart int
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, "", fmt.Errorf("parsing package.json: %w", err)
		}
		keyEnd := int(dec.InputOffset())
		keyStart := bytes.LastIndexByte(content[:keyEnd-1], '"')
		if firstKeyStart == 0 {
			firstKeyStart = keyStart
		}
		key, _ := keyTok.(string)
		if key == "version" {
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
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, "", fmt.Errorf("parsing package.json: %w", err)
		}
		if key == "name" && nameEnd == 0 {
			nameEnd, nameKeyStart = int(dec.InputOffset()), keyStart
		}
	}
	if added, ok := addPackageJSONVersion(content, version, nameEnd, nameKeyStart, firstKeyStart); ok {
		return added, "", nil
	}
	return nil, "", absent("%s: package.json has no top-level \"version\" and no key to add one beside", ErrNoVersion.Error())
}

var (
	tomlSection = regexp.MustCompile(`^[ \t]*\[([^\[\]]+)\][ \t]*$`)
	tomlVersion = regexp.MustCompile(`^([ \t]*version[ \t]*=[ \t]*["'])([^"']*)(["'])`)
)

// Pyproject edits `version = "..."` under [project] or [tool.poetry], adding
// one under the first of those that exists (unless the version is declared
// dynamic).
func Pyproject(content []byte, version string) ([]byte, string, error) {
	section := ""
	offset := 0
	insertAfter := map[string]int{} // section -> offset just past its header line
	bodyStart := map[string]int{}
	for _, line := range bytes.SplitAfter(content, []byte("\n")) {
		trimmed := bytes.TrimRight(line, "\r\n")
		if m := tomlSection.FindSubmatch(trimmed); m != nil {
			section = strings.TrimSpace(string(m[1]))
			if section == "project" || section == "tool.poetry" {
				insertAfter[section] = offset + len(line)
				bodyStart[section] = offset + len(line)
			}
		} else if section == "project" || section == "tool.poetry" {
			if loc := tomlVersion.FindSubmatchIndex(trimmed); loc != nil {
				return splice(content, offset+loc[4], offset+loc[5], version)
			}
		}
		offset += len(line)
	}
	for _, sec := range []string{"project", "tool.poetry"} {
		at, ok := insertAfter[sec]
		if !ok {
			continue
		}
		if tomlDynamicVersion.Match(sectionBody(content, bodyStart[sec])) {
			return nil, "", fmt.Errorf("%w: pyproject.toml declares the version dynamic", ErrNoVersion)
		}
		return insertAt(content, at, func(nl string) string {
			prefix := ""
			if at > 0 && content[at-1] != '\n' {
				prefix = nl // header was the last line, with no trailing newline
			}
			return prefix + "version = \"" + version + "\"" + nl
		}), "", nil
	}
	return nil, "", absent("%s: pyproject.toml has no [project] or [tool.poetry] section to add one to", ErrNoVersion.Error())
}

var tomlDynamicVersion = regexp.MustCompile(`(?s)(?m)^[ \t]*dynamic[ \t]*=[ \t]*\[[^\]]*["']version["']`)

// sectionBody returns the TOML text from start up to the next table header.
func sectionBody(content []byte, start int) []byte {
	rest := content[start:]
	for off := 0; off < len(rest); {
		end := bytes.IndexByte(rest[off:], '\n')
		line := rest[off:]
		if end >= 0 {
			line = rest[off : off+end]
		}
		if tomlSection.Match(bytes.TrimRight(line, "\r")) {
			return rest[:off]
		}
		if end < 0 {
			break
		}
		off += end + 1
	}
	return rest
}

var csprojApplicationName = regexp.MustCompile(`(?i)(<ApplicationName>\s*)([^<]*?)(\s*</ApplicationName>)`)

// SetApplicationName makes sure a .csproj has an <ApplicationName>, which the
// IIS templates name the build artifact after. An existing non-empty value is
// never touched: it is returned as existing with changed == false. A missing
// or empty one is set to name.
func SetApplicationName(content []byte, name string) (out []byte, existing string, changed bool) {
	if loc := csprojApplicationName.FindSubmatchIndex(content); loc != nil {
		if old := string(content[loc[4]:loc[5]]); old != "" {
			return content, old, false
		}
		filled := append(append(append([]byte{}, content[:loc[4]]...), name...), content[loc[5]:]...)
		return filled, "", true
	}
	added, ok := addCsprojElement(content, "ApplicationName", name)
	if !ok {
		return content, "", false
	}
	return added, "", true
}
