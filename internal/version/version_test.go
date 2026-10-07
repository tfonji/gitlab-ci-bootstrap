package version

import (
	"errors"
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
		{"pom inherits parent", Pom, `<project><parent><version>1</version></parent></project>`},
		{"pom property undefined", Pom, `<project><version>${revision}</version></project>`},
		{"package.json missing", PackageJSON, `{"name":"x"}`},
		{"package.json non-string", PackageJSON, `{"version": 3}`},
		{"csproj property", Csproj, `<Version>$(Base)</Version>`},
		{"csproj none", Csproj, `<Project/>`},
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
		if got := EditorFor(path, []string{"conf/custom.props"}) != nil; got != want {
			t.Errorf("EditorFor(%q) matched = %v, want %v", path, got, want)
		}
	}
}
