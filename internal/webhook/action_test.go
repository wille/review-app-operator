package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is idiomatic for Ginkgo specs
	. "github.com/onsi/gomega"    //nolint:staticcheck // dot-import is idiomatic for Gomega matchers
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
)

// Runs the GitHub Action script against the webhook, so that the two stay in step
var _ = Describe("Review App Action", func() {
	var (
		c      client.Client
		server *httptest.Server
		dir    string

		// The GitHub API that the action adds checks with
		github      *httptest.Server
		checks      []checkRequest
		checksMutex sync.Mutex
		checkStatus int
	)

	prKey := types.NamespacedName{Namespace: "default", Name: "app-feature-login"}

	BeforeEach(func() {
		for _, tool := range []string{"bash", "curl", "jq", "openssl"} {
			if _, err := exec.LookPath(tool); err != nil {
				Skip(tool + " is not installed")
			}
		}

		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(reviewapps.AddToScheme(scheme)).To(Succeed())

		reviewApp := &reviewapps.ReviewAppConfig{
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

		// The deployment the controller created and rolled out with the new image
		isController := true
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-web-feature-login",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: reviewapps.GroupVersion.String(),
					Kind:       "PullRequest",
					Name:       prKey.Name,
					UID:        "pr",
					Controller: &isController,
				}},
			},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "web", Image: "nginx:2"}},
					},
				},
			},
		}

		c = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(reviewApp, deployment).
			WithStatusSubresource(&reviewapps.PullRequest{}).
			Build()

		server = httptest.NewServer(WebhookServer{Client: c, WebhookSecret: "secret"})
		DeferCleanup(server.Close)

		dir = GinkgoT().TempDir()

		checks = nil
		checkStatus = http.StatusCreated
		github = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			check := checkRequest{Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization")}
			Expect(json.NewDecoder(r.Body).Decode(&check.Body)).To(Succeed())

			checksMutex.Lock()
			defer checksMutex.Unlock()
			checks = append(checks, check)

			w.WriteHeader(checkStatus)
			if checkStatus != http.StatusCreated {
				_, _ = w.Write([]byte(`{"message": "Resource not accessible by integration"}`))
			}
		}))
		DeferCleanup(github.Close)
	})

	// addedChecks returns the checks that the action added
	addedChecks := func() []checkRequest {
		checksMutex.Lock()
		defer checksMutex.Unlock()
		return checks
	}

	// runAction runs the action for a pull request event like GitHub runs it and returns its
	// outputs. env sets more environment variables, like inputs
	runAction := func(action, secret string, env ...string) (string, error) {
		event := filepath.Join(dir, "event.json")
		Expect(os.WriteFile(event, []byte(`{
			"action": "`+action+`",
			"number": 7,
			"repository": {"html_url": "https://github.com/o/r", "full_name": "o/r"},
			"pull_request": {
				"head": {"ref": "feature/login", "sha": "abc123"},
				"html_url": "https://github.com/o/r/pull/7",
				"merged": false
			},
			"sender": {"html_url": "https://github.com/octocat"}
		}`), 0o600)).To(Succeed())

		output := filepath.Join(dir, "github_output")
		Expect(os.WriteFile(output, nil, 0o600)).To(Succeed())

		cmd := exec.Command("bash", "../../action-entrypoint.sh")
		cmd.Env = append(os.Environ(),
			"GITHUB_EVENT_NAME=pull_request",
			"GITHUB_EVENT_PATH="+event,
			"GITHUB_OUTPUT="+output,
			"RUNNER_TEMP="+dir,
			"INPUT_REVIEW_APP_NAME=app",
			"INPUT_REVIEW_APP_NAMESPACE=default",
			"INPUT_WEBHOOK_URL="+server.URL,
			"INPUT_WEBHOOK_SECRET="+secret,
			"INPUT_IMAGE=nginx:2",
			"INPUT_CHECK=true",
			"INPUT_GITHUB_TOKEN=token",
			"GITHUB_API_URL="+github.URL,
			"GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary.md"),
		)
		cmd.Env = append(cmd.Env, env...)
		log, err := cmd.CombinedOutput()
		GinkgoWriter.Printf("%s\n", log)

		outputs, readErr := os.ReadFile(output)
		Expect(readErr).NotTo(HaveOccurred())

		return string(outputs), err
	}

	It("deploys and deletes the review app", func() {
		Expect(runAction("synchronize", "secret")).To(Equal("review_app_url=https://feature-login.example.com\n" +
			`review_app_urls={"web":["https://feature-login.example.com"]}` + "\n"))

		var pr reviewapps.PullRequest
		Expect(c.Get(context.Background(), prKey, &pr)).To(Succeed())
		Expect(pr.Spec.BranchName).To(Equal("feature/login"))
		Expect(pr.Status.PullRequestNumber).To(Equal(7))

		_, err := runAction("closed", "secret")
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(c.Get(context.Background(), prKey, &reviewapps.PullRequest{}))).To(BeTrue())
	})

	It("succeeds to close a pull request that has no review app", func() {
		_, err := runAction("closed", "secret")

		Expect(err).NotTo(HaveOccurred())
		Expect(addedChecks()).To(BeEmpty())
	})

	It("adds a check with the URLs of the review app, and marks it deleted when the pull request closes", func() {
		_, err := runAction("synchronize", "secret")
		Expect(err).NotTo(HaveOccurred())

		Expect(addedChecks()).To(HaveLen(1))
		check := addedChecks()[0]
		Expect(check.Method).To(Equal(http.MethodPost))
		Expect(check.Path).To(Equal("/repos/o/r/check-runs"))
		Expect(check.Authorization).To(Equal("Bearer token"))
		Expect(check.Body).To(Equal(map[string]any{
			"name":       "Review app app",
			"head_sha":   "abc123",
			"status":     "completed",
			"conclusion": "success",
			"output": map[string]any{
				"title":   "https://feature-login.example.com",
				"summary": "**web**\n\n- https://feature-login.example.com",
			},
		}))

		summary, err := os.ReadFile(filepath.Join(dir, "summary.md"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(summary)).To(Equal("### Review app `app`\n\n**web**\n\n- https://feature-login.example.com\n"))

		_, err = runAction("closed", "secret")
		Expect(err).NotTo(HaveOccurred())

		Expect(addedChecks()).To(HaveLen(2))
		Expect(addedChecks()[1].Body).To(HaveKeyWithValue("conclusion", "neutral"))
		Expect(addedChecks()[1].Body).To(HaveKeyWithValue("head_sha", "abc123"))
	})

	It("deploys when the check can't be added", func() {
		checkStatus = http.StatusForbidden

		outputs, err := runAction("synchronize", "secret")

		Expect(err).NotTo(HaveOccurred())
		Expect(outputs).To(HavePrefix("review_app_url=https://feature-login.example.com\n"))
		Expect(addedChecks()).To(HaveLen(1))
	})

	It("adds no check when it is turned off", func() {
		_, err := runAction("synchronize", "secret", "INPUT_CHECK=false")

		Expect(err).NotTo(HaveOccurred())
		Expect(addedChecks()).To(BeEmpty())
	})

	It("fails when the webhook rejects the signature", func() {
		outputs, err := runAction("synchronize", "another secret")

		Expect(err).To(HaveOccurred())
		Expect(outputs).To(BeEmpty())
	})
})

// checkRequest is a request of the action to add a check to the pull request
type checkRequest struct {
	Method        string
	Path          string
	Authorization string
	Body          map[string]any
}
