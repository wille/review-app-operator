package utils

import (
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/wille/review-app-operator/api/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestResourceNames(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Resource Names")
}

var _ = Describe("Resource names", func() {
	validate := func(s, expected string) {
		n := normalize(s)
		Expect(n).To(Equal(expected))
		Expect(validation.IsDNS1035Label(n)).To(HaveLen(0))
		Expect(validation.IsDNS1123Label(n)).To(HaveLen(0))
	}
	It("Generates valid resource names, service names and label values", func() {
		validate("dependabot/npm_and_yarn/mongodb-4.17.0", "dependabot-npm-and-yarn-mongodb-4-17-0")
		validate("test", "test")
		validate("test+-", "test")
		validate("test--\"|test", "test-test")
		validate("test_\"|", "test")
		validate("1test_\"|", "test")
		validate("test-test", "test-test")
		validate(strings.Repeat("a", 100), strings.Repeat("a", validation.DNS1035LabelMaxLength))
		validate("feature/v1å[]", "feature-v1")
	})
})

var _ = Describe("Hostname templates", func() {
	pr := PullRequest{
		Spec: PullRequestSpec{
			BranchName: "feature/v1å[]",
		},
	}
	reviewApp := ReviewAppConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review-sample",
		},
		Spec: ReviewAppConfigSpec{},
	}
	deploymentName := "nginx"

	It("should generate a valid hostname from a template", func() {
		Expect(GetHostnameFromTemplate("{{.BranchName}}.review.example.com", deploymentName, pr, reviewApp)).To(Equal("feature-v1.review.example.com"))
		Expect(GetHostnameFromTemplate("{{.BranchName}}-{{.DeploymentName}}.review.example.com", deploymentName, pr, reviewApp)).To(Equal("feature-v1-nginx.review.example.com"))
		Expect(GetHostnameFromTemplate("{{.ReviewAppConfig}}-{{.BranchName}}-{{.DeploymentName}}.review.example.com", deploymentName, pr, reviewApp)).To(Equal("review-sample-feature-v1-nginx.review.example.com"))

		long, _ := GetHostnameFromTemplate("{{.BranchName}}-"+strings.Repeat("a", validation.DNS1123LabelMaxLength)+".example.com", deploymentName, pr, reviewApp)
		Expect(validation.IsDNS1123Subdomain(long)).To(HaveLen(0))
	})
})
