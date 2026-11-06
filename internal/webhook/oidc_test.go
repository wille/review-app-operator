package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is idiomatic for Ginkgo specs
	. "github.com/onsi/gomega"    //nolint:staticcheck // dot-import is idiomatic for Gomega matchers
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/metrics"
)

// fakeIssuer issues ID tokens like GitHub Actions, and serves the keys to verify them with
type fakeIssuer struct {
	URL string
	key *rsa.PrivateKey
}

// newFakeIssuer starts an issuer that is stopped after the spec
func newFakeIssuer() *fakeIssuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	Expect(err).NotTo(HaveOccurred())

	issuer := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer.URL,
			"jwks_uri":                              issuer.URL + "/.well-known/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/.well-known/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "key", Algorithm: string(jose.RS256), Use: "sig"},
		}})
	})

	server := httptest.NewServer(mux)
	DeferCleanup(server.Close)
	issuer.URL = server.URL

	return issuer
}

// sign returns an ID token for a pull request workflow, with claims that change the defaults
func (i *fakeIssuer) sign(claims map[string]any) string {
	return signToken(i.key, i.idTokenClaims(claims))
}

// idTokenClaims are the claims of an ID token of the pull request workflow of octo-org/app
// for pull request 42 of the feature branch, changed by claims
func (i *fakeIssuer) idTokenClaims(claims map[string]any) map[string]any {
	now := time.Now()
	defaults := map[string]any{
		"iss":        i.URL,
		"aud":        "review-app-operator",
		"sub":        "repo:octo-org/app:pull_request",
		"iat":        now.Unix(),
		"nbf":        now.Unix(),
		"exp":        now.Add(5 * time.Minute).Unix(),
		"repository": "octo-org/app",
		"event_name": "pull_request",
		"head_ref":   "feature",
		"ref":        "refs/pull/42/merge",
	}
	maps.Copy(defaults, claims)
	return defaults
}

// signToken signs claims with key into a JWT
func signToken(key *rsa.PrivateKey, claims map[string]any) string {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "key", Algorithm: string(jose.RS256)}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	Expect(err).NotTo(HaveOccurred())

	payload, err := json.Marshal(claims)
	Expect(err).NotTo(HaveOccurred())

	signed, err := signer.Sign(payload)
	Expect(err).NotTo(HaveOccurred())

	token, err := signed.CompactSerialize()
	Expect(err).NotTo(HaveOccurred())
	return token
}

var _ = Describe("Deploy webhook with GitHub Actions ID tokens", func() {
	var (
		c      client.Client
		issuer *fakeIssuer
		server WebhookServer
	)

	BeforeEach(func() {
		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(reviewapps.AddToScheme(scheme)).To(Succeed())

		reviewApp := &reviewapps.ReviewAppConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec: reviewapps.ReviewAppConfigSpec{
				Repositories: []string{"octo-org/other", "Octo-Org/App"},
				Deployments: []reviewapps.Deployments{{
					Name:                "web",
					TargetContainerName: "web",
					TargetContainerPort: 80,
					HostTemplates:       []string{"{{.BranchName}}.example.com"},
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx:1"}}},
					},
				}},
			},
		}

		// The deployment of the review app, rolled out with the deployed image
		isController := true
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-web-feature",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: reviewapps.GroupVersion.String(),
					Kind:       "PullRequest",
					Name:       "app-feature",
					UID:        "app-feature",
					Controller: &isController,
				}},
			},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx:2"}}},
			}},
		}

		c = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(reviewApp, deployment).
			WithStatusSubresource(&reviewapps.PullRequest{}).
			Build()

		issuer = newFakeIssuer()
		server = WebhookServer{
			Client:        c,
			PodReader:     c,
			WebhookSecret: "secret",
			IDTokens:      &IDTokenVerifier{Issuer: issuer.URL, Audience: "review-app-operator"},
		}
	})

	// serve sends a webhook for pull request 42 of the feature branch with the ID token
	serve := func(method, token string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]any{
			"reviewAppName":      "app",
			"reviewAppNamespace": "default",
			"branchName":         "feature",
			"pullRequestNumber":  42,
			"image":              "nginx:2",
			"method":             method,
			"timestamp":          time.Now().Unix(),
		})
		Expect(err).NotTo(HaveOccurred())

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req := httptest.NewRequestWithContext(ctx, method, "/v1", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)

		return rec
	}

	It("deploys the pull request of a repository that the ReviewAppConfig allows", func() {
		succeeded := metrics.Deploys.WithLabelValues("app", metrics.DeploySucceeded)
		duration := metrics.DeployDuration.WithLabelValues("app")
		succeededBefore, durationBefore := testutil.ToFloat64(succeeded), histogramCount(duration)

		rec := serve(http.MethodPost, issuer.sign(nil))

		Expect(rec.Code).To(Equal(http.StatusCreated), rec.Body.String())
		Expect(rec.Body.String()).To(HaveSuffix("Review App URL: https://feature.example.com"))
		Expect(testutil.ToFloat64(succeeded) - succeededBefore).To(BeEquivalentTo(1))
		Expect(histogramCount(duration) - durationBefore).To(BeEquivalentTo(1))
	})

	It("deletes the review app of the pull request", func() {
		Expect(serve(http.MethodPost, issuer.sign(nil)).Code).To(Equal(http.StatusCreated))

		rec := serve(http.MethodDelete, issuer.sign(nil))

		Expect(rec.Code).To(Equal(http.StatusOK))
		Expect(rec.Body.String()).To(Equal("Pull request deleted\n"))
	})

	DescribeTable("refuses ID tokens that don't allow the deploy",
		func(claims map[string]any, message string) {
			rec := serve(http.MethodPost, issuer.sign(claims))

			Expect(rec.Code).To(Equal(http.StatusForbidden))
			Expect(rec.Body.String()).To(ContainSubstring(message))
		},
		Entry("from a repository that the ReviewAppConfig doesn't allow", map[string]any{"repository": "octo-org/attacker"},
			"the ReviewAppConfig app does not allow deploys from octo-org/attacker, which can be added to its spec.repositories"),
		Entry("from a workflow that isn't for a pull request", map[string]any{"event_name": "push"},
			"the ID token is from a push workflow"),
		Entry("from another branch", map[string]any{"head_ref": "main"},
			`the ID token is from the branch "main", not "feature"`),
		Entry("from another pull request", map[string]any{"ref": "refs/pull/41/merge"},
			"the ID token is from refs/pull/41/merge, not pull request 42"),
	)

	It("creates no review app for a forbidden ID token", func() {
		rejected := metrics.Deploys.WithLabelValues("app", metrics.DeployRejected)
		rejectedBefore := testutil.ToFloat64(rejected)

		Expect(serve(http.MethodPost, issuer.sign(map[string]any{"repository": "octo-org/attacker"})).Code).To(Equal(http.StatusForbidden))
		Expect(testutil.ToFloat64(rejected) - rejectedBefore).To(BeEquivalentTo(1))

		var prs reviewapps.PullRequestList
		Expect(c.List(context.Background(), &prs)).To(Succeed())
		Expect(prs.Items).To(BeEmpty())
	})

	DescribeTable("rejects invalid ID tokens",
		func(token func() string, message string) {
			rec := serve(http.MethodPost, token())

			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
			Expect(rec.Body.String()).To(ContainSubstring(message))
		},
		Entry("for another audience", func() string {
			return issuer.sign(map[string]any{"aud": "another-app"})
		}, "invalid ID token"),
		Entry("that expired", func() string {
			return issuer.sign(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})
		}, "token is expired"),
		Entry("from another issuer", func() string {
			return issuer.sign(map[string]any{"iss": "https://token.actions.example.com"})
		}, "invalid ID token"),
		Entry("signed with another key", func() string {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			Expect(err).NotTo(HaveOccurred())
			return signToken(key, issuer.idTokenClaims(nil))
		}, "invalid ID token"),
		Entry("that aren't tokens", func() string { return "not-a-token" }, "invalid ID token"),
	)

	It("rejects ID tokens when the webhook has no issuer", func() {
		server.IDTokens = nil

		rec := serve(http.MethodPost, issuer.sign(nil))

		Expect(rec.Code).To(Equal(http.StatusUnauthorized))
		Expect(rec.Body.String()).To(ContainSubstring("deploys can't authenticate with ID tokens"))
	})

	It("rejects deploys signed with the webhook secret when it is turned off", func() {
		server.WebhookSecret = ""

		req := httptest.NewRequest(http.MethodPost, "/v1", strings.NewReader("{}"))
		req.Header.Set("X-Hub-Signature-256", "sha256=")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)

		Expect(rec.Code).To(Equal(http.StatusUnauthorized))
		Expect(rec.Body.String()).To(ContainSubstring("only with GitHub Actions ID tokens"))
	})

	It("tries to reach the issuer again after it could not", func() {
		server.IDTokens = &IDTokenVerifier{Issuer: "http://127.0.0.1:1", Audience: "review-app-operator"}
		Expect(serve(http.MethodPost, issuer.sign(nil)).Code).To(Equal(http.StatusUnauthorized))

		server.IDTokens.Issuer = issuer.URL

		Expect(serve(http.MethodPost, issuer.sign(nil)).Code).To(Equal(http.StatusCreated))
	})
})

// histogramCount returns how many values a histogram observed
func histogramCount(observer prometheus.Observer) uint64 {
	var metric dto.Metric
	Expect(observer.(prometheus.Histogram).Write(&metric)).To(Succeed())
	return metric.GetHistogram().GetSampleCount()
}
