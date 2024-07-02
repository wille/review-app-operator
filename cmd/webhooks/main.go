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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"

	williamnuv1alpha1 "github.com/wille/rac/api/v1alpha1"
	"github.com/wille/rac/internal/reviewapp"
	"github.com/wille/rac/internal/utils"
)

var webhookSecret = "secret"

func validateWebhook(body []byte, r *http.Request) error {
	signature := r.Header.Get("x-hub-signature-256")

	_hmac := hmac.New(sha256.New, []byte(webhookSecret))

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

/*
	{
	    "event": "push",
	    "repository": "owner/project",
	    "commit": "a636b6f0861bbee98039bf3df66ee13d8fbc9c74",
	    "ref": "refs/heads/master",
	    "head": "",
	    "workflow": "Build and deploy",
	    "data": {
	        "weapon": "hammer",
	        "drink": "beer"
	    },
	    "requestID": "74b1912d19cfe780f1fada4b525777fd"
	}
*/
type Webhook struct {
	Ref   string `json:"ref"`
	Image string `json:"image"`
}

func Run() {
	handler := http.NewServeMux()

	log := log.Log.WithName("webhooks")

	handler.Handle("/v1/{reviewApp}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)

		if err != nil {
			http.Error(w, "Error reading body", http.StatusInternalServerError)
			return
		}

		if err := validateWebhook(body, r); err != nil {
			http.Error(w, "Invalid signature", http.StatusUnauthorized)
			return
		}

		reviewAppRef := r.PathValue("reviewApp")

		var webhook Webhook
		if err := json.Unmarshal(body, &webhook); err != nil {
			log.Error(err, "Error unmarshalling body")
			http.Error(w, "Error unmarshalling body", http.StatusBadRequest)
			return
		}

		pullRequest := webhook.Ref

		log := log.WithValues("reviewApp", reviewAppRef, "pullRequest", pullRequest)

		williamnuv1alpha1.AddToScheme(scheme.Scheme)
		c, err := client.New(config.GetConfigOrDie(), client.Options{Scheme: scheme.Scheme})
		reviewApp := williamnuv1alpha1.ReviewApp{}
		if err := c.Get(context.TODO(), types.NamespacedName{Name: reviewAppRef, Namespace: "default"}, &reviewApp); err != nil {
			// Refuse to create a PullRequest if there is no valid ReviewApp in reviewAppRef
			if apierrors.IsNotFound(err) {
				log.Error(err, "unable to find review app", "name", reviewAppRef)
				return
			}

			log.Error(err, "Error getting review app")
			http.Error(w, "Error getting review app", http.StatusInternalServerError)
			return
		}

		pullRequestResourceName := utils.GetResourceName(reviewAppRef, pullRequest)

		switch r.Method {
		case http.MethodDelete:
			log.Info("Delete webhook received")

			if err := reviewapp.DeletePullRequestByName(pullRequestResourceName); err != nil {
				if apierrors.IsNotFound(err) {
					http.Error(w, "Pull request not found", http.StatusNotFound)
					return
				}

				http.Error(w, "Error deleting pull request", http.StatusInternalServerError)
				return
			}

			http.Error(w, "Pull request deleted", http.StatusOK)
			return
		case http.MethodPost:
			log.Info("Create webhook received", "webhook", webhook)

			if err := reviewapp.CreateOrUpdatePullRequest(&reviewApp, pullRequestResourceName, williamnuv1alpha1.PullRequestSpec{
				ReviewAppRef: reviewApp.Name,
				ImageName:    webhook.Image,
			}); err != nil {
				log.Error(err, "Error creating pull request")
				http.Error(w, "Error creating pull request", http.StatusInternalServerError)
				return
			}
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
	}))

	http.ListenAndServe(":8080", handler)
}
