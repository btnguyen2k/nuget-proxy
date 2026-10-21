package releaseplan_test

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

type actionStep struct {
	ID    string            `yaml:"id"`
	Uses  string            `yaml:"uses"`
	If    string            `yaml:"if"`
	Shell string            `yaml:"shell"`
	Run   string            `yaml:"run"`
	Env   map[string]string `yaml:"env"`
	With  map[string]string `yaml:"with"`
}

type actionManifest struct {
	Inputs map[string]struct {
		Required bool   `yaml:"required"`
		Default  string `yaml:"default"`
	} `yaml:"inputs"`
	Outputs map[string]struct {
		Value string `yaml:"value"`
	} `yaml:"outputs"`
	Runs struct {
		Using string       `yaml:"using"`
		Steps []actionStep `yaml:"steps"`
	} `yaml:"runs"`
}

func loadAction(t *testing.T) actionManifest {
	t.Helper()
	data, err := os.ReadFile("action.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var action actionManifest
	if err := yaml.Unmarshal(data, &action); err != nil {
		t.Fatal(err)
	}
	return action
}

func findStep(t *testing.T, action actionManifest, id string) actionStep {
	t.Helper()
	for _, step := range action.Runs.Steps {
		if step.ID == id {
			return step
		}
	}
	t.Fatalf("missing action step %q", id)
	return actionStep{}
}

func expression(value string) string {
	return "${{ " + value + " }}"
}

func valuesFor(action actionManifest, inputs map[string]string) map[string]string {
	values := map[string]string{}
	for name, input := range action.Inputs {
		value, ok := inputs[name]
		if !ok {
			value = input.Default
		}
		values[expression("inputs."+name)] = value
	}
	for _, output := range []string{
		"release_dry_run.outputs.result",
		"release_dry_run.outputs.releaseVersion",
		"release_dry_run.outputs.releaseNotes",
		"pending_plan.outputs.result",
		"pending_plan.outputs.version",
	} {
		values[expression("steps."+output)] = ""
	}
	return values
}

func isolatedGitEnv(t *testing.T, directory string) []string {
	t.Helper()
	config := filepath.Join(directory, "gitconfig")
	if err := os.WriteFile(config, nil, 0600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.ToSlash(config),
		"GIT_TERMINAL_PROMPT=0",
	}
}

func bashPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		git, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		for directory := filepath.Dir(git); ; directory = filepath.Dir(directory) {
			bash := filepath.Join(directory, "bin", "bash.exe")
			if _, err := os.Stat(bash); err == nil {
				return bash
			}
			if filepath.Dir(directory) == directory {
				break
			}
		}
		t.Fatal("Git Bash is required on Windows")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	return bash
}

func readOutputs(t *testing.T, data string) map[string]string {
	t.Helper()
	outputs := map[string]string{}
	lines := strings.Split(strings.TrimSuffix(data, "\n"), "\n")
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		if line == "" {
			continue
		}
		key, delimiter, multiline := strings.Cut(line, "<<")
		var value string
		if multiline {
			start := index + 1
			for index++; index < len(lines) && lines[index] != delimiter; index++ {
			}
			if index == len(lines) {
				t.Fatalf("unterminated output %q", key)
			}
			value = strings.Join(lines[start:index], "\n")
		} else {
			var ok bool
			key, value, ok = strings.Cut(line, "=")
			if !ok {
				t.Fatalf("invalid output line %q", line)
			}
		}
		if _, exists := outputs[key]; exists {
			t.Fatalf("duplicate output %q", key)
		}
		outputs[key] = value
	}
	return outputs
}

func runStep(t *testing.T, step actionStep, values map[string]string, directory, temporary string) (map[string]string, error, string) {
	t.Helper()
	outputFile := filepath.Join(temporary, step.ID+".output")
	if err := os.WriteFile(outputFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	script := step.Run
	if runtime.GOOS == "windows" {
		script = `cat() {
  if [ "$#" -eq 1 ] && [ "$1" = /proc/sys/kernel/random/uuid ]; then
    printf '00000000-0000-4000-8000-000000000001\n'
  else
    command cat "$@"
  fi
}
` + script
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, bashPath(t), "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script)
	command.Dir = directory
	command.Env = append(os.Environ(), isolatedGitEnv(t, temporary)...)
	command.Env = append(command.Env,
		"RUNNER_TEMP="+filepath.ToSlash(temporary),
		"GITHUB_OUTPUT="+filepath.ToSlash(outputFile))
	for name, reference := range step.Env {
		value, ok := values[reference]
		if !ok {
			t.Fatalf("unhandled environment expression %q", reference)
		}
		command.Env = append(command.Env, name+"="+value)
	}
	log, runErr := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("step timed out: %s", log)
	}
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	if runErr != nil && len(data) != 0 {
		t.Fatalf("failed step emitted outputs: %s\n%s", data, log)
	}
	return readOutputs(t, string(data)), runErr, string(log)
}

func TestActionContract(t *testing.T) {
	action := loadAction(t)
	if action.Runs.Using != "composite" || len(action.Runs.Steps) != 6 {
		t.Fatal("unexpected composite action structure")
	}
	for _, name := range []string{"mode", "source-sha", "release-branch", "github-token"} {
		if !action.Inputs[name].Required || action.Inputs[name].Default != "" {
			t.Errorf("input %q must be required without a default", name)
		}
	}
	if action.Inputs["tag-prefix"].Default != "v" {
		t.Fatal("tag prefix default changed")
	}
	for _, name := range []string{"result", "version", "release_notes"} {
		if action.Outputs[name].Value != expression("steps.plan.outputs."+name) {
			t.Errorf("incorrect output mapping for %q", name)
		}
	}
	if len(action.Outputs) != 3 {
		t.Fatal("unexpected action outputs")
	}
	checkout := findStep(t, action, "checkout_source")
	if checkout.Uses != "actions/checkout@v7" || !maps.Equal(checkout.With, map[string]string{
		"ref": expression("inputs.source-sha"), "fetch-depth": "0", "token": expression("inputs.github-token"),
	}) {
		t.Fatal("source checkout must use the pinned SHA with full history")
	}
	node := findStep(t, action, "setup_node")
	if node.If != "inputs.mode == 'new'" || node.Uses != "actions/setup-node@v7" || node.With["node-version"] != "lts/*" {
		t.Fatal("Node must only be installed for a new plan")
	}
	dryRun := findStep(t, action, "release_dry_run")
	if dryRun.If != "inputs.mode == 'new'" ||
		dryRun.Uses != "btnguyen2k/action-semrelease@9a1f9152815b6ae99468911fb885c4d65942d29c" ||
		!maps.Equal(dryRun.With, map[string]string{
			"dry-run": "true", "target-sha": expression("inputs.source-sha"),
			"github-token":      expression("inputs.github-token"),
			"tag-major-release": "false", "tag-minor-release": "false",
			"branches": expression("inputs.release-branch"), "tag-prefix": expression("inputs.tag-prefix"),
			"tag-only": "true",
		}) {
		t.Fatal("dry-run action configuration changed")
	}
	if findStep(t, action, "pending_plan").If != "inputs.mode == 'resume'" {
		t.Fatal("pending plans must only be loaded in resume mode")
	}
	for _, step := range action.Runs.Steps {
		if step.Run != "" && (step.Shell != "bash" ||
			strings.Contains(step.Run, "ACTION_PATH") || strings.Contains(step.Run, "action_path")) {
			t.Fatalf("step %q must remain inline and independent of action files", step.ID)
		}
	}
}

func TestValidateInputs(t *testing.T) {
	action := loadAction(t)
	base := map[string]string{
		"mode": "new", "source-sha": strings.Repeat("a", 40), "release-branch": "release",
		"candidate-ref": "refs/heads/release-pending/v1.2.3", "candidate-sha": strings.Repeat("b", 40),
	}
	for _, test := range []struct {
		name, mode, key, value, failure string
	}{
		{"new", "new", "", "", ""},
		{"resume", "resume", "", "", ""},
		{"bad-mode", "invalid", "", "", "mode must be new or resume"},
		{"missing-source", "new", "source-sha", "", "source-sha must be"},
		{"short-source", "new", "source-sha", "abc", "source-sha must be"},
		{"missing-branch", "new", "release-branch", "", "release-branch must be"},
		{"invalid-branch", "new", "release-branch", "invalid branch", "release-branch must be"},
		{"missing-candidate-ref", "resume", "candidate-ref", "", "candidate-ref must be"},
		{"tag-ref", "resume", "candidate-ref", "refs/tags/v1.2.3", "candidate-ref must be"},
		{"missing-candidate-sha", "resume", "candidate-sha", "", "candidate-sha must be"},
		{"short-candidate-sha", "resume", "candidate-sha", "abc", "candidate-sha must be"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inputs := maps.Clone(base)
			inputs["mode"] = test.mode
			if test.key != "" {
				inputs[test.key] = test.value
			}
			directory := t.TempDir()
			_, err, log := runStep(t, findStep(t, action, "validate"), valuesFor(action, inputs), directory, directory)
			if (err != nil) != (test.failure != "") || !strings.Contains(log, test.failure) {
				t.Fatalf("validation error=%v, want %q:\n%s", err, test.failure, log)
			}
		})
	}
}

func TestNormalizeNewPlan(t *testing.T) {
	action := loadAction(t)
	for _, test := range []struct {
		name, result, version, notes string
	}{
		{"success", "SUCCESS", "1.2.3", "First line\nEOF\nLiteral $HOME and $(printf unsafe)\ncaf\u00e9\n"},
		{"skipped", "SKIPPED", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := valuesFor(action, map[string]string{"mode": "new"})
			values[expression("steps.release_dry_run.outputs.result")] = test.result
			values[expression("steps.release_dry_run.outputs.releaseVersion")] = test.version
			values[expression("steps.release_dry_run.outputs.releaseNotes")] = test.notes
			directory := t.TempDir()
			outputs, err, log := runStep(t, findStep(t, action, "plan"), values, directory, directory)
			if err != nil {
				t.Fatalf("normalize: %v\n%s", err, log)
			}
			want := map[string]string{"result": test.result, "version": test.version, "release_notes": test.notes}
			if !maps.Equal(outputs, want) {
				t.Fatalf("outputs=%q, want %q", outputs, want)
			}
		})
	}
}

type pendingFixture struct {
	directory, source, candidate, ref string
	env                               []string
}

func (fixture pendingFixture) git(t *testing.T, arguments ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = fixture.directory
	command.Env = append(os.Environ(), fixture.env...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func newPendingFixture(t *testing.T, missing string) pendingFixture {
	t.Helper()
	root := t.TempDir()
	fixture := pendingFixture{directory: filepath.Join(root, "work"), ref: "refs/heads/release-pending/v1.2.3"}
	if err := os.Mkdir(fixture.directory, 0700); err != nil {
		t.Fatal(err)
	}
	fixture.env = isolatedGitEnv(t, root)
	fixture.git(t, "init", "--quiet", "--bare", filepath.Join(root, "origin.git"))
	fixture.git(t, "init", "--quiet", "--initial-branch=release")
	fixture.git(t, "config", "user.name", "Release plan test")
	fixture.git(t, "config", "user.email", "release-plan@example.invalid")
	fixture.git(t, "config", "core.autocrlf", "false")
	fixture.git(t, "remote", "add", "origin", filepath.Join(root, "origin.git"))
	if err := os.WriteFile(filepath.Join(fixture.directory, "source.txt"), []byte("Source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixture.git(t, "add", "source.txt")
	fixture.git(t, "commit", "--quiet", "-m", "Release source")
	fixture.source = fixture.git(t, "rev-parse", "HEAD")
	fixture.git(t, "push", "--quiet", "origin", "HEAD:refs/heads/release")
	fixture.git(t, "checkout", "--quiet", "-b", "release-pending/v1.2.3")
	pending := filepath.Join(fixture.directory, ".semrelease", "pending")
	if err := os.MkdirAll(pending, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"target-sha": fixture.source + "\n", "version": "1.2.3\r\n",
		"release-notes.md": "Saved notes\nEOF\nLiteral $HOME\n",
	} {
		if name != missing {
			if err := os.WriteFile(filepath.Join(pending, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	fixture.git(t, "add", ".semrelease/pending")
	fixture.git(t, "commit", "--quiet", "-m", "Record pending release")
	fixture.candidate = fixture.git(t, "rev-parse", "HEAD")
	fixture.git(t, "push", "--quiet", "origin", "HEAD:"+fixture.ref)
	fixture.git(t, "checkout", "--quiet", "--detach", fixture.source)
	return fixture
}

func TestResumePlanFromSourcePredatingAction(t *testing.T) {
	action := loadAction(t)
	fixture := newPendingFixture(t, "")
	fixture.git(t, "checkout", "--quiet", "-b", "workflow")
	actionPath := filepath.Join(fixture.directory, ".github", "actions", "release-plan", "action.yaml")
	if err := os.MkdirAll(filepath.Dir(actionPath), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("action.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(actionPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	fixture.git(t, "add", ".github/actions/release-plan/action.yaml")
	fixture.git(t, "commit", "--quiet", "-m", "Add planning action")
	fixture.git(t, "checkout", "--quiet", "--detach", fixture.source)
	if _, err := os.Stat(actionPath); !os.IsNotExist(err) {
		t.Fatalf("old source unexpectedly contains the action: %v", err)
	}
	values := valuesFor(action, map[string]string{
		"mode": "resume", "source-sha": fixture.source, "release-branch": "release",
		"candidate-ref": fixture.ref, "candidate-sha": fixture.candidate,
	})
	temporary := t.TempDir()
	outputs, err, log := runStep(t, findStep(t, action, "pending_plan"), values, fixture.directory, temporary)
	if err != nil {
		t.Fatalf("load pending plan: %v\n%s", err, log)
	}
	if !maps.Equal(outputs, map[string]string{"result": "SUCCESS", "version": "1.2.3"}) {
		t.Fatalf("unexpected pending outputs: %q", outputs)
	}
	values[expression("steps.pending_plan.outputs.result")] = outputs["result"]
	values[expression("steps.pending_plan.outputs.version")] = outputs["version"]
	outputs, err, log = runStep(t, findStep(t, action, "plan"), values, fixture.directory, temporary)
	if err != nil {
		t.Fatalf("normalize pending plan: %v\n%s", err, log)
	}
	want := map[string]string{"result": "SUCCESS", "version": "1.2.3", "release_notes": "Saved notes\nEOF\nLiteral $HOME"}
	if !maps.Equal(outputs, want) {
		t.Fatalf("outputs=%q, want %q", outputs, want)
	}
}

func TestPendingPlanFailures(t *testing.T) {
	action := loadAction(t)
	for _, test := range []struct {
		name, missing, failure string
		moved                  bool
	}{
		{"changed-candidate", "", "The pending release candidate changed", true},
		{"missing-version", "version", ".semrelease/pending/version", false},
		{"missing-notes", "release-notes.md", ".semrelease/pending/release-notes.md", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPendingFixture(t, test.missing)
			if test.moved {
				fixture.git(t, "checkout", "--quiet", "--detach", fixture.candidate)
				fixture.git(t, "commit", "--quiet", "--allow-empty", "-m", "Moved candidate")
				fixture.git(t, "push", "--quiet", "origin", "HEAD:"+fixture.ref)
				fixture.git(t, "checkout", "--quiet", "--detach", fixture.source)
			}
			values := valuesFor(action, map[string]string{
				"mode": "resume", "candidate-ref": fixture.ref, "candidate-sha": fixture.candidate,
			})
			_, err, log := runStep(t, findStep(t, action, "pending_plan"), values, fixture.directory, t.TempDir())
			if err == nil || !strings.Contains(log, test.failure) {
				t.Fatalf("error=%v, want %q:\n%s", err, test.failure, log)
			}
		})
	}
}
