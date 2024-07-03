package utils

import (
	"strings"

	. "github.com/wille/rac/api/v1alpha1"
)

func GetDeploymentHostname(reviewApp *ReviewApp, pr *PullRequest, deploymentName string) string {
	return GetResourceName(
		pr.Name,
		deploymentName,
	) + reviewApp.Spec.HostnameSuffix

}

func GetResourceName(name ...string) string {
	return strings.Join(name, "-")
	/*
		// Kubernetes resource names has a maximum length of 253 chars
		    // but Service objects can be at most 63 chars in length because need to be valid to DNS labels
		    const resourceName = `review-${reviewAppConfigName}-${normalizedName}`
		        .substring(0, 42)
		        .replace(/-+$/, "");

		    // DNS labels max length is 63 chars
		    const dnsName = normalizedName.substring(0, 32).replace(/-+$/, "");
	*/
}

func GetResourceNameFrom(reviewApp *ReviewApp, pr *PullRequest) string {
	return GetResourceName(reviewApp.Name, pr.Name)
}
