package version

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

func TestNext(t *testing.T) {
	cases := []struct{ tag, want string }{
		{"", "1.0.0"},
		{"1.2.3", "1.2.4"},
		{"0.0.0", "0.0.1"},
		{"10.20.39", "10.20.40"},
		{"v1.2.3", "1.0.0"},
		{"1.2", "1.0.0"},
		{"1.2.3-rc1", "1.0.0"},
		{"release-5", "1.0.0"},
		{"1.2.99999999999999999999", "1.0.0"},
	}
	for _, c := range cases {
		if got, _ := Next(c.tag); got != c.want {
			t.Errorf("Next(%q) = %q, want %q", c.tag, got, c.want)
		}
	}
}

func TestEditors(t *testing.T) {
	cases := []struct {
		name    string
		edit    Editor
		in      string
		want    string
		wantOld string
	}{
		{
			name: "pom own version, not parent or dependency",
			edit: Pom,
			in: `<?xml version="1.0"?>
<project>
  <parent><groupId>g</groupId><version>9.9.9</version></parent>
  <artifactId>a</artifactId>
  <version>0.0.1-SNAPSHOT</version>
  <dependencies><dependency><version>5.5.5</version></dependency></dependencies>
</project>
`,
			want: `<?xml version="1.0"?>
<project>
  <parent><groupId>g</groupId><version>9.9.9</version></parent>
  <artifactId>a</artifactId>
  <version>2.0.0</version>
  <dependencies><dependency><version>5.5.5</version></dependency></dependencies>
</project>
`,
			wantOld: "0.0.1-SNAPSHOT",
		},
		{
			name: "pom property reference edits the property",
			edit: Pom,
			in: `<project>
  <version>${revision}</version>
  <properties><java>17</java><revision>1.0.0-SNAPSHOT</revision></properties>
</project>`,
			want: `<project>
  <version>${revision}</version>
  <properties><java>17</java><revision>2.0.0</revision></properties>
</project>`,
			wantOld: "1.0.0-SNAPSHOT",
		},
		{
			name:    "package.json top-level only, formatting preserved",
			edit:    PackageJSON,
			in:      "{\n  \"name\": \"x\",\n  \"engines\": {\"version\": \"9\"},\n  \"version\": \"0.1.0\",\n  \"dependencies\": {\"version\": \"1\"}\n}\n",
			want:    "{\n  \"name\": \"x\",\n  \"engines\": {\"version\": \"9\"},\n  \"version\": \"2.0.0\",\n  \"dependencies\": {\"version\": \"1\"}\n}\n",
			wantOld: "0.1.0",
		},
		{
			name:    "csproj Version",
			edit:    Csproj,
			in:      "<Project><PropertyGroup>\r\n  <Version>1.0.0</Version>\r\n</PropertyGroup></Project>",
			want:    "<Project><PropertyGroup>\r\n  <Version>2.0.0</Version>\r\n</PropertyGroup></Project>",
			wantOld: "1.0.0",
		},
		{
			name:    "csproj falls back to VersionPrefix",
			edit:    Csproj,
			in:      "<Project><PropertyGroup><VersionPrefix>0.3.0</VersionPrefix></PropertyGroup></Project>",
			want:    "<Project><PropertyGroup><VersionPrefix>2.0.0</VersionPrefix></PropertyGroup></Project>",
			wantOld: "0.3.0",
		},
		{
			name:    "app.properties ignores versionCode and comments",
			edit:    Properties(`version`),
			in:      "# version=0.0.0\nname=app\nversionCode=7\nversion = 1.4.2  \r\nother=1\n",
			want:    "# version=0.0.0\nname=app\nversionCode=7\nversion = 2.0.0  \r\nother=1\n",
			wantOld: "1.4.2",
		},
		{
			name:    "gradle.properties project.version",
			edit:    Properties(`(?:project\.)?version`),
			in:      "org.gradle.jvmargs=-Xmx1g\nproject.version=0.0.1\n",
			want:    "org.gradle.jvmargs=-Xmx1g\nproject.version=2.0.0\n",
			wantOld: "0.0.1",
		},
		{
			name:    "pyproject poetry",
			edit:    Pyproject,
			in:      "[build-system]\nversion = \"9\"\n\n[tool.poetry]\nname = \"x\"\nversion = \"0.1.0\"\n",
			want:    "[build-system]\nversion = \"9\"\n\n[tool.poetry]\nname = \"x\"\nversion = \"2.0.0\"\n",
			wantOld: "0.1.0",
		},
		{
			name:    "bundled version.txt",
			edit:    Bundled("version.txt"),
			in:      "version: x.y.z\n",
			want:    "version: 2.0.0\n",
			wantOld: "x.y.z",
		},
		{
			name:    "bundled VERSION",
			edit:    Bundled("VERSION"),
			in:      "project.version=x.y.z\n",
			want:    "project.version=2.0.0\n",
			wantOld: "x.y.z",
		},
		{
			name:    "bundled .bumpversion.cfg only touches current_version",
			edit:    Bundled(".bumpversion.cfg"),
			in:      "[bumpversion]\ncurrent_version = 1.0.1\nserialize =\n\t{major}.{minor}.{patch}\n[bumpversion:file:app.properties]\nsearch = version={current_version}\n",
			want:    "[bumpversion]\ncurrent_version = 2.0.0\nserialize =\n\t{major}.{minor}.{patch}\n[bumpversion:file:app.properties]\nsearch = version={current_version}\n",
			wantOld: "1.0.1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, old, err := c.edit([]byte(c.in), "2.0.0")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("content:\n got %q\nwant %q", got, c.want)
			}
			if old != c.wantOld {
				t.Errorf("old = %q, want %q", old, c.wantOld)
			}
		})
	}
}

func TestEditorsNoLiteralVersion(t *testing.T) {
	cases := []struct {
		name string
		edit Editor
		in   string
	}{
		{"pom property undefined", Pom, `<project><version>${revision}</version></project>`},
		{"package.json empty object", PackageJSON, `{}`},
		{"pom version is CDATA", Pom, `<project><artifactId>a</artifactId><version><![CDATA[1]]></version></project>`},
		{"pom without artifactId", Pom, `<project><parent><version>1</version></parent></project>`},
		{"csproj without any element", Csproj, `<Project/>`},
		{"pyproject without project table", Pyproject, "[tool.black]\nline-length = 88\n"},
		{"gradle.properties is never added to", Properties(`(?:project\.)?version`), "org.gradle.jvmargs=-Xmx2g\n"},
		{"package.json non-string", PackageJSON, `{"version": 3}`},
		{"csproj property", Csproj, `<Version>$(Base)</Version>`},
		{"properties none", Properties(`version`), "name=app\n"},
		{"properties reference", Properties(`version`), "version=${v}\n"},
		{"pyproject dynamic", Pyproject, "[project]\ndynamic = [\"version\"]\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := c.edit([]byte(c.in), "2.0.0"); !errors.Is(err, ErrNoVersion) {
				t.Fatalf("err = %v, want ErrNoVersion", err)
			}
		})
	}
}

func TestEditorFor(t *testing.T) {
	for path, want := range map[string]bool{
		"pom.xml":               true,
		"sub/pom.xml":           false,
		"package.json":          true,
		"web/package.json":      false,
		"src/App/App.csproj":    true,
		"app.properties":        true,
		"gradle.properties":     true,
		"pyproject.toml":        true,
		"conf/custom.props":     true, // via propertyFiles
		"README.md":             false,
		"nested/app.properties": false,
	} {
		if got := EditorFor(path, Options{PropertyFiles: []string{"conf/custom.props"}}) != nil; got != want {
			t.Errorf("EditorFor(%q) matched = %v, want %v", path, got, want)
		}
	}
}

func TestCsprojMatchesElementNamesCaseInsensitively(t *testing.T) {
	out, old, err := Csproj([]byte("<Project><PropertyGroup><version>0.9.0</version></PropertyGroup></Project>"), "1.0.1")
	if err != nil || old != "0.9.0" || !strings.Contains(string(out), "<version>1.0.1</version>") {
		t.Errorf("got %q old=%q err=%v", out, old, err)
	}
}

func TestEditorsAddMissingVersion(t *testing.T) {
	cases := []struct {
		name string
		edit Editor
		in   string
		want string
	}{
		{
			name: "pom inheriting its version from the parent gets its own after artifactId",
			edit: Pom,
			in:   "<project>\n  <parent><groupId>g</groupId><version>9.9.9</version></parent>\n  <artifactId>a</artifactId>\n  <dependencies/>\n</project>\n",
			want: "<project>\n  <parent><groupId>g</groupId><version>9.9.9</version></parent>\n  <artifactId>a</artifactId>\n  <version>2.0.0</version>\n  <dependencies/>\n</project>\n",
		},
		{
			name: "pom with crlf line endings and a dependency artifactId earlier in the text",
			edit: Pom,
			in:   "<project>\r\n  <artifactId>a</artifactId>\r\n</project>\r\n",
			want: "<project>\r\n  <artifactId>a</artifactId>\r\n  <version>2.0.0</version>\r\n</project>\r\n",
		},
		{
			name: "package.json gets version after name, keeping indentation",
			edit: PackageJSON,
			in:   "{\n    \"name\": \"x\",\n    \"scripts\": {}\n}\n",
			want: "{\n    \"name\": \"x\",\n    \"version\": \"2.0.0\",\n    \"scripts\": {}\n}\n",
		},
		{
			name: "package.json whose name is the last key",
			edit: PackageJSON,
			in:   "{\n  \"name\": \"x\"\n}\n",
			want: "{\n  \"name\": \"x\",\n  \"version\": \"2.0.0\"\n}\n",
		},
		{
			name: "package.json without a name gets it first",
			edit: PackageJSON,
			in:   "{\n  \"private\": true\n}\n",
			want: "{\n  \"version\": \"2.0.0\",\n  \"private\": true\n}\n",
		},
		{
			name: "minified package.json",
			edit: PackageJSON,
			in:   `{"name":"x","main":"i.js"}`,
			want: `{"name":"x","version":"2.0.0","main":"i.js"}`,
		},
		{
			name: "csproj adds to its first unconditional property group",
			edit: CsprojAdding(""),
			in:   "<Project Sdk=\"Microsoft.NET.Sdk\">\n  <PropertyGroup Condition=\"'$(X)'=='1'\">\n    <A>1</A>\n  </PropertyGroup>\n  <PropertyGroup>\n    <TargetFramework>net8.0</TargetFramework>\n  </PropertyGroup>\n</Project>\n",
			want: "<Project Sdk=\"Microsoft.NET.Sdk\">\n  <PropertyGroup Condition=\"'$(X)'=='1'\">\n    <A>1</A>\n  </PropertyGroup>\n  <PropertyGroup>\n    <TargetFramework>net8.0</TargetFramework>\n    <Version>2.0.0</Version>\n  </PropertyGroup>\n</Project>\n",
		},
		{
			name: "csproj uses the requested element spelling and tab indentation",
			edit: CsprojAdding("version"),
			in:   "<Project>\r\n\t<PropertyGroup>\r\n\t\t<OutputType>Exe</OutputType>\r\n\t</PropertyGroup>\r\n</Project>\r\n",
			want: "<Project>\r\n\t<PropertyGroup>\r\n\t\t<OutputType>Exe</OutputType>\r\n\t\t<version>2.0.0</version>\r\n\t</PropertyGroup>\r\n</Project>\r\n",
		},
		{
			name: "csproj with no property group gets one",
			edit: CsprojAdding(""),
			in:   "<Project Sdk=\"Microsoft.NET.Sdk\">\n  <ItemGroup/>\n</Project>\n",
			want: "<Project Sdk=\"Microsoft.NET.Sdk\">\n  <PropertyGroup>\n    <Version>2.0.0</Version>\n  </PropertyGroup>\n  <ItemGroup/>\n</Project>\n",
		},
		{
			name: "csproj with an existing version is edited, not added to",
			edit: CsprojAdding("version"),
			in:   "<Project><PropertyGroup><Version>1.0.0</Version></PropertyGroup></Project>",
			want: "<Project><PropertyGroup><Version>2.0.0</Version></PropertyGroup></Project>",
		},
		{
			name: "pyproject adds under [project]",
			edit: Pyproject,
			in:   "[build-system]\nrequires = []\n\n[project]\nname = \"a\"\n",
			want: "[build-system]\nrequires = []\n\n[project]\nversion = \"2.0.0\"\nname = \"a\"\n",
		},
		{
			name: "pyproject adds under [tool.poetry] when there is no [project]",
			edit: Pyproject,
			in:   "[tool.poetry]\nname = \"a\"\n",
			want: "[tool.poetry]\nversion = \"2.0.0\"\nname = \"a\"\n",
		},
		{
			name: "app.properties gets a version line",
			edit: PropertiesAdding(`version`, "version"),
			in:   "name=app",
			want: "name=app\nversion=2.0.0\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, old, err := c.edit([]byte(c.in), "2.0.0")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("content:\n got %q\nwant %q", got, c.want)
			}
			if strings.Contains(c.name, "existing") != (old != "") {
				t.Errorf("old = %q (empty means a version was added)", old)
			}
		})
	}
}

func TestPyprojectDynamicVersionIsNotAddedTo(t *testing.T) {
	for _, in := range []string{
		"[project]\nname = \"a\"\ndynamic = [\"version\"]\n",
		"[project]\ndynamic = [\n  \"readme\",\n  \"version\",\n]\n",
	} {
		_, _, err := Pyproject([]byte(in), "2.0.0")
		if !errors.Is(err, ErrNoVersion) || errors.Is(err, ErrAbsent) {
			t.Errorf("%q: err = %v, want a non-absent ErrNoVersion", in, err)
		}
	}
}

func TestAddedVersionsStayWellFormed(t *testing.T) {
	out, _, err := PackageJSON([]byte("{\n  \"name\": \"x\",\n  \"dependencies\": {\"a\": \"1\"}\n}\n"), "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil || m["version"] != "2.0.0" {
		t.Errorf("package.json after adding: %s (%v)", out, err)
	}
	pom, _, err := Pom([]byte("<project><artifactId>a</artifactId></project>"), "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := xml.Unmarshal(pom, new(struct{})); err != nil {
		t.Errorf("pom after adding is not well-formed XML: %v\n%s", err, pom)
	}
}
