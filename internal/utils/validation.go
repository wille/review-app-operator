package utils

import (
	"strings"

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
