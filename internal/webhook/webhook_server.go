package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/metrics"
	"github.com/wille/review-app-operator/internal/utils"
)

var log = ctrl.Log.WithName("webhooks")

type WebhookBody struct {
	// ReviewAppConfigName is the name of the Review App to update
	ReviewAppConfigName string `json:"reviewAppName"`

	// ReviewAppConfigNamespace is the namespace of the Review App to update
	ReviewAppConfigNamespace string `json:"reviewAppNamespace"`

	// RepositoryURL is the repository url, eg https://github.com/wille/review-app-operator
	RepositoryURL string `json:"repositoryUrl"`

	// BranchName is the affected branch
	BranchName string `json:"branchName"`

	// PullRequestURL is the URL to the pull request
	PullRequestURL string `json:"pullRequestUrl"`

	PullRequestNumber int `json:"pullRequestNumber"`

	// Image is the image to deploy
	// Only used on POST hooks
	Image string `json:"image"`

	// Merged is if the PR is merged or closed
	// Only used on DELETE hooks
	Merged bool `json:"merged"`

	// Sender is the URL to the Github user who initiated the action
	Sender string `json:"sender"`

	// Method is the HTTP method the webhook is signed for, so that it can't be
	// replayed with another method
	Method string `json:"method"`

	// Timestamp is the unix time the webhook was signed at, so that it can't be
	// replayed later on
	Timestamp int64 `json:"timestamp"`
}

type WebhookServer struct {
	Addr string
	client.Client

	// PodReader lists pods directly from the API server to find why a rollout fails, so that
	// the webhook doesn't cache every pod in the namespaces
	PodReader client.Reader

	// PodLogs reads the logs of crashing containers
	PodLogs corev1client.PodsGetter

	// WebhookSecret signs the deploys of the action. Deploys can't authenticate with it if it is empty
	WebhookSecret string

	// IDTokens verifies the GitHub Actions ID tokens that deploys can authenticate with instead
	// of the webhook secret, for the repositories that ReviewAppConfigs allow. Deploys can't
	// authenticate with ID tokens if it is nil
	IDTokens *IDTokenVerifier

	// Serving reports whether the webhook accepts requests, for its readiness probe
	Serving *utils.Serving

	// ShutdownTimeout is how long the webhook waits for the requests in flight when it stops,
	// like deploys that stream their progress
	ShutdownTimeout time.Duration
}

func (wh WebhookServer) Start(ctx context.Context) error {
	handler := http.NewServeMux()
	handler.Handle("/v1", wh)

	ln, err := net.Listen("tcp", wh.Addr)
	if err != nil {
		return err
	}

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
	err = utils.Serve(ctx, ln, srv, wh.Serving, wh.ShutdownTimeout)
	log.Info("Stopped webhook server")
	return err
}

// maxWebhookBodyBytes is the maximum size of a webhook request body we are
// willing to buffer before authenticating the request.
const maxWebhookBodyBytes = 1 << 20 // 1 MiB

// readHeaderTimeout limits how long clients may take to send the request headers
const readHeaderTimeout = 10 * time.Second

// maxWebhookAge is how long after it was signed a webhook is accepted, allowing for clock skew
const maxWebhookAge = 5 * time.Minute

// validateReplay ensures that a signed webhook is only accepted for the method
// it was signed for, and only shortly after it was signed
func validateReplay(webhook WebhookBody, method string, now time.Time) error {
	if webhook.Method == "" || webhook.Timestamp == 0 {
		return errors.New("the webhook is not signed with a method and timestamp, update the review app action")
	}

	if webhook.Method != method {
		return fmt.Errorf("the webhook is signed for %s", webhook.Method)
	}

	if age := now.Sub(time.Unix(webhook.Timestamp, 0)); age > maxWebhookAge || age < -maxWebhookAge {
		return fmt.Errorf("the webhook was not signed within %s of the server time", maxWebhookAge)
	}

	return nil
}

// authenticate returns the claims of the GitHub Actions ID token that the request is authorized
// with, or nil for a request that is signed with the webhook secret
func (wh WebhookServer) authenticate(body []byte, r *http.Request) (*githubClaims, error) {
	if rawToken, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		if wh.IDTokens == nil {
			return nil, errors.New("deploys can't authenticate with ID tokens, as the webhook has no ID token issuer")
		}

		claims, err := wh.IDTokens.verify(r.Context(), rawToken)
		if err != nil {
			return nil, fmt.Errorf("invalid ID token: %w", err)
		}
		return &claims, nil
	}

	if wh.WebhookSecret == "" {
		return nil, errors.New("deploys can't authenticate with the webhook secret, only with GitHub Actions ID tokens")
	}

	if err := wh.validateWebhook(body, r); err != nil {
		// Don't tell why the signature is invalid
		return nil, errors.New("invalid signature")
	}
	return nil, nil
}

func (wh WebhookServer) validateWebhook(body []byte, r *http.Request) error {
	signature := r.Header.Get("x-hub-signature-256")

	_hmac := hmac.New(sha256.New, []byte(wh.WebhookSecret))

	if _, err := _hmac.Write(body); err != nil {
		return err
	}

	expectedSignature := "sha256=" + hex.EncodeToString(_hmac.Sum(nil))

	if !hmac.Equal([]byte(signature), []byte(expectedSignature)) {
		return errors.New("invalid signature")
	}

	return nil
}

func (wh WebhookServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))

	w.Header().Set("Accept", "application/json")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if err != nil {
		http.Error(w, "Error reading body", http.StatusInternalServerError)
		return
	}

	claims, err := wh.authenticate(body, r)
	if err != nil {
		log.Error(err, "Unauthenticated webhook")
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	var webhook WebhookBody
	if err := json.Unmarshal(body, &webhook); err != nil {
		log.Error(err, "Error unmarshalling body")
		http.Error(w, "Error unmarshalling body", http.StatusBadRequest)
		return
	}

	if err := validateReplay(webhook, r.Method, time.Now()); err != nil {
		log.Error(err, "Rejected webhook", "method", r.Method)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	reviewApp := reviewapps.ReviewAppConfig{}
	if err := wh.Get(context.TODO(), types.NamespacedName{
		Name:      webhook.ReviewAppConfigName,
		Namespace: webhook.ReviewAppConfigNamespace,
	}, &reviewApp); err != nil {
		// Refuse to create a PullRequest if there is no valid ReviewAppConfig in reviewAppRef
		if apierrors.IsNotFound(err) {
			log.Error(nil, "Review app not found", "name", webhook.ReviewAppConfigName)
			http.Error(w, "Review app not found", http.StatusNotFound)
			return
		}

		// For example when the review app namespace is not watched by the operator
		log.Error(err, "Error getting review app")
		http.Error(w, "Error getting review app: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Deploys are only counted for ReviewAppConfigs that exist, so that requests can't add labels
	deployed := func(result string) {
		if r.Method == http.MethodPost {
			metrics.Deploys.WithLabelValues(reviewApp.Name, result).Inc()
		}
	}

	// An ID token only allows deploys of the pull requests of the repositories of the ReviewAppConfig
	if claims != nil {
		if err := authorizeIDToken(&reviewApp, *claims, webhook); err != nil {
			log.Error(err, "Forbidden webhook", "repository", claims.Repository)
			deployed(metrics.DeployRejected)
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}

	pullRequestResourceName := utils.GetResourceName(reviewApp.Name, webhook.BranchName)

	// TODO unify with pullrequest.go
	name := types.NamespacedName{
		Name:      pullRequestResourceName,
		Namespace: reviewApp.Namespace,
	}

	switch r.Method {
	case http.MethodDelete:
		log.Info("Delete webhook received", "name", name)

		if err := deletePullRequestByName(wh.Client, name, webhook.BranchName); err != nil {
			if apierrors.IsNotFound(err) || errors.Is(err, errNameConflict) {
				// Closing a pull request without a review app, like when its first deploy
				// failed or it was already deleted, leaves nothing to do
				log.Info("No review app to delete", "name", name, "reason", err.Error())
				http.Error(w, fmt.Sprintf("No review app to delete for branch \"%s\"", webhook.BranchName), http.StatusOK)
				return
			}

			log.Error(err, "Error deleting pull request", "name", name)
			http.Error(w, "Error deleting pull request", http.StatusInternalServerError)
			return
		}

		log.Info("Pull request deleted", "name", name)
		http.Error(w, "Pull request deleted", http.StatusOK)
		return
	case http.MethodPost:
		log.Info("Create webhook received", "webhook", webhook)
		start := time.Now()

		// The ReviewAppConfig must define at least one deployment with at least
		// one host template before we can deploy and report back a URL.
		if len(reviewApp.Spec.Deployments) == 0 || len(reviewApp.Spec.Deployments[0].HostTemplates) == 0 {
			err := errors.New("ReviewAppConfig has no deployments or host templates configured")
			log.Error(err, "Invalid ReviewAppConfig", "name", reviewApp.Name)
			deployed(metrics.DeployFailed)
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}

		pr, err := createOrUpdatePullRequest(
			r.Context(),
			wh.Client,
			podInspector{reader: wh.PodReader, logs: wh.PodLogs},
			&reviewApp,
			webhook,
			w,
		)
		if err != nil {
			// The status code can't be changed once the deployment progress has started
			// streaming, so clients must check that the response ends with the review app URL
			log.Error(err, "Error deploying", "name", name)
			deployed(metrics.DeployFailed)
			http.Error(w, "Error deploying: "+err.Error(), http.StatusInternalServerError)
			return
		}

		urls, err := utils.GetURLs(reviewApp, *pr)
		if err != nil {
			log.Error(err, "Error creating deployment url from template")
			deployed(metrics.DeployFailed)
			http.Error(w, "Error creating deployment url from template", http.StatusInternalServerError)
			return
		}

		deployed(metrics.DeploySucceeded)
		metrics.DeployDuration.WithLabelValues(reviewApp.Name).Observe(time.Since(start).Seconds())

		// The URL of the review app is the first URL of the first deployment, which GitHub links
		// the deployment of the pull request to
		deploymentUrl := urls[0].URLs[0]
		log.Info("Successfully deployed", "url", deploymentUrl)

		// Since the response is streamed to the Github Action so you can follow progress in real
		// time, the action parses the URLs from the text. It lists every URL of every deployment
		// on the pull request, and older actions only read the URL of the review app on the last line
		for _, deployment := range urls {
			for _, url := range deployment.URLs {
				writeFlush(w, fmt.Sprintf("URL of %s: %s\n", deployment.Deployment, url))
			}
		}
		_, _ = w.Write([]byte("Review App URL: " + deploymentUrl))
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
}
