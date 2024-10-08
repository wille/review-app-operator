package utils

import (
	"maps"

	. "github.com/wille/review-app-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MatchingLabels selector to select resources managed by the review-app-controller
var MatchingLabels = client.MatchingLabels{"app.kubernetes.io/managed-by": "review-app-controller"}

// HostIndexFieldName is the field indexes on Services to store the hostnames they are active for
const HostIndexFieldName = ".hosts"

// GetSelectorLabels returns selector labels to be used with pod selectors in Services and Deployments
func GetSelectorLabels(reviewApp *ReviewAppConfig, pr PullRequest, deploymentName string) map[string]string {
	instance := GetResourceName(reviewApp.Name, pr.Spec.BranchName)

	if deploymentName != "" {
		instance = GetResourceName(instance, deploymentName)
	}

	labels := map[string]string{
		"app.kubernetes.io/name":     normalize(pr.Spec.BranchName),
		"app.kubernetes.io/instance": instance,
		// "app.kubernetes.io/version":    "",
		"app.kubernetes.io/component":  "review-app",
		"app.kubernetes.io/part-of":    normalize(reviewApp.Name),
		"app.kubernetes.io/managed-by": "review-app-controller",
	}

	return labels
}

// GetResourceLabels returns the labels for all ReviewAppConfig child resources with all user supplied labels included
func GetResourceLabels(reviewApp *ReviewAppConfig, pr PullRequest, deploymentName string) map[string]string {
	labels := GetSelectorLabels(reviewApp, pr, deploymentName)
	maps.Copy(labels, reviewApp.ObjectMeta.Labels)

	return labels
}
