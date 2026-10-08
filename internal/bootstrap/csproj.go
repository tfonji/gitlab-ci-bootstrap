package bootstrap

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"gitlab-ci-bootstrap/internal/config"
)

// CsprojPlaceholder is the text a bundled file (see FileSpec.CsprojPlaceholder)
// holds where the project's .csproj path belongs.
const CsprojPlaceholder = "<path to .csproj file>"

// ignoredDirs never hold the project's own .csproj: build output and
// restored dependencies.
var ignoredDirs = map[string]bool{"bin": true, "obj": true, "packages": true, "node_modules": true}

type csprojInfo struct {
	Path       string
	Test       bool
	Deployable bool
}

var (
	testProjectName  = regexp.MustCompile(`(?i)[._-](unit|integration|ui)?tests?$`)
	testProjectRefs  = regexp.MustCompile(`(?i)Microsoft\.NET\.Test\.Sdk|<IsTestProject>\s*true|Include="(xunit|nunit|MSTest)`)
	exeOutputType    = regexp.MustCompile(`(?i)<OutputType>\s*(Win)?Exe\s*</OutputType>`)
	webProjectTypeID = "349c5851-65df-11da-9384-00065b846f21"
)

// classifyCsproj decides from a .csproj's content (and whether a Web.config
// sits beside it) whether it is a test project and whether it is something
// that gets deployed: a web app, or an executable/service.
func classifyCsproj(filePath, content string, hasWebConfig bool) csprojInfo {
	lower := strings.ToLower(content)
	name := strings.TrimSuffix(path.Base(filePath), path.Ext(filePath))
	return csprojInfo{
		Path: filePath,
		Test: testProjectName.MatchString(name) || testProjectRefs.MatchString(content),
		Deployable: hasWebConfig ||
			strings.Contains(lower, "sdk.web") ||
			strings.Contains(lower, webProjectTypeID) ||
			exeOutputType.MatchString(content),
	}
}

// pickCsproj chooses the .csproj a pipeline should version and build:
// test projects never count; a single remaining project is it; otherwise the
// single deployable one. When that leaves zero or several, it returns an issue
// instead -- one pipeline deploys one app, so guessing would be wrong.
func pickCsproj(all []csprojInfo) (chosen, issue string) {
	var candidates, deployable []csprojInfo
	for _, c := range all {
		if c.Test {
			continue
		}
		candidates = append(candidates, c)
		if c.Deployable {
			deployable = append(deployable, c)
		}
	}
	switch {
	case len(all) == 0:
		return "", "no .csproj file was found in the repository"
	case len(candidates) == 0:
		return "", "only test projects were found (" + joinPaths(all) + ")"
	case len(candidates) == 1:
		return candidates[0].Path, ""
	case len(deployable) == 1:
		return deployable[0].Path, ""
	case len(deployable) > 1:
		return "", "several deployable projects were found (" + joinPaths(deployable) + ")"
	default:
		return "", "several projects were found and none is clearly the deployable one (" + joinPaths(candidates) + ")"
	}
}

func joinPaths(cs []csprojInfo) string {
	paths := make([]string, len(cs))
	for i, c := range cs {
		paths[i] = "`" + c.Path + "`"
	}
	return strings.Join(paths, ", ")
}

// resolveCsproj finds the .csproj in the repo tree and picks the one to use.
func (b *Bootstrapper) resolveCsproj(ctx context.Context, plan *Plan, tree []*gitlab.TreeNode) (chosen, issue string, err error) {
	blobs := map[string]bool{}
	for _, n := range tree {
		if n.Type == "blob" {
			blobs[n.Path] = true
		}
	}
	var infos []csprojInfo
	for p := range blobs {
		if !strings.HasSuffix(strings.ToLower(p), ".csproj") || inIgnoredDir(p) {
			continue
		}
		file, _, err := b.Client.REST.RepositoryFiles.GetFile(plan.ProjectID, p, &gitlab.GetFileOptions{
			Ref: gitlab.Ptr(plan.BaseBranch),
		}, gitlab.WithContext(ctx))
		if err != nil {
			return "", "", fmt.Errorf("reading %s: %w", p, err)
		}
		dir := path.Dir(p)
		webConfig := "Web.config"
		if dir != "." {
			webConfig = dir + "/Web.config"
		}
		infos = append(infos, classifyCsproj(p, decodedContent(file), blobs[webConfig]))
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Path < infos[j].Path })
	chosen, issue = pickCsproj(infos)
	return chosen, issue, nil
}

func inIgnoredDir(p string) bool {
	segments := strings.Split(p, "/")
	for _, s := range segments[:len(segments)-1] {
		if ignoredDirs[strings.ToLower(s)] {
			return true
		}
	}
	return false
}

// writeCsprojSection asks for the .csproj path by hand when it couldn't be
// determined, once per place the template needs it.
func writeCsprojSection(b *strings.Builder, tmpl *config.Template, plan *Plan) {
	if tmpl == nil || plan == nil || plan.CsprojIssue == "" {
		return
	}
	b.WriteString("\n## .csproj path\n\n")
	for _, f := range tmpl.Files {
		if f.CsprojPlaceholder {
			fmt.Fprintf(b, "- [ ] Replace `%s` in `%s` with the path of the .csproj to version -- %s.\n", CsprojPlaceholder, f.TargetPath, plan.CsprojIssue)
		}
		for _, v := range f.ExtraVariables {
			if v.From == config.FromCsproj {
				fmt.Fprintf(b, "- [ ] Set `%s` in `%s` to the path of the .csproj to version -- %s. It is blank until then, so the shared template falls back to its own discovery.\n", v.Name, f.TargetPath, plan.CsprojIssue)
			}
		}
	}
}
