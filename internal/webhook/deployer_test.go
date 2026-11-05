package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is idiomatic for Ginkgo specs
	. "github.com/onsi/gomega"    //nolint:staticcheck // dot-import is idiomatic for Gomega matchers
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

		// Serves "fake logs" as the logs of every container
		podLogs *clientsetfake.Clientset
	)

	BeforeEach(func() {
		podLogs = clientsetfake.NewClientset()

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
			// Like the API server, ignore the status and generate a random UID when creating a PullRequest
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if pr, ok := obj.(*reviewapps.PullRequest); ok {
						pr.Status = reviewapps.PullRequestStatus{}
						pr.UID = uuid.NewUUID()
					}
					return c.Create(ctx, obj, opts...)
				},
			}).
			Build()
	})

	getPullRequest := func() *reviewapps.PullRequest {
		var pr reviewapps.PullRequest
		Expect(c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "app-feature"}, &pr)).To(Succeed())
		return &pr
	}

	// controlledBy returns a controller reference to the PullRequest, like the controller sets on deployments
	controlledBy := func(pullRequestName string) []metav1.OwnerReference {
		isController := true
		return []metav1.OwnerReference{{
			APIVersion: reviewapps.GroupVersion.String(),
			Kind:       "PullRequest",
			Name:       pullRequestName,
			UID:        types.UID(pullRequestName),
			Controller: &isController,
		}}
	}

	// rolledOutDeployment returns the review app deployment for the "feature"
	// branch, fully rolled out with image in the target container
	rolledOutDeployment := func(image string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "app-web-feature", Namespace: "default", OwnerReferences: controlledBy("app-feature")},
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

		_, err := createOrUpdatePullRequest(ctx, c, podInspector{reader: c, logs: podLogs.CoreV1()}, reviewApp, WebhookBody{
			BranchName:        "feature",
			Image:             image,
			PullRequestNumber: 42,
			Sender:            "https://github.com/octocat",
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

	It("persists the deploy information when creating the PullRequest", func() {
		Expect(c.Create(context.Background(), rolledOutDeployment("nginx:2"))).To(Succeed())

		Expect(deploy(5*time.Second, "nginx:2")).To(Succeed())

		pr := getPullRequest()
		Expect(pr.Status.PullRequestNumber).To(Equal(42))
		Expect(pr.Status.DeployedBy).To(Equal("https://github.com/octocat"))
	})

	It("updates the deploy information without clobbering the controller status", func() {
		Expect(c.Create(context.Background(), rolledOutDeployment("nginx:2"))).To(Succeed())

		existing := &reviewapps.PullRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "app-feature", Namespace: "default"},
			Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: "app", BranchName: "feature", ImageName: "nginx:1"},
		}
		Expect(c.Create(context.Background(), existing)).To(Succeed())
		existing.Status = reviewapps.PullRequestStatus{
			PullRequestNumber: 1,
			Deployments: map[string]*reviewapps.DeploymentStatus{
				"web": {Hostnames: []string{"feature.example.com"}},
			},
		}
		Expect(c.Status().Update(context.Background(), existing)).To(Succeed())

		Expect(deploy(5*time.Second, "nginx:2")).To(Succeed())

		pr := getPullRequest()
		Expect(pr.Status.PullRequestNumber).To(Equal(42))
		Expect(pr.Status.DeployedBy).To(Equal("https://github.com/octocat"))
		Expect(pr.Status.Deployments["web"].Hostnames).To(ConsistOf("feature.example.com"))
	})

	// serve sends a webhook request with body, signed with the webhook secret
	serve := func(method string, body map[string]any) *httptest.ResponseRecorder {
		data, err := json.Marshal(body)
		Expect(err).NotTo(HaveOccurred())

		mac := hmac.New(sha256.New, []byte("secret"))
		mac.Write(data)

		// Don't wait for the full rollout timeout when a deployment never rolls out
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req := httptest.NewRequestWithContext(ctx, method, "/v1", bytes.NewReader(data))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		rec := httptest.NewRecorder()

		WebhookServer{Client: c, PodReader: c, PodLogs: podLogs.CoreV1(), WebhookSecret: "secret"}.ServeHTTP(rec, req)

		return rec
	}

	webhookBody := func(signedMethod string, signedAt time.Time) map[string]any {
		return map[string]any{
			"reviewAppName":      "app",
			"reviewAppNamespace": "default",
			"branchName":         "feature",
			"image":              "nginx:2",
			"method":             signedMethod,
			"timestamp":          signedAt.Unix(),
		}
	}

	DescribeTable("rejects signed requests that are replayed",
		func(body map[string]any) {
			Expect(serve(http.MethodDelete, body).Code).To(Equal(http.StatusUnauthorized))
		},
		Entry("with another method", webhookBody(http.MethodPost, time.Now())),
		Entry("long after they were signed", webhookBody(http.MethodDelete, time.Now().Add(-10*time.Minute))),
		Entry("with a timestamp in the future", webhookBody(http.MethodDelete, time.Now().Add(10*time.Minute))),
		Entry("without a method and timestamp", map[string]any{"reviewAppName": "app", "reviewAppNamespace": "default", "branchName": "feature"}),
	)

	It("accepts a signed request for its method shortly after it was signed", func() {
		// There is no PullRequest to delete
		Expect(serve(http.MethodDelete, webhookBody(http.MethodDelete, time.Now().Add(-time.Minute))).Code).To(Equal(http.StatusNotFound))
	})

	It("ends the streamed response with the error when the rollout fails", func() {
		deployment := rolledOutDeployment("nginx:2")
		deployment.Status.Conditions = []appsv1.DeploymentCondition{{
			Type:   appsv1.DeploymentProgressing,
			Status: corev1.ConditionFalse,
			Reason: "ProgressDeadlineExceeded",
		}}
		Expect(c.Create(context.Background(), deployment)).To(Succeed())

		rec := serve(http.MethodPost, webhookBody(http.MethodPost, time.Now()))

		// The status code was sent before the rollout failed
		Expect(rec.Code).To(Equal(http.StatusCreated))
		lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
		Expect(lines[len(lines)-1]).To(HavePrefix("Error deploying: "))
		Expect(rec.Body.String()).NotTo(ContainSubstring("Review App URL"))
	})

	It("reports the review app URL with the random ID of the PullRequest", func() {
		reviewApp.Spec.Deployments[0].HostTemplates = []string{"{{.BranchName}}-{{.RandomID}}.example.com"}
		Expect(c.Update(context.Background(), reviewApp)).To(Succeed())
		Expect(c.Create(context.Background(), rolledOutDeployment("nginx:2"))).To(Succeed())

		rec := serve(http.MethodPost, webhookBody(http.MethodPost, time.Now()))

		pr := getPullRequest()
		Expect(pr.UID).NotTo(BeEmpty())
		id := strings.ReplaceAll(string(pr.UID), "-", "")[:12]
		lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
		Expect(lines[len(lines)-1]).To(Equal("Review App URL: https://feature-" + id + ".example.com"))
	})

	It("does not deploy when the deployment name is used by another review app", func() {
		// Long branch names are truncated to the same deployment name
		// before they are truncated to the same PullRequest name
		deployment := rolledOutDeployment("nginx:1")
		deployment.OwnerReferences = controlledBy("app-feature-other")
		Expect(c.Create(context.Background(), deployment)).To(Succeed())

		rec := serve(http.MethodPost, webhookBody(http.MethodPost, time.Now()))

		Expect(rec.Code).To(Equal(http.StatusConflict))
		Expect(rec.Body.String()).To(ContainSubstring(`"app-web-feature"`))
		err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "app-feature"}, &reviewapps.PullRequest{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the PullRequest should not be created")
	})

	Context("when another branch has the same normalized name", func() {
		// feature/login and feature-login both normalize to app-feature-login
		BeforeEach(func() {
			Expect(c.Create(context.Background(), &reviewapps.PullRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "app-feature-login", Namespace: "default"},
				Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: "app", BranchName: "feature/login", ImageName: "nginx:1"},
			})).To(Succeed())
		})

		branchBody := func(method, branchName string) map[string]any {
			body := webhookBody(method, time.Now())
			body["branchName"] = branchName
			return body
		}

		expectUnchanged := func() {
			var pr reviewapps.PullRequest
			Expect(c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "app-feature-login"}, &pr)).To(Succeed())
			Expect(pr.Spec.BranchName).To(Equal("feature/login"))
			Expect(pr.Spec.ImageName).To(Equal("nginx:1"))
		}

		It("does not deploy over the review app of the other branch", func() {
			rec := serve(http.MethodPost, branchBody(http.MethodPost, "feature-login"))

			Expect(rec.Code).To(Equal(http.StatusConflict))
			Expect(rec.Body.String()).To(ContainSubstring(`"feature/login"`))
			expectUnchanged()
		})

		It("does not delete the review app of the other branch", func() {
			Expect(serve(http.MethodDelete, branchBody(http.MethodDelete, "feature-login")).Code).To(Equal(http.StatusNotFound))
			expectUnchanged()
		})

		It("deletes the review app of its own branch", func() {
			Expect(serve(http.MethodDelete, branchBody(http.MethodDelete, "feature/login")).Code).To(Equal(http.StatusOK))
		})
	})

	Context("when the review app fails to start", func() {
		// rollingOutDeployment returns the review app deployment for the "feature" branch,
		// rolling out image with a pod that isn't available
		rollingOutDeployment := func(image string) *appsv1.Deployment {
			deployment := rolledOutDeployment(image)
			replicas := int32(1)
			deployment.Spec.Replicas = &replicas
			deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "feature"}}
			deployment.Status = appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1}
			return deployment
		}

		pod := func(image string, status corev1.ContainerStatus) *corev1.Pod {
			status.Name = "web"
			return &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "app-web-feature-1", Namespace: "default", Labels: map[string]string{"app": "feature"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: image}}},
				Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{status}},
			}
		}

		crashLooping := func(restarts int32) corev1.ContainerStatus {
			return corev1.ContainerStatus{
				RestartCount:         restarts,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
			}
		}

		lastLine := func(rec *httptest.ResponseRecorder) string {
			lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
			return lines[len(lines)-1]
		}

		It("fails right away when a new pod keeps crashing", func() {
			Expect(c.Create(context.Background(), rollingOutDeployment("nginx:2"))).To(Succeed())
			Expect(c.Create(context.Background(), pod("nginx:2", crashLooping(3)))).To(Succeed())

			start := time.Now()
			rec := serve(http.MethodPost, webhookBody(http.MethodPost, time.Now()))

			Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
			Expect(rec.Body.String()).To(ContainSubstring("Last logs of container \"web\" in pod app-web-feature-1:\nfake logs\n"))
			Expect(lastLine(rec)).To(Equal(`Error deploying: deployment "app-web-feature" fails to start: ` +
				`container "web" in pod app-web-feature-1 is crashing, it restarted 3 times and last exited with code 1 (Error)`))
		})

		It("gives a crashing pod a chance to start", func() {
			Expect(c.Create(context.Background(), rollingOutDeployment("nginx:2"))).To(Succeed())
			Expect(c.Create(context.Background(), pod("nginx:2", crashLooping(1)))).To(Succeed())

			Expect(deploy(1500*time.Millisecond, "nginx:2")).To(MatchError(context.DeadlineExceeded))
		})

		It("ignores the crashing pods of the previous image", func() {
			Expect(c.Create(context.Background(), rollingOutDeployment("nginx:2"))).To(Succeed())
			Expect(c.Create(context.Background(), pod("nginx:1", crashLooping(5)))).To(Succeed())

			Expect(deploy(1500*time.Millisecond, "nginx:2")).To(MatchError(context.DeadlineExceeded))
		})

		It("fails right away when the image can't be pulled", func() {
			Expect(c.Create(context.Background(), rollingOutDeployment("nginx:2"))).To(Succeed())
			Expect(c.Create(context.Background(), pod("nginx:2", corev1.ContainerStatus{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: `Back-off pulling image "nginx:2"`}},
			}))).To(Succeed())

			rec := serve(http.MethodPost, webhookBody(http.MethodPost, time.Now()))

			Expect(lastLine(rec)).To(Equal(`Error deploying: deployment "app-web-feature" fails to start: ` +
				`container "web" in pod app-web-feature-1 can't start: ImagePullBackOff: Back-off pulling image "nginx:2"`))
		})

		// failedPullRequest creates the PullRequest of the "feature" branch, which the controller
		// failed to reconcile at failedAt
		failedPullRequest := func(failedAt time.Time, observedGenerationOffset int64) {
			pr := &reviewapps.PullRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "app-feature", Namespace: "default"},
				Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: "app", BranchName: "feature", ImageName: "nginx:2"},
			}
			Expect(c.Create(context.Background(), pr)).To(Succeed())

			pr.Status = reviewapps.PullRequestStatus{
				Deployments: map[string]*reviewapps.DeploymentStatus{},
				Conditions: []metav1.Condition{{
					Type:               reviewapps.ConditionReconciled,
					Status:             metav1.ConditionFalse,
					Reason:             "InvalidHostTemplate",
					Message:            "invalid host template web-{{.RandomId}}.example.com: unknown variable {{.RandomId}}",
					ObservedGeneration: pr.Generation + observedGenerationOffset,
					LastTransitionTime: metav1.NewTime(failedAt),
				}},
			}
			Expect(c.Status().Update(context.Background(), pr)).To(Succeed())
		}

		It("fails right away when the controller can't deploy the review app", func() {
			failedPullRequest(time.Now().Add(-time.Minute), 0)

			rec := serve(http.MethodPost, webhookBody(http.MethodPost, time.Now()))

			Expect(lastLine(rec)).To(Equal("Error deploying: the review app can't be deployed: " +
				"invalid host template web-{{.RandomId}}.example.com: unknown variable {{.RandomId}}"))
		})

		It("gives the controller a chance to retry an error", func() {
			failedPullRequest(time.Now(), 0)

			Expect(deploy(1500*time.Millisecond, "nginx:2")).To(MatchError(context.DeadlineExceeded))
		})

		It("ignores a controller error for a previous spec of the PullRequest", func() {
			failedPullRequest(time.Now().Add(-time.Minute), 1)

			Expect(deploy(1500*time.Millisecond, "nginx:2")).To(MatchError(context.DeadlineExceeded))
		})
	})
})
