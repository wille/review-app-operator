package webhook

import (
	"context"
	"fmt"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/wille/review-app-operator/internal/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubectl/pkg/polymorphichelpers"

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
	reviewApp *ReviewApp,
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
			ReviewAppRef: reviewApp.Name,
			ImageName:    webhook.Image,
			BranchName:   webhook.BranchName,
			// TODO set events and statuses
		},
		Status: PullRequestStatus{
			DeployedBy:     webhook.Sender,
			DeployedAt:     time.Now().Format(time.RFC3339),
			RepositoryURL:  webhook.RepositoryURL,
			PullRequestURL: webhook.PullRequestURL,
		},
	}

	for _, deployment := range reviewApp.Spec.Deployments {
		for _, container := range deployment.Template.Spec.Containers {
			// The image repo and name defined in the ReviewApp must match the deployed image
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
		bothDone := true

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

			statusViewer := polymorphichelpers.DeploymentStatusViewer{}

			obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&deployment)
			if err != nil {
				log.Error(err, "Error converting deployment to unstructured")
				return &desiredPr, nil
			}

			status, done, err := statusViewer.Status(&unstructured.Unstructured{Object: obj}, 0)
			if err != nil {
				log.Error(err, "Error getting deployment status")
				return &desiredPr, nil
			}
			if !done {
				bothDone = false
			}

			writeFlush(w, status)
		}

		if bothDone {
			break
		}

		attempts++

		// TODO configuration option
		//
		if attempts > 120 {
			log.Info("Timeout waiting for deployments to be ready")
			http.Error(w, "Timeout waiting for deployments to be ready", http.StatusRequestTimeout)
			break
		}

		time.Sleep(1 * time.Second)
	}

	return &desiredPr, nil
}
