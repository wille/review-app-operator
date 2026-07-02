package utils

import (
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is idiomatic for Ginkgo specs
	. "github.com/onsi/gomega"    //nolint:staticcheck // dot-import is idiomatic for Gomega matchers
	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"

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
		Expect(validation.IsDNS1035Label(n)).To(BeEmpty())
		Expect(validation.IsDNS1123Label(n)).To(BeEmpty())
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
		validate("86954rgpd_tillo-api-upgrade", "rgpd-tillo-api-upgrade")
	})

	It("Generates a valid deployment name", func() {
		rac := func(n string) *reviewapps.ReviewAppConfig {
			return &reviewapps.ReviewAppConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: n,
				},
			}
		}

		pr := func(n string) *reviewapps.PullRequest {
			return &reviewapps.PullRequest{
				Spec: reviewapps.PullRequestSpec{
					BranchName: n,
				},
			}
		}
		Expect(GetDeploymentName(rac("reviewapp"), pr("branchname"), "deployment")).
			To(Equal("reviewapp-deployment-branchname"))

		Expect(GetDeploymentName(rac("reviewapp"), pr("superlongbranchname"+strings.Repeat("a", 100)), "staging")).
			To(Equal("reviewapp-staging-superlongbranchnameaaaaaaaaaaaaaaaaaaaaaaaaaa"))

		Expect(GetDeploymentName(rac("rac"), pr("pr"), "")).To(Equal("rac-pr"))
	})
})

var _ = Describe("Hostname templates", func() {
	pr := reviewapps.PullRequest{
		Spec: reviewapps.PullRequestSpec{
			BranchName: "feature/v1å[]",
		},
	}
	reviewApp := reviewapps.ReviewAppConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review-sample",
		},
		Spec: reviewapps.ReviewAppConfigSpec{},
	}
	deploymentName := "nginx"

	It("should generate a valid hostname from a template", func() {
		Expect(GetHostnameFromTemplate("{{.BranchName}}.review.example.com", deploymentName, pr, reviewApp)).To(Equal("feature-v1.review.example.com"))
		Expect(GetHostnameFromTemplate("{{.BranchName}}-{{.DeploymentName}}.review.example.com", deploymentName, pr, reviewApp)).To(Equal("feature-v1-nginx.review.example.com"))
		Expect(GetHostnameFromTemplate("{{.ReviewAppConfig}}-{{.BranchName}}-{{.DeploymentName}}.review.example.com", deploymentName, pr, reviewApp)).To(Equal("review-sample-feature-v1-nginx.review.example.com"))

		long, _ := GetHostnameFromTemplate("{{.BranchName}}-"+strings.Repeat("a", validation.DNS1123LabelMaxLength)+".example.com", deploymentName, pr, reviewApp)
		Expect(validation.IsDNS1123Subdomain(long)).To(BeEmpty())
	})
})
