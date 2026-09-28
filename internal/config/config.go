// Package config loads the template bundle definitions this tool offers.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Templates is every CI/CD template bundle this tool can add to a project.
type Templates struct {
	Templates []Template `yaml:"templates"`
}

// Template is one selectable pipeline-UI option: a name and every file it
// adds/replaces in the target project -- picking the template adds the
// whole bundle, there's no separate per-file choice.
type Template struct {
	Name  string     `yaml:"name"`
	Files []FileSpec `yaml:"files"`

	// Detect/DetectContentContains are used only by the `suggest` command,
	// an informational hint printed for whoever triggers the pipeline --
	// they never decide what gets applied. The human's template pick
	// (TEMPLATE_NAME pipeline variable) is the only thing Plan/Apply act on.
	Detect                []string `yaml:"detect,omitempty"`
	DetectContentContains string   `yaml:"detect_content_contains,omitempty"`
}

// FileSpec is one file the template bundle manages. SourcePath is a path
// within this repo's own checkout (e.g. "templates/java-maven-ci.gitlab.yml"
// or "files/maven-settings.xml") -- all template content lives here, read
// straight off disk in the CI job's own checkout. No remote-fetch: this
// tool briefly fetched .gitlab-ci.yml content from a separate centralized
// GitLab project, but that added a failure mode (wrong path/permissions/ref
// against a project this tool doesn't control) for no benefit once the
// actual templates moved into this repo instead.
type FileSpec struct {
	TargetPath string `yaml:"target_path"` // path in the destination project
	SourcePath string `yaml:"source_path"` // path in this repo
}

func LoadTemplates(path string) (*Templates, error) {
	var tpl Templates
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading templates %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &tpl); err != nil {
		return nil, fmt.Errorf("parsing templates %s: %w", path, err)
	}
	return &tpl, nil
}

func (t *Templates) Find(name string) *Template {
	for i := range t.Templates {
		if t.Templates[i].Name == name {
			return &t.Templates[i]
		}
	}
	return nil
}
