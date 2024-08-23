package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/wille/review-app-operator/internal/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	. "github.com/wille/review-app-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func writeFlush(w http.ResponseWriter, s string) {
	w.Write([]byte(s))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// createOrUpdatePullRequest ensures that the desired pull request exists and is up to date
// and streams the status of the deployments to the response writer
func createOrUpdatePullRequest(
	ctx context.Context,
	c client.Client,
	reviewApp *ReviewAppConfig,
	key types.NamespacedName,
	webhook WebhookBody,
	w http.ResponseWriter,
) (*PullRequest, error) {
	desiredPr := PullRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Labels:    reviewApp.Labels,
			Namespace: key.Namespace,
		},
		Spec: PullRequestSpec{
			ReviewAppConfigRef: reviewApp.Name,
			ImageName:          webhook.Image,
			BranchName:         webhook.BranchName,
			// TODO set events and statuses
		},
		Status: PullRequestStatus{
			DeployedBy:        webhook.Sender,
			DeployedAt:        metav1.Now(),
			RepositoryURL:     webhook.RepositoryURL,
			PullRequestURL:    webhook.PullRequestURL,
			PullRequestNumber: webhook.PullRequestNumber,
		},
	}

	for _, deployment := range reviewApp.Spec.Deployments {
		for _, container := range deployment.Template.Spec.Containers {
			// The image repo and name defined in the ReviewAppConfig must match the deployed image
			if container.Name == deployment.TargetContainerName && !utils.IsSameImageRepo(container.Image, webhook.Image) {
				err := fmt.Errorf("The image repository is immutable: \"%s\" cannot be changed to \"%s\"", container.Image, webhook.Image)
				log.Error(err, "The image repository is immutable", "name", key.Name)
				w.WriteHeader(http.StatusForbidden)
				return nil, err
			}
		}
	}

	var existingPr PullRequest

	if err := c.Get(ctx, key, &existingPr); err != nil {
		// Create the PullRequest if it is not found
		if apierrors.IsNotFound(err) {
			if err := c.Create(ctx, &desiredPr); err != nil {
				return nil, err
			}

			w.WriteHeader(http.StatusCreated)
			writeFlush(w, fmt.Sprintf("Created pull request \"%s\" for branch \"%s\"\n", desiredPr.Name, desiredPr.Spec.BranchName))
		} else {
			log.Error(err, "Error getting pull request", "name", key.Name)
			return nil, err
		}
	} else {
		patch := client.MergeFrom(existingPr.DeepCopy())

		existingPr.Spec = desiredPr.Spec

		// Update the PullRequest if it is found
		if err := c.Patch(ctx, &existingPr, patch); err != nil {
			log.Error(err, "Error updating pull request", "name", key.Name)
			return nil, err
		}

		existingPr.Status = desiredPr.Status

		if err := c.Status().Patch(ctx, &existingPr, patch); err != nil {
			log.Error(err, "Failed to update status")
			return nil, err
		}

		w.WriteHeader(http.StatusAccepted)
		writeFlush(w, fmt.Sprintf("Updated pull request for branch \"%s\"\n", desiredPr.Spec.BranchName))
	}

	sharedName := utils.GetChildResourceName(reviewApp, &desiredPr)

	attempts := 0
	for {
		finished := true

		select {
		case <-ctx.Done():
			return &desiredPr, nil
		default:
		}

		for _, deploymentSpec := range reviewApp.Spec.Deployments {
			deploymentName := utils.GetResourceName(sharedName, deploymentSpec.Name)

			var deployment = appsv1.Deployment{}
			if err := c.Get(ctx, types.NamespacedName{
				Name:      deploymentName,
				Namespace: key.Namespace,
			}, &deployment); err != nil {
				if apierrors.IsNotFound(err) {
					writeFlush(w, fmt.Sprintf("Waiting for deployment \"%s\" to be created...\n", deploymentName))
					break
				}

				return nil, err
			}

			status, done, err := utils.GetDeploymentStatus(&deployment)

			if err != nil {
				return &desiredPr, err
			}

			if !done {
				finished = false
				log.Info("Deployment in progress: " + status)
			}

			writeFlush(w, status)
		}

		if finished {
			break
		}

		attempts++

		// TODO configuration option or use DeploymentSpec.ProgressDeadlineSeconds
		// The default value for ProgressDeadlineSeconds is 600
		if attempts > 600 {
			log.Info("Timeout waiting for deployments to be ready")
			http.Error(w, "Timeout waiting for deployments to be ready", http.StatusRequestTimeout)

			return nil, errors.New("Timeout waiting for deployments to be ready")
		}

		time.Sleep(1 * time.Second)
	}

	return &desiredPr, nil
}
