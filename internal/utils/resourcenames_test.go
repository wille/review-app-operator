package utils

import (
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/wille/rac/api/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestResourceNames(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Resource Names")
}

var _ = Describe("Generates valid hostnames", func() {
	pr := PullRequest{
		Spec: PullRequestSpec{
			BranchName: "feature/v1å[]",
		},
	}
	reviewApp := ReviewApp{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review-sample",
		},
		Spec: ReviewAppSpec{
			Domain: "review.example.com",
		},
	}
	deploymentName := "nginx"

	It("should generate a valid hostname from a template", func() {
		Expect(GetHostnameFromTemplate("{{branchName}}", deploymentName, pr, reviewApp)).To(Equal("featurev1.review.example.com"))
		Expect(GetHostnameFromTemplate("{{branchName}}-{{deploymentName}}", deploymentName, pr, reviewApp)).To(Equal("featurev1-nginx.review.example.com"))
		Expect(GetHostnameFromTemplate("{{reviewAppName}}-{{branchName}}-{{deploymentName}}", deploymentName, pr, reviewApp)).To(Equal("review-sample-featurev1-nginx.review.example.com"))
		Expect(GetHostnameFromTemplate(strings.Repeat("a", validation.DNS1123LabelMaxLength+5), deploymentName, pr, reviewApp)).To(HaveLen(validation.DNS1123LabelMaxLength + 1 + len(reviewApp.Spec.Domain)))
	})
})
