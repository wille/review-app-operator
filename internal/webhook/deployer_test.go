package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is idiomatic for Ginkgo specs
	. "github.com/onsi/gomega"    //nolint:staticcheck // dot-import is idiomatic for Gomega matchers
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
)

func TestWebhook(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Webhook")
}

var _ = Describe("Deploy webhook", func() {
	var (
		reviewApp *reviewapps.ReviewAppConfig
		c         client.Client
	)

	BeforeEach(func() {
		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(reviewapps.AddToScheme(scheme)).To(Succeed())

		reviewApp = &reviewapps.ReviewAppConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec: reviewapps.ReviewAppConfigSpec{
				Deployments: []reviewapps.Deployments{{
					Name:                "web",
					TargetContainerName: "web",
					TargetContainerPort: 80,
					HostTemplates:       []string{"{{.BranchName}}.example.com"},
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "web", Image: "nginx:1"}},
						},
					},
				}},
			},
		}

		c = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(reviewApp).
			WithStatusSubresource(&reviewapps.PullRequest{}).
			Build()
	})

	// rolledOutDeployment returns the review app deployment for the "feature"
	// branch, fully rolled out with image in the target container
	rolledOutDeployment := func(image string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "app-web-feature", Namespace: "default"},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "web", Image: image}},
					},
				},
			},
		}
	}

	deploy := func(timeout time.Duration, image string) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		_, err := createOrUpdatePullRequest(ctx, c, reviewApp, WebhookBody{
			BranchName: "feature",
			Image:      image,
		}, httptest.NewRecorder())

		return err
	}

	It("does not report success before the deployment exists", func() {
		Expect(deploy(1500*time.Millisecond, "nginx:2")).To(MatchError(context.DeadlineExceeded))
	})

	It("does not report success before the deployment runs the new image", func() {
		Expect(c.Create(context.Background(), rolledOutDeployment("nginx:1"))).To(Succeed())

		Expect(deploy(1500*time.Millisecond, "nginx:2")).To(MatchError(context.DeadlineExceeded))
	})

	It("reports success once the deployment has rolled out the new image", func() {
		Expect(c.Create(context.Background(), rolledOutDeployment("nginx:2"))).To(Succeed())

		Expect(deploy(5*time.Second, "nginx:2")).To(Succeed())
	})

	It("ends the streamed response with the error when the rollout fails", func() {
		deployment := rolledOutDeployment("nginx:2")
		deployment.Status.Conditions = []appsv1.DeploymentCondition{{
			Type:   appsv1.DeploymentProgressing,
			Status: corev1.ConditionFalse,
			Reason: "ProgressDeadlineExceeded",
		}}
		Expect(c.Create(context.Background(), deployment)).To(Succeed())

		body := []byte(`{"reviewAppName":"app","reviewAppNamespace":"default","branchName":"feature","image":"nginx:2"}`)
		mac := hmac.New(sha256.New, []byte("secret"))
		mac.Write(body)

		req := httptest.NewRequest(http.MethodPost, "/v1", bytes.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		rec := httptest.NewRecorder()

		WebhookServer{Client: c, WebhookSecret: "secret"}.ServeHTTP(rec, req)

		// The status code was sent before the rollout failed
		Expect(rec.Code).To(Equal(http.StatusCreated))
		lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
		Expect(lines[len(lines)-1]).To(HavePrefix("Error deploying: "))
		Expect(rec.Body.String()).NotTo(ContainSubstring("Review App URL"))
	})
})
