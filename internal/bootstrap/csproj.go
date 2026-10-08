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
	Content    string
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
func (b *Bootstrapper) resolveCsproj(ctx context.Context, plan *Plan, tree []*gitlab.TreeNode) (chosen, issue, content string, err error) {
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
			return "", "", "", fmt.Errorf("reading %s: %w", p, err)
		}
		dir := path.Dir(p)
		webConfig := "Web.config"
		if dir != "." {
			webConfig = dir + "/Web.config"
		}
		text := decodedContent(file)
		info := classifyCsproj(p, text, blobs[webConfig])
		info.Content = text
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Path < infos[j].Path })
	chosen, issue = pickCsproj(infos)
	for _, i := range infos {
		if i.Path == chosen {
			content = i.Content
		}
	}
	return chosen, issue, content, nil
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

// writeCsprojSection asks for what couldn't be determined about the .csproj
// by hand, and spells out the values the pipeline's variables must match.
func writeCsprojSection(b *strings.Builder, tmpl *config.Template, plan *Plan) {
	if tmpl == nil || plan == nil {
		return
	}
	var items []string
	if plan.CsprojIssue != "" {
		for _, f := range tmpl.Files {
			if f.CsprojPlaceholder {
				items = append(items, fmt.Sprintf("Replace `%s` in `%s` with the path of the .csproj to version -- %s.", CsprojPlaceholder, f.TargetPath, plan.CsprojIssue))
			}
			for _, v := range f.ExtraVariables {
				if v.From == config.FromCsproj {
					items = append(items, fmt.Sprintf("Set `%s` in `%s` to the path of the .csproj to version -- %s. It is blank until then, so the shared template falls back to its own discovery.", v.Name, f.TargetPath, plan.CsprojIssue))
				}
			}
		}
		if tmpl.CsprojApplicationName {
			items = append(items, "Add an `<ApplicationName>` element to that .csproj"+artifactNameSuffix(tmpl, "")+".")
		}
	}
	if plan.ApplicationName != "" {
		item := fmt.Sprintf("Confirm `<ApplicationName>%s</ApplicationName>` in `%s`", plan.ApplicationName, plan.Csproj)
		if plan.ApplicationNameAdded {
			item = fmt.Sprintf("Confirm `<ApplicationName>%s</ApplicationName>`, added to `%s` (the file name)", plan.ApplicationName, plan.Csproj)
		}
		items = append(items, item+artifactNameSuffix(tmpl, plan.ApplicationName)+".")
	}
	if len(items) == 0 {
		return
	}
	b.WriteString("\n## .csproj\n\n")
	for _, it := range items {
		b.WriteString("- [ ] " + it + "\n")
	}
}

// artifactNameSuffix ties <ApplicationName> to the ARTIFACT_NAME variable for
// templates that have one, which must match it exactly.
func artifactNameSuffix(tmpl *config.Template, name string) string {
	if tmpl.MRChecklist == nil {
		return ""
	}
	for _, v := range tmpl.MRChecklist.PerProject {
		if v == "ARTIFACT_NAME" {
			if name == "" {
				return ", and set `ARTIFACT_NAME` to the same value"
			}
			return fmt.Sprintf(", and set `ARTIFACT_NAME` to `%s`", name)
		}
	}
	return ""
}
