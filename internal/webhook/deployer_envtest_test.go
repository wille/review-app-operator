package webhook

import (
	"context"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is idiomatic for Ginkgo specs
	. "github.com/onsi/gomega"    //nolint:staticcheck // dot-import is idiomatic for Gomega matchers
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/controller"
)

// Runs the webhook against a real API server and the PullRequest controller to cover
// what the fake client does not emulate, like CRD validation and the status subresource
var _ = Describe("Deploy webhook with an API server", Ordered, func() {
	var (
		testEnv *envtest.Environment
		c       client.Client
		ctx     context.Context
		cancel  context.CancelFunc
	)

	BeforeAll(func() {
		logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

		testEnv = &envtest.Environment{
			CRDDirectoryPaths:     []string{"../../config/crd/bases"},
			ErrorIfCRDPathMissing: true,
		}

		cfg, err := testEnv.Start()
		Expect(err).NotTo(HaveOccurred())

		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(reviewapps.AddToScheme(scheme)).To(Succeed())

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: scheme,
			Metrics: metricsserver.Options{
				BindAddress: "0",
			},
			HealthProbeBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred())

		Expect((&controller.PullRequestReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}).SetupWithManager(mgr)).To(Succeed())

		ctx, cancel = context.WithCancel(context.Background())
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(ctx)).To(Succeed())
		}()

		Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())

		// Like the operator, the webhook reads through the manager cache
		c = mgr.GetClient()
	})

	AfterAll(func() {
		cancel()
		Expect(testEnv.Stop()).To(Succeed())
	})

	It("persists the deploy information and renders the pull request number in hostnames", func() {
		reviewApp := &reviewapps.ReviewAppConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec: reviewapps.ReviewAppConfigSpec{
				Deployments: []reviewapps.Deployments{{
					Name:                "web",
					TargetContainerName: "web",
					TargetContainerPort: 80,
					HostTemplates:       []string{"pr-{{.PullRequestNumber}}-{{.BranchName}}.example.com"},
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "web", Image: "nginx:1"}},
						},
					},
				}},
			},
		}
		Expect(c.Create(ctx, reviewApp)).To(Succeed())

		deploy := func(sender string) {
			// envtest runs no deployment controller so the rollout never finishes,
			// but the PullRequest is written before the webhook waits for it
			deployCtx, deployCancel := context.WithTimeout(ctx, time.Second)
			defer deployCancel()

			_, err := createOrUpdatePullRequest(deployCtx, c, podInspector{}, reviewApp, WebhookBody{
				BranchName:        "feature",
				Image:             "nginx:2",
				PullRequestNumber: 42,
				Sender:            sender,
			}, 0, httptest.NewRecorder())
			Expect(err).To(MatchError(context.DeadlineExceeded))
		}

		expectStatus := func(sender string) {
			Eventually(func(g Gomega) {
				var pr reviewapps.PullRequest
				g.Expect(c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "app-feature"}, &pr)).To(Succeed())
				g.Expect(pr.Status.PullRequestNumber).To(Equal(42))
				g.Expect(pr.Status.DeployedBy).To(Equal(sender))
				g.Expect(pr.Status.Deployments).To(HaveKey("web"))
				g.Expect(pr.Status.Deployments["web"].Hostnames).To(ConsistOf("pr-42-feature.example.com"))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		}

		By("creating the PullRequest")
		deploy("https://github.com/alice")
		expectStatus("https://github.com/alice")

		By("updating the PullRequest")
		deploy("https://github.com/bob")
		expectStatus("https://github.com/bob")
	})

	It("fails the deploy with the error of the controller", func() {
		DeferCleanup(func(gracePeriod time.Duration) { reconcileErrorGracePeriod = gracePeriod }, reconcileErrorGracePeriod)
		reconcileErrorGracePeriod = 0

		// Valid for the CRD, but too long for a DNS label with the random ID
		reviewApp := &reviewapps.ReviewAppConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "failing", Namespace: "default"},
			Spec: reviewapps.ReviewAppConfigSpec{
				Deployments: []reviewapps.Deployments{{
					Name:                "web",
					TargetContainerName: "web",
					TargetContainerPort: 80,
					HostTemplates:       []string{strings.Repeat("a", 60) + "-{{.RandomID}}.example.com"},
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "web", Image: "nginx:1"}},
						},
					},
				}},
			},
		}
		Expect(c.Create(ctx, reviewApp)).To(Succeed())

		deployCtx, deployCancel := context.WithTimeout(ctx, 10*time.Second)
		defer deployCancel()

		_, err := createOrUpdatePullRequest(deployCtx, c, podInspector{}, reviewApp, WebhookBody{
			BranchName: "feature",
			Image:      "nginx:2",
		}, 0, httptest.NewRecorder())
		Expect(err).To(MatchError(ContainSubstring("the review app can't be deployed: invalid host template")))
	})
})
