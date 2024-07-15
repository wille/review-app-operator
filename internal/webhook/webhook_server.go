package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	williamnuv1alpha1 "github.com/wille/rac/api/v1alpha1"
	"github.com/wille/rac/internal/reviewapp"
	"github.com/wille/rac/internal/utils"

	ctrl "sigs.k8s.io/controller-runtime"
)

var log = ctrl.Log.WithName("webhooks")

type WebhookBody struct {
	// ReviewAppName is the name of the Review App to update
	ReviewAppName string `json:"reviewAppName"`

	// ReviewAppNamespace is the namespace of the Review App to update
	ReviewAppNamespace string `json:"reviewAppNamespace"`

	// RepositoryURL is the repository url, eg https://github.com/wille/review-app-operator
	RepositoryURL string `json:"repositoryUrl"`

	// BranchName is the affected branch
	BranchName string `json:"branchName"`

	// PullRequestURL is the URL to the pull request
	PullRequestURL string `json:"pullRequestUrl"`

	// Image is the image to deploy
	// Only used on POST hooks
	Image string `json:"image"`

	// Merged is if the PR is merged or closed
	// Only used on DELETE hooks
	Merged bool `json:"merged"`

	// Sender is the Github user who initiated the action
	Sender string `json:"sender"`
}

type WebhookServer struct {
	Addr string
	client.Client

	webhookSecret string
}

func (wh WebhookServer) Start(ctx context.Context) error {
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		return fmt.Errorf("WEBHOOK_SECRET is not set!")
	}
	wh.webhookSecret = secret

	handler := http.NewServeMux()

	handler.Handle("/v1", http.HandlerFunc(wh.ServeHTTP))

	return http.ListenAndServe(wh.Addr, handler)
}

func (wh WebhookServer) validateWebhook(body []byte, r *http.Request) error {
	signature := r.Header.Get("x-hub-signature-256")

	_hmac := hmac.New(sha256.New, []byte(wh.webhookSecret))

	if _, err := _hmac.Write(body); err != nil {
		return err
	}

	expectedSignature := "sha256=" + hex.EncodeToString(_hmac.Sum(nil))

	if !hmac.Equal([]byte(signature), []byte(expectedSignature)) {
		fmt.Printf("Invalid signature %s, expected=%s\n", signature, expectedSignature)
		return errors.New("Invalid signature")
	}

	return nil
}

func (wh WebhookServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if err != nil {
		http.Error(w, "Error reading body", http.StatusInternalServerError)
		return
	}

	if err := wh.validateWebhook(body, r); err != nil {
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}

	var webhook WebhookBody
	if err := json.Unmarshal(body, &webhook); err != nil {
		log.Error(err, "Error unmarshalling body")
		http.Error(w, "Error unmarshalling body", http.StatusBadRequest)
		return
	}

	williamnuv1alpha1.AddToScheme(scheme.Scheme)
	c, err := client.New(config.GetConfigOrDie(), client.Options{Scheme: scheme.Scheme})
	reviewApp := williamnuv1alpha1.ReviewApp{}
	if err := c.Get(context.TODO(), types.NamespacedName{
		Name:      webhook.ReviewAppName,
		Namespace: webhook.ReviewAppNamespace,
	}, &reviewApp); err != nil {
		// Refuse to create a PullRequest if there is no valid ReviewApp in reviewAppRef
		if apierrors.IsNotFound(err) {
			log.Error(nil, "Review app not found", "name", webhook.ReviewAppName)
			http.Error(w, "Review app not found", http.StatusNotFound)
			return
		}

		log.Error(err, "Error getting review app")
		http.Error(w, "Error getting review app", http.StatusInternalServerError)
		return
	}

	pullRequestResourceName := utils.GetResourceName(reviewApp.Name, webhook.BranchName)

	switch r.Method {
	case http.MethodDelete:
		log.Info("Delete webhook received", "name", pullRequestResourceName)

		if err := reviewapp.DeletePullRequestByName(types.NamespacedName{
			Name:      pullRequestResourceName,
			Namespace: reviewApp.Namespace,
		}); err != nil {
			if apierrors.IsNotFound(err) {
				log.Info("Pull request not found", "name", pullRequestResourceName)
				http.Error(w, "Pull request not found", http.StatusNotFound)
				return
			}

			log.Error(err, "Error deleting pull request", "name", pullRequestResourceName)
			http.Error(w, "Error deleting pull request", http.StatusInternalServerError)
			return
		}

		log.Info("Pull request deleted", "name", pullRequestResourceName)
		http.Error(w, "Pull request deleted", http.StatusOK)
		return
	case http.MethodPost:
		log.Info("Create webhook received", "webhook", webhook)

		pr, err := reviewapp.CreateOrUpdatePullRequest(&reviewApp, types.NamespacedName{
			Name:      pullRequestResourceName,
			Namespace: reviewApp.Namespace,
		}, williamnuv1alpha1.PullRequestSpec{
			ReviewAppRef: reviewApp.Name,
			ImageName:    webhook.Image,
			BranchName:   webhook.BranchName,
			// TODO set events and statuses
		}, w)
		if err != nil {
			log.Error(err, "Error creating pull request", "name", pullRequestResourceName)
			http.Error(w, "Error creating pull request", http.StatusInternalServerError)
			return
		}

		// Pick the first deployment in the .spec.deployments list
		primaryDeploymentSpec := reviewApp.Spec.Deployments[0]

		// Use the first host template in the .spec.deployments.*.hostTemplates list
		template := primaryDeploymentSpec.HostTemplates[0]
		deploymentUrl, err := utils.GetHostnameFromTemplate(template, primaryDeploymentSpec.Name, *pr, reviewApp)
		if err != nil {
			log.Error(err, "Error creating deployment url from template")
			http.Error(w, "Error creating deployment url from template", http.StatusInternalServerError)
			return
		}

		// Only return the "primary" host URL for now.
		// If you have multiple hostnames for a deployment, like when a container
		// is serving multiple websites and you need to rely on the Host header,
		// the github pull request would be cluttered by one deployment for every host.
		//
		// Since the response is streamed to the Github Action so you can follow progress
		// in real time, we just stream status text and finish with a `Review App URL: <url>`
		// that is parsed by the action
		w.Write([]byte("Review App URL: " + deploymentUrl))
		return
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
}
