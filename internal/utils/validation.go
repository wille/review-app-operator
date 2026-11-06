package utils

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
)

// IsSameImageRepo checks if two image names and tags are from the same repository
// gchr.io/test/repo:v1 and gchr.io/test/repo:v2 is the same repository
// gchr.io/test/repo:v1 and gchr.io/test/example:v1 is not
func IsSameImageRepo(i1, i2 string) bool {
	return imageRepository(i1) == imageRepository(i2)
}

// imageRepository returns the image without its tag and digest,
// registry.example.com:5000/repo:v1@sha256:... is registry.example.com:5000/repo
func imageRepository(image string) string {
	repo, _, _ := strings.Cut(image, "@")

	// A colon before the last slash separates the registry port, not the tag
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}

	return repo
}

// ImageHasDigest checks if the image has a digest
func ImageHasDigest(image string) bool {
	return strings.Contains(image, "@")
}

// IsImageAllowed checks if the image matches the base image set on the deployments in a ReviewAppConfig
func IsImageAllowed(reviewApp reviewapps.ReviewAppConfig, image string) bool {
	for _, d := range reviewApp.Spec.Deployments {
		for _, c := range d.Template.Spec.Containers {
			if d.TargetContainerName == c.Name &&
				!IsSameImageRepo(c.Image, image) {
				return false
			}
		}
	}

	return true
}

// ErrTargetContainerNotFound is returned when the target container of a deployment is not in its
// template, so that the image of the pull request would never be deployed
var ErrTargetContainerNotFound = errors.New("targetContainerName is not a container of the template")

// CheckTargetContainers returns an error when a deployment of the ReviewAppConfig doesn't have its
// target container. The CRD can't check it, as the rule would be too costly for the API server
func CheckTargetContainers(reviewApp reviewapps.ReviewAppConfig) error {
	for _, deployment := range reviewApp.Spec.Deployments {
		found := slices.ContainsFunc(deployment.Template.Spec.Containers, func(container corev1.Container) bool {
			return container.Name == deployment.TargetContainerName
		})

		if !found {
			return fmt.Errorf("%w of deployment %q in ReviewAppConfig %s: %q",
				ErrTargetContainerNotFound, deployment.Name, reviewApp.Name, deployment.TargetContainerName)
		}
	}

	return nil
}
