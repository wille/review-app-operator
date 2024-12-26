package utils

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	. "github.com/wille/review-app-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func isAlpha(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isAlphanumeric(c rune) bool {
	return isAlpha(c) || (c >= '0' && c <= '9')
}

// Normalizes a resource name or label value.
// Different Kubernetes resources has different name restrictions,
// - Service names must be a DNS1035 string with no starting digit and a max length of 63
// - Other names must be a DNS1123 subdomain string
// - Label values https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/
// This function ensures that the string conforms to all of these restrictions.
func normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimSpace(s)
	s = regexp.MustCompile("[^a-z0-9\\- /_.]+").ReplaceAllString(s, "")
	s = regexp.MustCompile("/_.").ReplaceAllString(s, "-")
	s = regexp.MustCompile("[-/_.]+").ReplaceAllString(s, "-")

	// Only allow leading a-z to comply with DNS1035 for Service names and label values
	s = regexp.MustCompile("^[^a-z]+").ReplaceAllString(s, "")

	// Only allow trailing alphanumeric characters to comply with DNS1123 and DNS1035
	s = regexp.MustCompile("[^a-z0-9]+$").ReplaceAllString(s, "")

	if len(s) > validation.DNS1123LabelMaxLength {
		s = normalize(s[:validation.DNS1123LabelMaxLength])
	}

	return s
}

// GetHostnameFromTemplate generates a hostname from a template string.
// Permitted variables are
// - {{.ReviewAppConfig}}
// - {{.BranchName}}
// - {{.DeploymentName}}
func GetHostnameFromTemplate(template string, deploymentName string, pr PullRequest, reviewApp ReviewAppConfig) (string, error) {
	if !strings.Contains(template, "{{.BranchName}}") {
		return "", fmt.Errorf("Template %s does not contain {{.BranchName}}", template)
	}

	// Uses go template syntax
	s := strings.ReplaceAll(template, "{{.BranchName}}", normalize(pr.Spec.BranchName))
	s = strings.ReplaceAll(s, "{{.ReviewAppConfig}}", normalize(reviewApp.Name))
	s = strings.ReplaceAll(s, "{{.DeploymentName}}", normalize(deploymentName))
	s = strings.ReplaceAll(s, "{{.PullRequestNumber}}", strconv.Itoa(pr.Status.PullRequestNumber))

	// Split template-x.review.example.com into template-x, review.example.com
	// and ensure that it isn't too long
	label, domain, _ := strings.Cut(s, ".")
	label = strings.TrimSuffix(label, ".")

	if domain == "" {
		return "", fmt.Errorf("No domain in %s", s)
	}

	// Try to cut the long dns label to a valid length
	if len(label) > validation.DNS1123LabelMaxLength {
		label = normalize(label[:validation.DNS1123LabelMaxLength])
	}

	s = fmt.Sprintf("%s.%s", label, domain)

	if err := validation.IsDNS1123Subdomain(s); err != nil {
		return "", fmt.Errorf("generated host template '%s' is invalid %s", s, err[0])
	}

	return s, nil
}

// GetHostnamesFromTemplate returns a list of interpolated hostname templates for a a given deployment, pr and review app
func GetHostnamesFromTemplate(templates []string, deploymentName string, pr PullRequest, reviewApp ReviewAppConfig) ([]string, error) {
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
func GetChildResourceName(reviewApp *ReviewAppConfig, pr *PullRequest) string {
	return GetResourceName(reviewApp.Name, pr.Spec.BranchName)
}

func GetDeploymentName(reviewApp *ReviewAppConfig, pr *PullRequest, deploymentName string) string {
	return GetResourceName(reviewApp.Name, deploymentName, pr.Spec.BranchName)
}
