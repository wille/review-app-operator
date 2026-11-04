package downscaler

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is idiomatic for Ginkgo specs
	. "github.com/onsi/gomega"    //nolint:staticcheck // dot-import is idiomatic for Gomega matchers
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/utils"
)

func TestDownscaler(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Downscaler")
}

var _ = Describe("Downscaler", func() {
	reviewApp := func(namespace string, scaleDownAfter time.Duration) *reviewapps.ReviewAppConfig {
		return &reviewapps.ReviewAppConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace},
			Spec: reviewapps.ReviewAppConfigSpec{
				ScaleDownAfter: metav1.Duration{Duration: scaleDownAfter},
				Deployments:    []reviewapps.Deployments{{Name: "web"}},
			},
		}
	}

	// idlePullRequest returns an active PullRequest that has not received traffic for two hours
	idlePullRequest := func(namespace string) *reviewapps.PullRequest {
		return &reviewapps.PullRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "app-feature", Namespace: namespace},
			Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: "app", BranchName: "feature"},
			Status: reviewapps.PullRequestStatus{
				Deployments: map[string]*reviewapps.DeploymentStatus{
					"web": {IsActive: true, LastActive: metav1.NewTime(time.Now().Add(-2 * time.Hour))},
				},
			},
		}
	}

	newClient := func(funcs interceptor.Funcs, objs ...client.Object) client.Client {
		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(reviewapps.AddToScheme(scheme)).To(Succeed())

		return fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(objs...).
			WithStatusSubresource(&reviewapps.PullRequest{}).
			WithIndex(&reviewapps.PullRequest{}, utils.ReviewAppRefField, func(obj client.Object) []string {
				return []string{obj.(*reviewapps.PullRequest).Spec.ReviewAppConfigRef}
			}).
			WithInterceptorFuncs(funcs).
			Build()
	}

	isActive := func(c client.Client, namespace string) bool {
		var pr reviewapps.PullRequest
		Expect(c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "app-feature"}, &pr)).To(Succeed())
		return pr.Status.Deployments["web"].IsActive
	}

	It("uses the ReviewAppConfig in the namespace of the PullRequest", func() {
		// Review apps with the same name, where only the one in "scaled" scales down
		c := newClient(interceptor.Funcs{},
			reviewApp("unscaled", 0), idlePullRequest("unscaled"),
			reviewApp("scaled", time.Hour), idlePullRequest("scaled"),
		)

		Downscaler{Client: c}.run(context.Background())

		Expect(isActive(c, "unscaled")).To(BeTrue())
		Expect(isActive(c, "scaled")).To(BeFalse())
	})

	It("continues with the other review apps when listing the PullRequests of one fails", func() {
		failed := false
		c := newClient(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*reviewapps.PullRequestList); ok && !failed {
					failed = true
					return errors.New("list failed")
				}
				return c.List(ctx, list, opts...)
			},
		},
			reviewApp("a", time.Hour), idlePullRequest("a"),
			reviewApp("b", time.Hour), idlePullRequest("b"),
		)

		Downscaler{Client: c}.run(context.Background())

		// The PullRequest of the review app listed first stays active
		Expect([]bool{isActive(c, "a"), isActive(c, "b")}).To(ConsistOf(true, false))
	})
})
