package utils

import (
	"fmt"
	"regexp"
	"strings"

	. "github.com/wille/rac/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimSpace(s)
	s = regexp.MustCompile("[^a-z0-9\\- ]+").ReplaceAllString(s, "")
	s = regexp.MustCompile(" +").ReplaceAllString(s, "-")
	s = regexp.MustCompile("-+").ReplaceAllString(s, "-")
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

	if err := validation.IsDNS1123Subdomain(s); err != nil {
		return "" //, fmt.Errorf("generated host template '%s' is invalid", err[0])
	}

	return s
}

// GetChildResourceName returns a child resource name for a reviewapp and pull request
func GetChildResourceName(reviewApp *ReviewApp, pr *PullRequest) string {
	return GetResourceName(reviewApp.Name, pr.Spec.BranchName)
}
