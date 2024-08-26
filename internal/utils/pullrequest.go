package utils

import (
	. "github.com/wille/review-app-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PullRequestCreationOptions struct {
	Image             string
	BranchName        string
	DeployedBy        string
	RepositoryURL     string
	PullRequestURL    string
	PullRequestNumber int
}

func PullRequestFor(reviewApp ReviewAppConfig, opts PullRequestCreationOptions) PullRequest {
	name := GetResourceName(reviewApp.Name, opts.BranchName)

	deployments := make(map[string]*DeploymentStatus)
	for _, spec := range reviewApp.Spec.Deployments {
		deployments[spec.Name] = &DeploymentStatus{
			LastActive: metav1.Now(),
			IsActive:   spec.StartOnDeploy || reviewApp.Spec.StartOnDeploy,
		}
	}

	return PullRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Labels:    reviewApp.ObjectMeta.Labels,
			Namespace: reviewApp.Namespace,
		},
		Spec: PullRequestSpec{
			ReviewAppConfigRef: reviewApp.Name,
			ImageName:          opts.Image,
			BranchName:         opts.BranchName,
		},
		Status: PullRequestStatus{
			DeployedBy:        opts.DeployedBy,
			DeployedAt:        metav1.Now(),
			RepositoryURL:     opts.RepositoryURL,
			PullRequestURL:    opts.PullRequestURL,
			PullRequestNumber: opts.PullRequestNumber,
			Deployments:       deployments,
		},
	}

}
