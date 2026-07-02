package utils

import (
	"maps"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MatchingLabels selector to select resources managed by the review-app-controller
var MatchingLabels = client.MatchingLabels{"app.kubernetes.io/managed-by": "review-app-controller"}

// HostIndexFieldName is the field indexes on Services to store the hostnames they are active for
const HostIndexFieldName = ".hosts"

// ReviewAppRefField is the field index on PullRequest resources keyed by
// .spec.reviewAppRef, used to look up all PullRequests belonging to a
// ReviewAppConfig. It is the single source of truth for that relationship
// (set at creation time), so it works before the controller takes ownership.
const ReviewAppRefField = ".spec.reviewAppRef"

// GetSelectorLabels returns selector labels to be used with pod selectors in Services and Deployments
func GetSelectorLabels(reviewApp *reviewapps.ReviewAppConfig, pr reviewapps.PullRequest, deploymentName string) map[string]string {
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
func GetResourceLabels(reviewApp *reviewapps.ReviewAppConfig, pr reviewapps.PullRequest, deploymentName string) map[string]string {
	labels := GetSelectorLabels(reviewApp, pr, deploymentName)
	maps.Copy(labels, reviewApp.Labels)

	return labels
}
