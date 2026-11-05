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

	It("should require the branch name or the random ID in a template", func() {
		_, err := GetHostnameFromTemplate("{{.ReviewAppConfig}}.review.example.com", deploymentName, pr, reviewApp)
		Expect(err).To(MatchError(ErrInvalidHostTemplate))
	})

	It("should name an unknown variable in the error", func() {
		_, err := GetHostnameFromTemplate("{{.BranchName}}-{{.RandomId}}.review.example.com", deploymentName, pr, reviewApp)
		Expect(err).To(MatchError(ErrInvalidHostTemplate))
		Expect(err).To(MatchError("invalid host template {{.BranchName}}-{{.RandomId}}.review.example.com: unknown variable {{.RandomId}}"))
	})

	Context("with a random ID", func() {
		withUID := pr
		withUID.UID = "3f9c1a7b-2d4e-4a5b-8c6d-7e8f9a0b1c2d"

		It("takes the random ID from the start of the UID", func() {
			Expect(RandomID(withUID)).To(Equal("3f9c1a7b2d4e"))
		})

		It("should generate a hostname with the random ID", func() {
			Expect(GetHostnameFromTemplate("{{.RandomID}}.review.example.com", deploymentName, withUID, reviewApp)).To(Equal("3f9c1a7b2d4e.review.example.com"))
			Expect(GetHostnameFromTemplate("{{.BranchName}}-{{.RandomID}}.review.example.com", deploymentName, withUID, reviewApp)).To(Equal("feature-v1-3f9c1a7b2d4e.review.example.com"))
		})

		It("shortens a long branch name instead of the random ID", func() {
			longBranch := withUID
			longBranch.Spec.BranchName = "dependabot/npm_and_yarn/" + strings.Repeat("a", 100)

			host, err := GetHostnameFromTemplate("frontend-{{.BranchName}}-{{.RandomID}}.review.example.com", deploymentName, longBranch, reviewApp)
			Expect(err).NotTo(HaveOccurred())
			label, _, _ := strings.Cut(host, ".")
			Expect(label).To(HaveLen(validation.DNS1123LabelMaxLength))
			Expect(label).To(HavePrefix("frontend-dependabot-npm-and-yarn-a"))
			Expect(label).To(HaveSuffix("-3f9c1a7b2d4e"))
			Expect(validation.IsDNS1123Subdomain(host)).To(BeEmpty())
		})

		It("fails when the template is too long for the random ID", func() {
			_, err := GetHostnameFromTemplate(strings.Repeat("a", validation.DNS1123LabelMaxLength)+"-{{.RandomID}}.review.example.com", deploymentName, withUID, reviewApp)
			Expect(err).To(HaveOccurred())
		})

		It("fails when the PullRequest has no UID yet", func() {
			_, err := GetHostnameFromTemplate("{{.RandomID}}.review.example.com", deploymentName, pr, reviewApp)
			Expect(err).To(HaveOccurred())
		})
	})
})
