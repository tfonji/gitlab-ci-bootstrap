package version

import (
	"bytes"
	"regexp"
)

// The helpers here add a version to a file that has none, matching that file's
// indentation and line endings so the diff is just the added line.

func newline(content []byte) string {
	if bytes.Contains(content, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// insertAt returns content with text(nl) inserted at offset at.
func insertAt(content []byte, at int, text func(nl string) string) []byte {
	ins := text(newline(content))
	out := make([]byte, 0, len(content)+len(ins))
	out = append(out, content[:at]...)
	out = append(out, ins...)
	out = append(out, content[at:]...)
	return out
}

// lineIndent returns the leading whitespace of the line containing offset at,
// and whether that line holds nothing but whitespace before at.
func lineIndent(content []byte, at int) (indent string, ownLine bool) {
	ls := bytes.LastIndexByte(content[:at], '\n') + 1
	prefix := content[ls:at]
	if len(bytes.TrimLeft(prefix, " \t")) != 0 {
		return "", false
	}
	return string(prefix), true
}

var (
	propertyGroupOpen = regexp.MustCompile(`(?is)<PropertyGroup(\s[^>]*)?>`)
	propertyGroupEnd  = []byte("</PropertyGroup>")
	projectOpen       = regexp.MustCompile(`(?is)<Project(\s[^>]*[^/>])?>`)
)

// addCsprojVersion adds <element>version</element> as the last child of the
// first PropertyGroup without a Condition, or in a new PropertyGroup at the top
// of the project when it has none.
func addCsprojVersion(content []byte, element, version string) ([]byte, bool) {
	line := "<" + element + ">" + version + "</" + element + ">"
	for _, loc := range propertyGroupOpen.FindAllSubmatchIndex(content, -1) {
		if loc[2] >= 0 && bytes.Contains(bytes.ToLower(content[loc[2]:loc[3]]), []byte("condition")) {
			continue
		}
		end := bytes.Index(content[loc[1]:], propertyGroupEnd)
		if end < 0 {
			continue
		}
		closeAt := loc[1] + end
		closeIndent, ownLine := lineIndent(content, closeAt)
		if !ownLine { // <PropertyGroup>...</PropertyGroup> on one line
			return insertAt(content, closeAt, func(string) string { return line }), true
		}
		childIndent := closeIndent + "  "
		if m := regexp.MustCompile(`\n([ \t]+)<`).FindSubmatch(content[loc[1]:closeAt]); m != nil {
			childIndent = string(m[1])
		}
		ls := closeAt - len(closeIndent)
		return insertAt(content, ls, func(nl string) string { return childIndent + line + nl }), true
	}
	loc := projectOpen.FindIndex(content)
	if loc == nil {
		return nil, false
	}
	return insertAt(content, loc[1], func(nl string) string {
		return nl + "  <PropertyGroup>" + nl + "    " + line + nl + "  </PropertyGroup>"
	}), true
}

// addPomVersion adds a <version> element right after the project's own
// <artifactId>, which ends at artifactEnd.
func addPomVersion(content []byte, artifactEnd int, version string) ([]byte, bool) {
	if artifactEnd <= 0 {
		return nil, false
	}
	start := bytes.LastIndex(content[:artifactEnd], []byte("<artifactId"))
	if start < 0 {
		return nil, false
	}
	line := "<version>" + version + "</version>"
	indent, ownLine := lineIndent(content, start)
	if !ownLine {
		return insertAt(content, artifactEnd, func(string) string { return line }), true
	}
	return insertAt(content, artifactEnd, func(nl string) string { return nl + indent + line }), true
}

// addPackageJSONVersion adds a "version" member right after "name" (whose
// value ends at nameEnd), or before the first member when there is no name.
func addPackageJSONVersion(content []byte, version string, nameEnd, nameKeyStart, firstKeyStart int) ([]byte, bool) {
	if nameEnd == 0 && firstKeyStart == 0 {
		return nil, false // empty object
	}
	if nameEnd > 0 {
		if indent, ownLine := lineIndent(content, nameKeyStart); ownLine {
			return insertAt(content, nameEnd, func(nl string) string {
				return "," + nl + indent + `"version": "` + version + `"`
			}), true
		}
		return insertAt(content, nameEnd, func(string) string { return `,"version":"` + version + `"` }), true
	}
	if indent, ownLine := lineIndent(content, firstKeyStart); ownLine {
		return insertAt(content, firstKeyStart, func(nl string) string {
			return `"version": "` + version + `",` + nl + indent
		}), true
	}
	return insertAt(content, firstKeyStart, func(string) string { return `"version":"` + version + `",` }), true
}
