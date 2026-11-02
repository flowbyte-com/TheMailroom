package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// releaseWorkflow is the path of the release workflow, relative to the
// repository root.
const releaseWorkflow = ".github/workflows/release.yml"

// TestReleaseWorkflowPublishesOnlyFromATag pins the property that makes
// the release path safe: the workflow can only be started by pushing a
// version tag.
//
// There is no pull_request trigger, no branch-push trigger, no
// workflow_dispatch trigger and no schedule trigger. An ordinary branch
// or a pull request therefore cannot start this workflow at all, which is
// a stronger property than checking inside a job: there is no code path
// that a branch push could reach.
func TestReleaseWorkflowPublishesOnlyFromATag(t *testing.T) {
	doc := loadWorkflow(t)

	on := mappingGet(t, doc, "on")
	if on == nil {
		t.Fatal("the release workflow has no \"on\" section")
	}
	if on.Kind != yaml.MappingNode {
		t.Fatalf("the \"on\" section is a %v, want a mapping", on.Kind)
	}

	// The only trigger is a tag push.
	if len(on.Content) != 2 {
		t.Errorf("the \"on\" section has %d keys, want exactly one (push): %v",
			len(on.Content)/2, mappingKeys(on))
	}
	push := mappingGet(t, on, "push")
	if push == nil {
		t.Fatalf("the release workflow has no \"push\" trigger: %v", mappingKeys(on))
	}
	if len(push.Content) != 2 {
		t.Errorf("the \"push\" trigger has %d keys, want exactly one (tags): %v",
			len(push.Content)/2, mappingKeys(push))
	}
	tags := mappingGet(t, push, "tags")
	if tags == nil {
		t.Fatalf("the \"push\" trigger has no \"tags\" filter: %v", mappingKeys(push))
	}

	// A branch filter would make an ordinary branch push publish.
	if mappingGet(t, push, "branches") != nil {
		t.Error("the \"push\" trigger has a \"branches\" filter; a branch push must not publish")
	}
	if mappingGet(t, push, "paths") != nil {
		t.Error("the \"push\" trigger has a \"paths\" filter")
	}

	// The tag pattern must be a version tag.
	var patterns []string
	if tags.Kind == yaml.SequenceNode {
		for _, item := range tags.Content {
			patterns = append(patterns, item.Value)
		}
	} else {
		patterns = append(patterns, tags.Value)
	}
	if len(patterns) == 0 {
		t.Error("the \"tags\" filter is empty")
	}
	for _, pattern := range patterns {
		if !strings.HasPrefix(pattern, "v") {
			t.Errorf("the tag pattern %q does not require a version tag", pattern)
		}
	}

	// The triggers that must never be present.
	for _, forbidden := range []string{
		"pull_request", "pull_request_target", "workflow_dispatch",
		"schedule", "repository_dispatch", "workflow_run", "issue_comment",
	} {
		if mappingGet(t, on, forbidden) != nil {
			t.Errorf("the release workflow has a %q trigger; it must publish only from a version tag", forbidden)
		}
	}
}

// TestReleaseWorkflowPublishesOnlyVerifiedArtifacts pins that the
// publish job cannot run on its own and cannot build its own artifacts.
//
// The publish job must depend on the build job, which is the job that
// verifies the artifacts, and it must not contain a build step: a
// release that published binaries it had not verified would defeat the
// point of the verification.
func TestReleaseWorkflowPublishesOnlyVerifiedArtifacts(t *testing.T) {
	doc := loadWorkflow(t)

	jobs := mappingGet(t, doc, "jobs")
	if jobs == nil {
		t.Fatal("the release workflow has no \"jobs\" section")
	}
	publish := mappingGet(t, jobs, "publish")
	if publish == nil {
		t.Fatal("the release workflow has no \"publish\" job")
	}
	build := mappingGet(t, jobs, "build")
	if build == nil {
		t.Fatal("the release workflow has no \"build\" job")
	}

	needs := mappingGet(t, publish, "needs")
	if needs == nil {
		t.Fatal("the \"publish\" job has no \"needs\"; it must depend on the job that verifies the artifacts")
	}
	if !nodeMentions(needs, "build") {
		t.Errorf("the \"publish\" job does not depend on the \"build\" job: %v", needs.Value)
	}

	// The publish job must not build the artifacts it uploads.
	for _, step := range jobSteps(t, publish) {
		if name := stepName(step); strings.Contains(strings.ToLower(name), "build") {
			t.Errorf("the \"publish\" job has a step named %q; it must upload the artifacts the build job verified", name)
		}
		if run := stepRun(step); strings.Contains(run, "make release") {
			t.Errorf("the \"publish\" job runs %q; it must not rebuild the artifacts", strings.TrimSpace(run))
		}
	}

	// The build job must actually verify the artifacts.
	var verifies bool
	for _, step := range jobSteps(t, build) {
		if strings.Contains(stepRun(step), "make verify") {
			verifies = true
		}
	}
	if !verifies {
		t.Error("the \"build\" job does not verify the artifacts")
	}
}

// TestReleaseWorkflowChecksTheTagIsOnMain pins the guard that stops a
// tag on a side branch from publishing.
//
// A tag is a ref anyone with push access can create, so the tag alone
// does not establish that the commit is on main. The workflow must check
// the ancestry explicitly.
func TestReleaseWorkflowChecksTheTagIsOnMain(t *testing.T) {
	doc := loadWorkflow(t)
	jobs := mappingGet(t, doc, "jobs")

	for _, name := range []string{"build", "publish"} {
		job := mappingGet(t, jobs, name)
		if job == nil {
			t.Fatalf("the release workflow has no %q job", name)
		}
		var found bool
		for _, step := range jobSteps(t, job) {
			run := stepRun(step)
			if strings.Contains(run, "merge-base --is-ancestor") {
				found = true
			}
		}
		if !found {
			t.Errorf("the %q job does not check that the tagged commit is on main", name)
		}
	}
}

// TestReleaseWorkflowChecksTheTagMatchesTheVersionFile pins the guard
// that stops a tag whose version does not match the source.
//
// The version in the source tree and the version in the tag must agree,
// or the release would ship a binary that reports a version the
// repository does not claim.
func TestReleaseWorkflowChecksTheTagMatchesTheVersionFile(t *testing.T) {
	doc := loadWorkflow(t)
	jobs := mappingGet(t, doc, "jobs")
	build := mappingGet(t, jobs, "build")
	if build == nil {
		t.Fatal("the release workflow has no \"build\" job")
	}

	var found bool
	for _, step := range jobSteps(t, build) {
		run := stepRun(step)
		if strings.Contains(run, "VERSION") && strings.Contains(run, "GITHUB_REF_NAME") {
			found = true
		}
	}
	if !found {
		t.Error("the \"build\" job does not check that the tag matches the VERSION file")
	}
}

// TestReleaseWorkflowScopesWritePermission pins that only the publish
// job may write.
//
// The build job runs arbitrary build steps, so it must not be able to
// write to the repository. A workflow-level write permission would give
// every step, including the build, the ability to push.
func TestReleaseWorkflowScopesWritePermission(t *testing.T) {
	doc := loadWorkflow(t)

	permissions := mappingGet(t, doc, "permissions")
	if permissions == nil {
		t.Fatal("the release workflow declares no top-level permissions")
	}
	if value := permissions.Value; value != "" && value != "read" {
		t.Errorf("the top-level permissions are %q, want a read-only default", value)
	}
	if mappingGet(t, permissions, "contents") == nil {
		contents := permissions
		if contents.Kind != yaml.ScalarNode || contents.Value != "read" {
			t.Errorf("the top-level permissions are %q, want \"contents: read\"", permissions.Value)
		}
	}

	jobs := mappingGet(t, doc, "jobs")
	build := mappingGet(t, jobs, "build")
	if build == nil {
		t.Fatal("the release workflow has no \"build\" job")
	}
	buildPerms := mappingGet(t, build, "permissions")
	if buildPerms != nil {
		if contents := mappingGet(t, buildPerms, "contents"); contents != nil && contents.Value != "read" {
			t.Errorf("the \"build\" job has contents permission %q, want \"read\"", contents.Value)
		}
	}

	publish := mappingGet(t, jobs, "publish")
	if publish == nil {
		t.Fatal("the release workflow has no \"publish\" job")
	}
	publishPerms := mappingGet(t, publish, "permissions")
	if publishPerms == nil {
		t.Fatal("the \"publish\" job declares no permissions")
	}
	contents := mappingGet(t, publishPerms, "contents")
	if contents == nil || contents.Value != "write" {
		t.Errorf("the \"publish\" job has contents permission %v, want \"write\"", contents)
	}
}

// TestReleaseWorkflowPinsTheReleaseToolchain pins that the release
// workflow builds with the Go version the module requires.
//
// The Go version is part of the binary, so two builds of the same commit
// with different Go versions are not identical. Pinning the toolchain to
// the version in go.mod keeps the module file the single source of truth
// and makes the reproducibility claim hold.
func TestReleaseWorkflowPinsTheReleaseToolchain(t *testing.T) {
	root := repoRoot(t)
	want, err := RequiredToolchain(root)
	if err != nil {
		t.Fatalf("RequiredToolchain: %v", err)
	}

	// The release workflow pins the toolchain for both of its jobs.
	doc := loadWorkflow(t)
	env := mappingGet(t, doc, "env")
	if env == nil {
		t.Fatal("the release workflow declares no environment")
	}
	got := mappingGet(t, env, "GO_VERSION")
	if got == nil {
		t.Fatal("the release workflow does not pin GO_VERSION")
	}
	if got.Value != want {
		t.Errorf("the release workflow pins Go %q, want the version the module requires (%q)", got.Value, want)
	}

	// The continuous integration release job pins the same version, so
	// that the artifacts CI builds match the artifacts a release
	// publishes.
	ci := loadCIWorkflow(t)
	ciJobs := mappingGet(t, ci, "jobs")
	release := mappingGet(t, ciJobs, "release")
	if release == nil {
		t.Fatal("the CI workflow has no \"release\" job")
	}
	ciEnv := mappingGet(t, release, "env")
	if ciEnv == nil {
		t.Fatal("the CI \"release\" job declares no environment")
	}
	ciGot := mappingGet(t, ciEnv, "GO_VERSION")
	if ciGot == nil {
		t.Fatal("the CI \"release\" job does not pin GO_VERSION")
	}
	if ciGot.Value != want {
		t.Errorf("the CI \"release\" job pins Go %q, want the version the module requires (%q)", ciGot.Value, want)
	}
}

// loadCIWorkflow parses the continuous integration workflow.
func loadCIWorkflow(t *testing.T) *yaml.Node {
	t.Helper()
	path := filepath.Join(repoRoot(t), ciWorkflow)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", ciWorkflow, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", ciWorkflow, err)
	}
	if len(doc.Content) == 0 {
		t.Fatalf("%s is empty", ciWorkflow)
	}
	return doc.Content[0]
}

// loadWorkflow parses the release workflow.
func loadWorkflow(t *testing.T) *yaml.Node {
	t.Helper()
	path := filepath.Join(repoRoot(t), releaseWorkflow)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", releaseWorkflow, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", releaseWorkflow, err)
	}
	if len(doc.Content) == 0 {
		t.Fatalf("%s is empty", releaseWorkflow)
	}
	return doc.Content[0]
}

// mappingGet returns the value node for key in the mapping node, or nil.
func mappingGet(t *testing.T, node *yaml.Node, key string) *yaml.Node {
	t.Helper()
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// mappingKeys lists the keys of a mapping node.
func mappingKeys(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]string, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		out = append(out, node.Content[i].Value)
	}
	return out
}

// jobSteps returns the step nodes of a job.
func jobSteps(t *testing.T, job *yaml.Node) []*yaml.Node {
	t.Helper()
	steps := mappingGet(t, job, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return nil
	}
	return steps.Content
}

// stepName returns the name of a step.
func stepName(step *yaml.Node) string {
	for i := 0; i+1 < len(step.Content); i += 2 {
		if step.Content[i].Value == "name" {
			return step.Content[i+1].Value
		}
	}
	return ""
}

// stepRun returns the run script of a step.
func stepRun(step *yaml.Node) string {
	for i := 0; i+1 < len(step.Content); i += 2 {
		if step.Content[i].Value == "run" {
			return step.Content[i+1].Value
		}
	}
	return ""
}

// nodeMentions reports whether a scalar or sequence node mentions the
// given text.
func nodeMentions(node *yaml.Node, text string) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.ScalarNode {
		return node.Value == text
	}
	for _, item := range node.Content {
		if item.Value == text {
			return true
		}
	}
	return false
}
