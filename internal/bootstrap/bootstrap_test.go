package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gitlab-ci-bootstrap/internal/config"
	"gitlab-ci-bootstrap/internal/gitlabclient"
)

// fakeGitLab serves just enough of the GitLab REST API for Plan and Apply:
// one project (id 7, default branch main) with the given tags and files.
type fakeGitLab struct {
	tags  []string // most recent first
	files map[string]string

	// Leftovers from a previous attempt, and what happens when they're removed.
	branchExists bool
	existingMRs  []map[string]any // each: iid, state
	denyMRDelete bool
	calls        []string // ordered record of every mutating call
	mu           sync.Mutex
	commit       map[string]any
	mrBody       map[string]any
	mrCount      int
	groupQuery   map[string]string // include_subgroups / with_shared as last requested
}

func (f *fakeGitLab) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/7")
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.URL.Path == "/api/v4/groups/5/projects":
		f.mu.Lock()
		f.groupQuery = map[string]string{
			"include_subgroups": r.URL.Query().Get("include_subgroups"),
			"with_shared":       r.URL.Query().Get("with_shared"),
		}
		f.mu.Unlock()
		// Two pages, to prove pagination is followed.
		if r.URL.Query().Get("page") == "2" {
			writeJSON([]map[string]any{
				{"id": 10, "path_with_namespace": "acme/sub/forked", "forked_from_project": map[string]any{"path_with_namespace": "other/orig"}},
				{"id": 11, "path_with_namespace": "acme/sub/excluded"},
			})
			return
		}
		w.Header().Set("X-Next-Page", "2")
		writeJSON([]map[string]any{
			{"id": 7, "path_with_namespace": "acme/app"},
			{"id": 8, "path_with_namespace": "acme/old", "archived": true},
			{"id": 9, "path_with_namespace": "acme/blank", "empty_repo": true},
		})
	case r.URL.Path == "/api/v4/groups/404/projects":
		http.Error(w, `{"message":"404 Group Not Found"}`, http.StatusNotFound)
	case r.URL.Path == "/api/v4/projects/7":
		writeJSON(map[string]any{"id": 7, "path_with_namespace": "acme/app", "default_branch": "main"})
	case p == "/repository/tags":
		var tags []map[string]any
		for _, t := range f.tags {
			tags = append(tags, map[string]any{"name": t})
		}
		if len(tags) > 1 && r.URL.Query().Get("per_page") == "1" {
			tags = tags[:1]
		}
		writeJSON(tags)
	case p == "/repository/tree":
		var nodes []map[string]any
		for path := range f.files {
			nodes = append(nodes, map[string]any{"type": "blob", "path": path, "name": filepath.Base(path)})
		}
		writeJSON(nodes)
	case strings.HasPrefix(p, "/repository/files/"):
		path := strings.TrimPrefix(p, "/repository/files/")
		content, ok := f.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(map[string]any{"file_path": path, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))})
	case p == "/merge_requests" && r.Method == http.MethodGet:
		mrs := []map[string]any{}
		for _, mr := range f.existingMRs {
			mrs = append(mrs, map[string]any{"iid": mr["iid"], "state": mr["state"], "source_branch": r.URL.Query().Get("source_branch")})
		}
		writeJSON(mrs)
	case strings.HasPrefix(p, "/merge_requests/") && r.Method == http.MethodDelete:
		iid := strings.TrimPrefix(p, "/merge_requests/")
		if f.denyMRDelete {
			http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
			return
		}
		f.record("delete-mr " + iid)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(p, "/merge_requests/") && r.Method == http.MethodPut:
		f.record("close-mr " + strings.TrimPrefix(p, "/merge_requests/"))
		writeJSON(map[string]any{"iid": 1, "state": "closed"})
	case strings.HasPrefix(p, "/repository/branches/") && r.Method == http.MethodGet:
		if !f.branchExists {
			http.NotFound(w, r)
			return
		}
		writeJSON(map[string]any{"name": strings.TrimPrefix(p, "/repository/branches/")})
	case strings.HasPrefix(p, "/repository/branches/") && r.Method == http.MethodDelete:
		f.record("delete-branch")
		w.WriteHeader(http.StatusNoContent)
	case p == "/repository/commits" && r.Method == http.MethodPost:
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&f.commit)
		f.calls = append(f.calls, "commit")
		writeJSON(map[string]any{"id": "abc"})
	case p == "/merge_requests" && r.Method == http.MethodPost:
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&f.mrBody)
		f.mrCount++
		f.calls = append(f.calls, "create-mr")
		writeJSON(map[string]any{"iid": 3, "web_url": "https://gitlab.example.com/acme/app/-/merge_requests/3"})
	default:
		http.NotFound(w, r)
	}
}

func newTestBootstrapper(t *testing.T, fake *fakeGitLab) *Bootstrapper {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client, err := gitlabclient.New(srv.URL, "token")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	versionTxt := filepath.Join(dir, "version.txt")
	if err := os.WriteFile(versionTxt, []byte("version: x.y.z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return New(client, &config.Templates{
		RemoteSource: config.RemoteSource{ProjectPath: "shared/templates", Ref: "master"},
		Templates: []config.Template{{
			Name: "demo",
			Files: []config.FileSpec{
				{TargetPath: ".gitlab-ci.yml", SourcePath: "templates/demo.gitlab.yml"},
				{TargetPath: "version.txt", SourcePath: versionTxt, Source: config.SourceLocal},
			},
		}},
	})
}

func planFiles(p *Plan) map[string]FileDiff {
	m := map[string]FileDiff{}
	for _, f := range p.Files {
		m[f.TargetPath] = f
	}
	return m
}

func TestPlanVersions(t *testing.T) {
	pom := "<project>\n  <version>0.0.1-SNAPSHOT</version>\n</project>\n"
	cases := []struct {
		name        string
		tags        []string
		wantVersion string
	}{
		{"no tags starts at 1.0.0", nil, "1.0.0"},
		{"x.y.z tag bumps patch", []string{"2.4.7", "1.0.0"}, "2.4.8"},
		{"non x.y.z latest tag starts at 1.0.0", []string{"v3.1.0", "2.4.7"}, "1.0.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBootstrapper(t, &fakeGitLab{tags: c.tags, files: map[string]string{"pom.xml": pom}})
			plan, err := b.Plan(context.Background(), 7, "demo")
			if err != nil {
				t.Fatal(err)
			}
			if plan.Version != c.wantVersion {
				t.Fatalf("Version = %q, want %q", plan.Version, c.wantVersion)
			}
			files := planFiles(plan)
			if got := files["pom.xml"]; got.Action != "update" || !got.Edit || !strings.Contains(got.Description, c.wantVersion) {
				t.Errorf("pom.xml diff = %+v", got)
			}
			if got := files["version.txt"]; got.Action != "create" {
				t.Errorf("version.txt diff = %+v", got)
			}
		})
	}
}

func TestPlanVersionFileStates(t *testing.T) {
	b := newTestBootstrapper(t, &fakeGitLab{tags: []string{"1.2.3"}, files: map[string]string{
		"pom.xml":      "<project><version>1.2.4</version></project>",
		"package.json": `{"name":"x"}`,
		"README.md":    "ignored",
		"sub/pom.xml":  "<project><version>9</version></project>",
	}})
	plan, err := b.Plan(context.Background(), 7, "demo")
	if err != nil {
		t.Fatal(err)
	}
	files := planFiles(plan)
	if got := files["pom.xml"]; got.Action != "unchanged" {
		t.Errorf("pom.xml already at target version: %+v", got)
	}
	if got := files["package.json"]; got.Action != "skipped" {
		t.Errorf("package.json without a version should be skipped: %+v", got)
	}
	for _, ignored := range []string{"README.md", "sub/pom.xml"} {
		if _, ok := files[ignored]; ok {
			t.Errorf("%s should not be treated as a version file", ignored)
		}
	}
}

func TestApplyWritesVersionAndExplainsInMR(t *testing.T) {
	fake := &fakeGitLab{tags: []string{"1.0.0"}, files: map[string]string{
		"pom.xml":      "<project>\n  <version>0.0.1-SNAPSHOT</version>\n</project>\n",
		"package.json": `{"name":"x"}`,
	}}
	b := newTestBootstrapper(t, fake)
	plan, err := b.Plan(context.Background(), 7, "demo")
	if err != nil {
		t.Fatal(err)
	}
	result, err := b.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "applied" || result.MRURL == "" {
		t.Fatalf("result = %+v", result)
	}

	committed := map[string]string{}
	for _, a := range fake.commit["actions"].([]any) {
		m := a.(map[string]any)
		committed[m["file_path"].(string)] = m["content"].(string)
	}
	if got, want := committed["pom.xml"], "<project>\n  <version>1.0.1</version>\n</project>\n"; got != want {
		t.Errorf("pom.xml committed as %q, want %q", got, want)
	}
	if got := committed["version.txt"]; got != "version: 1.0.1\n" {
		t.Errorf("version.txt committed as %q", got)
	}
	if _, ok := committed["package.json"]; ok {
		t.Error("package.json has no literal version and must not be committed")
	}

	desc, _ := fake.mrBody["description"].(string)
	for _, want := range []string{
		"## Group-level CI/CD variables",
		"- [ ] `IS_ARTIFACTORY_ENABLED` -- set to `true`",
		"## Version",
		"Version set to `1.0.1` (latest tag 1.0.0, patch bumped) in `version.txt`, `pom.xml`",
		"Set the version to `1.0.1` by hand in `package.json`",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("MR description missing %q:\n%s", want, desc)
		}
	}
}

func newApplyFixture(t *testing.T, fake *fakeGitLab) (*Bootstrapper, *Plan) {
	t.Helper()
	b := newTestBootstrapper(t, fake)
	plan, err := b.Plan(context.Background(), 7, "demo")
	if err != nil {
		t.Fatal(err)
	}
	return b, plan
}

func TestApplyReplacesPreviousAttempt(t *testing.T) {
	fake := &fakeGitLab{
		tags:         []string{"1.0.0"},
		files:        map[string]string{"pom.xml": "<project><version>0.0.1</version></project>"},
		branchExists: true,
		existingMRs: []map[string]any{
			{"iid": 4, "state": "opened"},
			{"iid": 5, "state": "closed"},
			{"iid": 6, "state": "merged"},
		},
	}
	b, plan := newApplyFixture(t, fake)
	result, err := b.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}

	want := "delete-mr 4, delete-mr 5, delete-branch, commit, create-mr"
	if got := strings.Join(fake.calls, ", "); got != want {
		t.Errorf("call order = %q, want %q (merged MR !6 must be left alone)", got, want)
	}
	if fake.commit["start_branch"] != "main" {
		t.Errorf("branch must be recreated from the base branch, start_branch = %v", fake.commit["start_branch"])
	}
	if result.Status != "applied" || !strings.Contains(result.Description, "deleted MR !4, deleted MR !5, deleted branch feature/add-demo") {
		t.Errorf("result = %+v", result)
	}
}

func TestApplyClosesMRWhenDeleteIsForbidden(t *testing.T) {
	fake := &fakeGitLab{
		tags:         []string{"1.0.0"},
		files:        map[string]string{},
		branchExists: true,
		existingMRs:  []map[string]any{{"iid": 4, "state": "opened"}},
		denyMRDelete: true,
	}
	b, plan := newApplyFixture(t, fake)
	result, err := b.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.calls, ", "); got != "close-mr 4, delete-branch, commit, create-mr" {
		t.Errorf("call order = %q", got)
	}
	if !strings.Contains(result.Description, "closed MR !4") {
		t.Errorf("result = %+v", result)
	}
}

func TestApplyWithNothingToCommitDeletesNothing(t *testing.T) {
	// version.txt already matches the template output, and there is no
	// .gitlab-ci.yml diff -- so no commit is needed and the old branch/MR stay.
	fake := &fakeGitLab{
		tags:         []string{"1.0.0"},
		files:        map[string]string{".gitlab-ci.yml": "", "version.txt": "version: 1.0.1\n"},
		branchExists: true,
		existingMRs:  []map[string]any{{"iid": 4, "state": "opened"}},
	}
	b, plan := newApplyFixture(t, fake)
	for i := range plan.Files {
		plan.Files[i].Action = "unchanged"
	}
	result, err := b.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "unchanged" || len(fake.calls) != 0 {
		t.Errorf("status = %q, calls = %v; nothing should be deleted when there is nothing to commit", result.Status, fake.calls)
	}
}

func TestMRDescriptionLinksArtifactoryOnboardingOnlyForCITemplates(t *testing.T) {
	cases := map[string]bool{
		"java-maven-ci":         true,
		"node-iis-ci-cd":        true,
		"dotnet-core-iis-ci-cd": true,
		"python-ci":             true,
		"ansible-cd":            false,
		"flyway-cd":             false,
		"terraform-cd":          false,
		"terraform-cli-cd":      false,
	}
	for name, wantLink := range cases {
		desc := mrDescription(&config.Template{Name: name}, nil)
		if got := strings.Contains(desc, artifactoryOnboardingURL); got != wantLink {
			t.Errorf("%s: onboarding link present = %v, want %v", name, got, wantLink)
		}
	}
	if strings.Contains(mrDescription(nil, nil), artifactoryOnboardingURL) {
		t.Error("no template means no CI component, so no Artifactory link")
	}
}

func planByPath(plans []*Plan) map[string]*Plan {
	m := map[string]*Plan{}
	for _, p := range plans {
		m[p.ProjectPath] = p
	}
	return m
}

func TestPlanAllExpandsGroupAndSkipsFilteredProjects(t *testing.T) {
	fake := &fakeGitLab{files: map[string]string{}}
	b := newTestBootstrapper(t, fake)

	var streamed int
	plans := b.PlanAll(context.Background(), PlanRequest{
		Template: "demo",
		Groups:   []string{"5"},
		Exclude:  []string{"11"},
		Group:    GroupOptions{IncludeSubgroups: true},
	}, func(*Plan) { streamed++ })

	if streamed != len(plans) || len(plans) != 5 {
		t.Fatalf("got %d plans (%d streamed), want 5 across both pages: %+v", len(plans), streamed, plans)
	}
	if fake.groupQuery["include_subgroups"] != "true" || fake.groupQuery["with_shared"] != "false" {
		t.Errorf("group listing query = %v, want include_subgroups=true with_shared=false", fake.groupQuery)
	}
	got := planByPath(plans)
	if p := got["acme/app"]; p.Skipped != "" || p.Error != "" || p.Branch == "" {
		t.Errorf("acme/app should be planned normally: %+v", p)
	}
	for path, want := range map[string]string{
		"acme/old":          "archived project",
		"acme/blank":        "empty repository",
		"acme/sub/forked":   "fork of other/orig",
		"acme/sub/excluded": "excluded via EXCLUDE_PROJECT_IDS",
	} {
		if got[path].Skipped != want {
			t.Errorf("%s skipped = %q, want %q", path, got[path].Skipped, want)
		}
	}
}

func TestPlanAllPassesSubgroupsFlagOn(t *testing.T) {
	fake := &fakeGitLab{files: map[string]string{}}
	b := newTestBootstrapper(t, fake)

	b.PlanAll(context.Background(), PlanRequest{Template: "demo", Groups: []string{"5"}}, nil)
	if fake.groupQuery["include_subgroups"] != "false" {
		t.Errorf("include_subgroups = %q, want false", fake.groupQuery["include_subgroups"])
	}
}

func TestPlanAllDedupesAndExplicitProjectsBypassFiltersButNotExclude(t *testing.T) {
	fake := &fakeGitLab{files: map[string]string{}}
	b := newTestBootstrapper(t, fake)

	// 7 is named and also in the group; 8 (archived) is named explicitly, so
	// the archived filter must not apply to it; 9 is named but also excluded.
	plans := b.PlanAll(context.Background(), PlanRequest{
		Template: "demo",
		Projects: []string{"7", "8", "9"},
		Groups:   []string{"5"},
		Exclude:  []string{"9"},
		Group:    GroupOptions{},
	}, nil)

	count := map[int64]int{}
	for _, p := range plans {
		count[p.ProjectID]++
	}
	for id, n := range count {
		if n != 1 {
			t.Errorf("project %d planned %d times, want once", id, n)
		}
	}
	for _, p := range plans {
		switch p.ProjectID {
		case 8:
			if p.Skipped != "" {
				t.Errorf("explicitly named archived project was skipped: %q", p.Skipped)
			}
		case 9:
			if p.Skipped != "excluded via EXCLUDE_PROJECT_IDS" {
				t.Errorf("excluded project: skipped = %q", p.Skipped)
			}
		}
	}
}

func TestPlanAllUnknownGroupIsAFailedPlanAndDoesNotStopTheRest(t *testing.T) {
	fake := &fakeGitLab{files: map[string]string{}}
	b := newTestBootstrapper(t, fake)

	plans := b.PlanAll(context.Background(), PlanRequest{Template: "demo", Projects: []string{"7"}, Groups: []string{"404"}}, nil)
	if len(plans) != 2 {
		t.Fatalf("got %d plans, want 2: %+v", len(plans), plans)
	}
	if plans[0].Error != "" || plans[0].Branch == "" {
		t.Errorf("project 7 should still be planned: %+v", plans[0])
	}
	if plans[1].ProjectPath != "group 404" || plans[1].Error == "" {
		t.Errorf("unknown group should be a failed plan: %+v", plans[1])
	}
}

func TestApplyPassesSkippedPlanThrough(t *testing.T) {
	fake := &fakeGitLab{files: map[string]string{}}
	b := newTestBootstrapper(t, fake)

	res, err := b.Apply(context.Background(), &Plan{ProjectID: 8, ProjectPath: "acme/old", Template: "demo", Skipped: "archived project"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "skipped" || res.Description != "archived project" {
		t.Errorf("result = %+v, want skipped: archived project", res)
	}
	if len(fake.calls) != 0 {
		t.Errorf("skipped plan made calls: %v", fake.calls)
	}
}
