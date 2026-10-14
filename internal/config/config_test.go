package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIISTemplatesSetTheDeployWhenVariablesAtTheTopLevelGroup(t *testing.T) {
	tpl, err := LoadTemplates("../../configs/templates.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"DEVELOPMENT_DEPLOY_WHEN", "TESTING_DEPLOY_WHEN", "STAGING_DEPLOY_WHEN", "STAGING_DR_DEPLOY_WHEN", "PERFORMANCE_DEPLOY_WHEN"}
	for _, name := range []string{"node-iis-ci-cd", "dotnet-core-iis-ci-cd", "dotnet-framework-iis-ci-cd"} {
		tmpl := tpl.Find(name)
		if tmpl == nil || len(tmpl.Variables) != len(want) {
			t.Fatalf("%s: variables = %+v", name, tmpl)
		}
		for i, v := range tmpl.Variables {
			if v.Name != want[i] || v.Value != "on_success" || v.Scope != ScopeGroup || v.Group != GroupTop ||
				v.Masked || v.Protected || !v.ShouldOverwrite() {
				t.Errorf("%s: %+v, want %s=on_success, group scope, top-level, unmasked, overwrite", name, v, want[i])
			}
		}
		if tmpl.MRChecklist != nil {
			for _, listed := range tmpl.MRChecklist.PerProject {
				if strings.HasSuffix(listed, "_DEPLOY_WHEN") {
					t.Errorf("%s: %s is set by the tool and shouldn't be on the checklist", name, listed)
				}
			}
		}
	}
}

func TestLoadTemplatesRejectsBadVariables(t *testing.T) {
	for name, entry := range map[string]string{
		"no name":          "- value: x\n        scope: group",
		"bad scope":        "- name: A\n        scope: org",
		"bad group":        "- name: A\n        scope: group\n        group: root",
		"group on project": "- name: A\n        scope: project\n        group: top",
	} {
		path := filepath.Join(t.TempDir(), "t.yaml")
		yaml := "templates:\n  - name: t\n    ci_variables:\n      " + entry + "\n"
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTemplates(path); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
