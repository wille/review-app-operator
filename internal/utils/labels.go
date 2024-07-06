package utils

import (
	"maps"

	racwilliamnuv1alpha1 "github.com/wille/rac/api/v1alpha1"
)

// GetResourceLabels returns the labels for all ReviewApp child resources
func GetResourceLabels(reviewApp *racwilliamnuv1alpha1.ReviewApp, pr racwilliamnuv1alpha1.PullRequest, deploymentName string, includeUserLabels bool) map[string]string {
	instance := GetResourceName(reviewApp.Name, pr.Spec.BranchName)

	if deploymentName != "" {
		instance = GetResourceName(instance, deploymentName)
	}

	labels := map[string]string{
		// TODO Label value
		//     	must be 63 characters or less (can be empty),
		// 		unless empty, must begin and end with an alphanumeric character ([a-z0-9A-Z]),
		// 		could contain dashes (-), underscores (_), dots (.), and alphanumerics between.
		/*
			Labels are key/value pairs. Valid label keys have two segments: an optional prefix and name, separated by a slash (/). The name segment is required and must be 63 characters or less, beginning and ending with an alphanumeric character ([a-z0-9A-Z]) with dashes (-), underscores (_), dots (.), and alphanumerics between. The prefix is optional. If specified, the prefix must be a DNS subdomain: a series of DNS labels separated by dots (.), not longer than 253 characters in total, followed by a slash (/).
		*/
		"app.kubernetes.io/name":     pr.Spec.BranchName,
		"app.kubernetes.io/instance": instance,
		// "app.kubernetes.io/version":    "",
		"app.kubernetes.io/component":  "review-app",
		"app.kubernetes.io/part-of":    reviewApp.Name,
		"app.kubernetes.io/managed-by": "review-app-controller",
	}

	if includeUserLabels {
		maps.Copy(labels, reviewApp.ObjectMeta.Labels)
	}

	return labels
}
