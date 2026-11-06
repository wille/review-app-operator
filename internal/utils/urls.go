package utils

import (
	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
)

// DeploymentURLs are the URLs of a deployment of a review app, one for each of its host templates
type DeploymentURLs struct {
	Deployment string
	URLs       []string
}

// GetURLs returns the URLs of the deployments of the review app of a PullRequest, in the order of
// the ReviewAppConfig. The first URL of the first deployment is the URL of the review app
func GetURLs(reviewApp reviewapps.ReviewAppConfig, pr reviewapps.PullRequest) ([]DeploymentURLs, error) {
	urls := make([]DeploymentURLs, 0, len(reviewApp.Spec.Deployments))

	for _, deployment := range reviewApp.Spec.Deployments {
		hostnames, err := GetHostnamesFromTemplate(deployment.HostTemplates, deployment.Name, pr, reviewApp)
		if err != nil {
			return nil, err
		}

		deploymentURLs := DeploymentURLs{Deployment: deployment.Name}
		for _, hostname := range hostnames {
			deploymentURLs.URLs = append(deploymentURLs.URLs, URL(hostname))
		}
		urls = append(urls, deploymentURLs)
	}

	return urls, nil
}

// URL returns the URL of a review app hostname. Review apps are served over HTTPS by the
// ingress in front of the forwarder
func URL(hostname string) string {
	return "https://" + hostname
}
