package bootstrap

import (
	"fmt"
	"path"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// isAzureFile reports whether a repo path is an Azure DevOps style file: a
// .yml/.yaml whose name starts with "azure" (azure-pipelines.yml,
// azure-pipelines-ci.yaml, .azure-pipelines.yml, ...), anywhere in the repo.
// Build output and dependency folders are ignored.
func isAzureFile(p string) bool {
	if inIgnoredDir(p) {
		return false
	}
	base := strings.ToLower(strings.TrimPrefix(path.Base(p), "."))
	ext := path.Ext(base)
	return strings.HasPrefix(base, "azure") && (ext == ".yml" || ext == ".yaml")
}

// planAzureDeletes plans removing every Azure file: the project is moving to
// GitLab CI, and a leftover Azure pipeline definition is dead weight that
// confuses which CI is the real one.
func planAzureDeletes(tree []*gitlab.TreeNode, bundled map[string]bool) []FileDiff {
	var diffs []FileDiff
	for _, n := range tree {
		if n.Type != "blob" || bundled[n.Path] || !isAzureFile(n.Path) {
			continue
		}
		diffs = append(diffs, FileDiff{TargetPath: n.Path, Action: ActionDelete,
			Description: "Azure DevOps file, will be deleted"})
	}
	return diffs
}

// writeRemovedSection asks the reviewer to confirm each deletion, since the
// match is by file name only.
func writeRemovedSection(b *strings.Builder, plan *Plan) {
	if plan == nil {
		return
	}
	var files []string
	for _, f := range plan.Files {
		if f.Action == ActionDelete {
			files = append(files, "`"+f.TargetPath+"`")
		}
	}
	if len(files) == 0 {
		return
	}
	b.WriteString("\n## Removed files\n\n")
	fmt.Fprintf(b, "- [ ] Confirm %s were only used by Azure DevOps and are safe to delete. Restore any that something else still needs.\n", strings.Join(files, ", "))
}
