package bootstrap

import (
	"context"
	"fmt"
	"sort"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"gitlab-ci-bootstrap/internal/config"
)

// slnInfo is a solution file at the repo root.
type slnInfo struct {
	Name    string
	Content string
}

// pickSolution chooses the solution file a pipeline builds. The value is a bare
// file name, so only a .sln at the repo root can be used; one in a subfolder is
// reported instead. With several at the root, the one that lists the chosen
// .csproj wins if exactly one does.
func pickSolution(root []slnInfo, nested []string, csproj string) (chosen, issue string) {
	switch len(root) {
	case 1:
		return root[0].Name, ""
	case 0:
		if len(nested) > 0 {
			return "", "no solution file at the repository root (found only " + quoteAll(nested) + ")"
		}
		return "", "no solution file was found"
	}

	if csproj != "" {
		// A .sln lists its projects relative to itself, with backslashes.
		ref := strings.ReplaceAll(csproj, "/", `\`)
		var matching []slnInfo
		for _, s := range root {
			if strings.Contains(s.Content, ref) {
				matching = append(matching, s)
			}
		}
		if len(matching) == 1 {
			return matching[0].Name, ""
		}
	}
	names := make([]string, len(root))
	for i, s := range root {
		names[i] = s.Name
	}
	return "", "several solution files at the repository root (" + quoteAll(names) + ")"
}

func quoteAll(items []string) string {
	q := make([]string, len(items))
	for i, it := range items {
		q[i] = "`" + it + "`"
	}
	return strings.Join(q, ", ")
}

// resolveSolution finds the .sln files in the tree and picks the one to build.
func (b *Bootstrapper) resolveSolution(ctx context.Context, plan *Plan, tree []*gitlab.TreeNode) (chosen, issue string, err error) {
	var rootNames, nested []string
	for _, n := range tree {
		if n.Type != "blob" || !strings.HasSuffix(strings.ToLower(n.Path), ".sln") || inIgnoredDir(n.Path) {
			continue
		}
		if strings.Contains(n.Path, "/") {
			nested = append(nested, n.Path)
		} else {
			rootNames = append(rootNames, n.Path)
		}
	}
	sort.Strings(rootNames)
	sort.Strings(nested)

	root := make([]slnInfo, 0, len(rootNames))
	for _, name := range rootNames {
		info := slnInfo{Name: name}
		if len(rootNames) > 1 { // content only matters to break a tie
			file, _, err := b.Client.REST.RepositoryFiles.GetFile(plan.ProjectID, name, &gitlab.GetFileOptions{
				Ref: gitlab.Ptr(plan.BaseBranch),
			}, gitlab.WithContext(ctx))
			if err != nil {
				return "", "", fmt.Errorf("reading %s: %w", name, err)
			}
			info.Content = decodedContent(file)
		}
		root = append(root, info)
	}
	chosen, issue = pickSolution(root, nested, plan.Csproj)
	return chosen, issue, nil
}

// writeSolutionSection asks for the solution file name by hand when it
// couldn't be determined.
func writeSolutionSection(b *strings.Builder, tmpl *config.Template, plan *Plan) {
	if tmpl == nil || plan == nil || plan.SolutionIssue == "" {
		return
	}
	var items []string
	for _, f := range tmpl.Files {
		for _, v := range f.ExtraVariables {
			if v.From == config.FromSolution {
				items = append(items, fmt.Sprintf("- [ ] Set `%s` in `%s` to the solution file name (e.g. `App.sln`) -- %s. It is blank until then.\n", v.Name, f.TargetPath, plan.SolutionIssue))
			}
		}
	}
	if len(items) == 0 {
		return
	}
	b.WriteString("\n## Solution file\n\n")
	for _, it := range items {
		b.WriteString(it)
	}
}
