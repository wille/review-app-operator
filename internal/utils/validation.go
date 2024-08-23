package utils

import (
	"strings"

	. "github.com/wille/review-app-operator/api/v1alpha1"
)

// IsSameImageRepo checks if two image names and tags are from the same repository
// gchr.io/test/repo:v1 and gchr.io/test/repo:v2 is the same repository
// gchr.io/test/repo:v1 and gchr.io/test/example:v1 is not
func IsSameImageRepo(i1, i2 string) bool {
	b1, _, _ := strings.Cut(i1, ":")
	b2, _, _ := strings.Cut(i2, ":")

	return b1 == b2
}

// ImageHasDigest checks if the image has a digest
func ImageHasDigest(image string) bool {
	return strings.Contains(image, "@")
}

// IsImageAllowed checks if the image matches the base image set on the deployments in a ReviewAppConfig
func IsImageAllowed(reviewApp ReviewAppConfig, image string) bool {
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
