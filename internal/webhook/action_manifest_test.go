package webhook

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// actionManifest is the action.yml of the Review App Action, with only the fields that GitHub
// Actions reads, so that a misspelled field fails too
type actionManifest struct {
	Name        string `json:"name"`
	Author      string `json:"author"`
	Description string `json:"description"`
	Branding    struct {
		Icon  string `json:"icon"`
		Color string `json:"color"`
	} `json:"branding"`
	Inputs map[string]struct {
		Description string `json:"description"`
		Required    bool   `json:"required"`
		Default     string `json:"default"`
	} `json:"inputs"`
	Outputs map[string]struct {
		Description string `json:"description"`
		Value       string `json:"value"`
	} `json:"outputs"`
	Runs struct {
		Using string `json:"using"`
		Steps []struct {
			ID    string            `json:"id"`
			Shell string            `json:"shell"`
			Run   string            `json:"run"`
			Env   map[string]string `json:"env"`
		} `json:"steps"`
	} `json:"runs"`
}

// The tests of the action run its script directly, so they don't load action.yml like GitHub
// Actions does. An action.yml that isn't valid YAML fails every workflow that uses the action
func TestActionManifest(t *testing.T) {
	data, err := os.ReadFile("../../action.yml")
	if err != nil {
		t.Fatal(err)
	}

	var manifest actionManifest
	if err := yaml.UnmarshalStrict(data, &manifest); err != nil {
		t.Fatalf("action.yml can't be loaded: %v", err)
	}

	if manifest.Runs.Using != "composite" || len(manifest.Runs.Steps) != 1 {
		t.Fatalf("action.yml should run one composite step, it runs %q with %d steps",
			manifest.Runs.Using, len(manifest.Runs.Steps))
	}
	step := manifest.Runs.Steps[0]
	if step.Shell != "bash" || !strings.Contains(step.Run, `"$GITHUB_ACTION_PATH/action-entrypoint.sh"`) {
		t.Errorf("the step should run action-entrypoint.sh with bash, it runs %q with %q", step.Run, step.Shell)
	}

	// Every input reaches the script as INPUT_<NAME>, like for actions that aren't composite
	for name := range manifest.Inputs {
		env := "INPUT_" + strings.ToUpper(name)
		if want := fmt.Sprintf("${{ inputs.%s }}", name); step.Env[env] != want {
			t.Errorf("input %s should be passed to the script as %s: %s, not %q", name, env, want, step.Env[env])
		}
	}

	// And the script reads no input that the step doesn't pass
	script, err := os.ReadFile("../../action-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for _, env := range regexp.MustCompile(`INPUT_[A-Z_]+`).FindAllString(string(script), -1) {
		if _, ok := step.Env[env]; !ok {
			missing = append(missing, env)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("action-entrypoint.sh reads inputs that action.yml doesn't pass: %v", missing)
	}

	for name, output := range manifest.Outputs {
		if !strings.HasPrefix(output.Value, "${{ steps."+step.ID+".outputs.") {
			t.Errorf("output %s should be an output of the step %s, it is %q", name, step.ID, output.Value)
		}
	}
}
