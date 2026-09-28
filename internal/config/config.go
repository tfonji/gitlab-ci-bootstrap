// Package config loads the template bundle definitions this tool offers.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Templates is every CI/CD template bundle this tool can add to a project.
type Templates struct {
	// RemoteSource is the centralized pipeline-templates project every
	// template's main .gitlab-ci.yml (Source == SourceRemote, the default)
	// is fetched from.
	RemoteSource RemoteSource `yaml:"remote_source"`
	Templates    []Template   `yaml:"templates"`
}

type RemoteSource struct {
	ProjectPath string `yaml:"project_path"` // "dso-templates/ci-cd-components/pipeline-templates"
	Ref         string `yaml:"ref"`
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

const (
	SourceRemote = "remote" // fetched from RemoteSource; this is the default when Source is empty
	SourceLocal  = "local"  // read from this repo's own checkout
)

// FileSpec is one file the template bundle manages. Most templates have a
// single remote-sourced file (the shared .gitlab-ci.yml, from the separate
// dso-templates/ci-cd-components/pipeline-templates project); a template
// can also bundle locally-sourced files this tool owns (e.g. settings.xml
// with Nexus/Artifactory credentials) that have no reason to live in that
// shared templates project.
type FileSpec struct {
	TargetPath string `yaml:"target_path"`      // path in the destination project
	SourcePath string `yaml:"source_path"`      // path within RemoteSource, or within this repo if Source == "local"
	Source     string `yaml:"source,omitempty"` // "remote" (default) or "local"
}

func (f FileSpec) IsLocal() bool {
	return f.Source == SourceLocal
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
