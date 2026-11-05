package utils

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

var (
	// Characters that are not allowed at all in a normalized name.
	reDisallowed = regexp.MustCompile(`[^a-z0-9\- /_.]+`)
	// Separator characters that collapse into a single dash.
	reSeparators = regexp.MustCompile(`[-/_.]+`)
	// Leading characters that are not lowercase letters (DNS1035 / label values).
	reLeading = regexp.MustCompile(`^[^a-z]+`)
	// Trailing characters that are not alphanumeric (DNS1123 / DNS1035).
	reTrailing = regexp.MustCompile(`[^a-z0-9]+$`)
)

// Normalizes a resource name or label value.
// Different Kubernetes resources has different name restrictions,
// - Service names must be a DNS1035 string with no starting digit and a max length of 63
// - Other names must be a DNS1123 subdomain string
// - Label values https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/
// This function ensures that the string conforms to all of these restrictions.
func normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimSpace(s)
	s = reDisallowed.ReplaceAllString(s, "")
	// Collapse any run of separator characters into a single dash
	s = reSeparators.ReplaceAllString(s, "-")

	// Only allow leading a-z to comply with DNS1035 for Service names and label values
	s = reLeading.ReplaceAllString(s, "")

	// Only allow trailing alphanumeric characters to comply with DNS1123 and DNS1035
	s = reTrailing.ReplaceAllString(s, "")

	if len(s) > validation.DNS1123LabelMaxLength {
		s = normalize(s[:validation.DNS1123LabelMaxLength])
	}

	return s
}

// randomIDLength is the number of hex digits in {{.RandomID}}, which is 48 random bits
const randomIDLength = 12

// RandomID returns the random ID of a pull request for hostnames. It is the start of the UID,
// which the API server generates randomly, so it is stable for the life of the PullRequest
func RandomID(pr reviewapps.PullRequest) (string, error) {
	id := strings.ReplaceAll(string(pr.UID), "-", "")
	if len(id) < randomIDLength {
		return "", fmt.Errorf("pull request %s has no UID for {{.RandomID}}", pr.Name)
	}

	return id[:randomIDLength], nil
}

// GetHostnameFromTemplate generates a hostname from a template string.
// Permitted variables are
// - {{.ReviewAppConfig}}
// - {{.BranchName}}
// - {{.DeploymentName}}
// - {{.PullRequestNumber}}
// - {{.RandomID}}
// A template must contain {{.BranchName}} or {{.RandomID}}
func GetHostnameFromTemplate(template string, deploymentName string, pr reviewapps.PullRequest, reviewApp reviewapps.ReviewAppConfig) (string, error) {
	hasRandomID := strings.Contains(template, "{{.RandomID}}")
	if !hasRandomID && !strings.Contains(template, "{{.BranchName}}") {
		return "", fmt.Errorf("template %s does not contain {{.BranchName}} or {{.RandomID}}", template)
	}

	randomID := ""
	if hasRandomID {
		var err error
		if randomID, err = RandomID(pr); err != nil {
			return "", err
		}
	}

	// Uses go template syntax
	render := func(template, branchName string) string {
		s := strings.ReplaceAll(template, "{{.BranchName}}", branchName)
		s = strings.ReplaceAll(s, "{{.ReviewAppConfig}}", normalize(reviewApp.Name))
		s = strings.ReplaceAll(s, "{{.DeploymentName}}", normalize(deploymentName))
		s = strings.ReplaceAll(s, "{{.PullRequestNumber}}", strconv.Itoa(pr.Status.PullRequestNumber))
		s = strings.ReplaceAll(s, "{{.RandomID}}", randomID)
		return s
	}

	branchName := normalize(pr.Spec.BranchName)

	// Shorten a long branch name to fit the label instead of cutting the end of the label,
	// which could cut the random ID. The template variables contain dots, so measure the
	// rendered label
	if hasRandomID {
		label, _, _ := strings.Cut(render(template, branchName), ".")
		if excess := len(label) - validation.DNS1123LabelMaxLength; excess > 0 && strings.Contains(label, branchName) {
			branchName = strings.TrimRight(branchName[:max(len(branchName)-excess, 0)], "-")
		}
	}

	s := render(template, branchName)

	// Split template-x.review.example.com into template-x, review.example.com
	// and ensure that it isn't too long
	label, domain, _ := strings.Cut(s, ".")
	label = strings.TrimSuffix(label, ".")

	if domain == "" {
		return "", fmt.Errorf("no domain in %s", s)
	}

	if len(label) > validation.DNS1123LabelMaxLength {
		if hasRandomID {
			return "", fmt.Errorf("template %s is too long for a DNS label", template)
		}

		// Try to cut the long dns label to a valid length
		label = normalize(label[:validation.DNS1123LabelMaxLength])
	}

	s = fmt.Sprintf("%s.%s", label, domain)

	if err := validation.IsDNS1123Subdomain(s); err != nil {
		return "", fmt.Errorf("generated host template '%s' is invalid %s", s, err[0])
	}

	return s, nil
}

// GetHostnamesFromTemplate returns a list of interpolated hostname templates for a a given deployment, pr and review app
func GetHostnamesFromTemplate(templates []string, deploymentName string, pr reviewapps.PullRequest, reviewApp reviewapps.ReviewAppConfig) ([]string, error) {
	hosts := []string{}

	for _, template := range templates {
		host, err := GetHostnameFromTemplate(template, deploymentName, pr, reviewApp)
		if err != nil {
			return nil, err
		}

		hosts = append(hosts, host)
	}

	return hosts, nil
}

// GetResourceName returns a normalized resource name
func GetResourceName(name ...string) string {
	s := normalize(strings.Join(name, "-"))

	return s
}

// GetChildResourceName returns a child resource name for a reviewapp and pull request
func GetChildResourceName(reviewApp *reviewapps.ReviewAppConfig, pr *reviewapps.PullRequest) string {
	return GetResourceName(reviewApp.Name, pr.Spec.BranchName)
}

func GetDeploymentName(reviewApp *reviewapps.ReviewAppConfig, pr *reviewapps.PullRequest, deploymentName string) string {
	return GetResourceName(reviewApp.Name, deploymentName, pr.Spec.BranchName)
}
