package utils

import (
	"fmt"
	"regexp"
	"strings"

	. "github.com/wille/rac/api/v1alpha1"
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
	for i, c := range s {
		if isAlpha(c) {
			break
		}

		s = s[i+1:]
	}

	// Only allow trailing alphanumeric characters to comply with DNS1123 and DNS1035
	for i := len(s) - 1; i >= 0; i-- {
		if isAlphanumeric(rune(s[i])) {
			break
		}

		s = s[:i]
	}

	if len(s) > validation.DNS1123LabelMaxLength {
		s = normalize(s[:validation.DNS1123LabelMaxLength])
	}

	return s
}

// GetHostnameFromTemplate generates a hostname from a template string.
// Permitted variables are
// - {{reviewAppName}}
// - {{branchName}}
// - {{deploymentName}}
func GetHostnameFromTemplate(template string, deploymentName string, pr PullRequest, reviewApp ReviewApp) (string, error) {
	s := strings.ReplaceAll(template, "{{branchName}}", pr.Spec.BranchName)
	s = strings.ReplaceAll(s, "{{reviewAppName}}", reviewApp.Name)
	s = strings.ReplaceAll(s, "{{deploymentName}}", deploymentName)
	s = normalize(s)

	if len(s) > validation.DNS1123LabelMaxLength {
		s = normalize(s[:validation.DNS1123LabelMaxLength])
	}

	if len(s+"."+reviewApp.Spec.Domain) > validation.DNS1123SubdomainMaxLength {
		s = normalize(s[:validation.DNS1123SubdomainMaxLength-len(reviewApp.Spec.Domain)-1])
	}

	if err := validation.IsDNS1123Label(s); err != nil {
		return "", fmt.Errorf("generated host template '%s' is invalid: %s", s, err[0])
	}

	s = s + "." + reviewApp.Spec.Domain

	if err := validation.IsDNS1123Subdomain(s); err != nil {
		return "", fmt.Errorf("generated host template '%s' is invalid %s", s, err[0])
	}

	// Service is DNS1035 64 chars no digit start

	return s, nil
}

// GetHostnamesFromTemplate returns a list of interpolated hostname templates for a a given deployment, pr and review app
func GetHostnamesFromTemplate(templates []string, deploymentName string, pr PullRequest, reviewApp ReviewApp) ([]string, error) {
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
func GetChildResourceName(reviewApp *ReviewApp, pr *PullRequest) string {
	return GetResourceName(reviewApp.Name, pr.Spec.BranchName)
}
