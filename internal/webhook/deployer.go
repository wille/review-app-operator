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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	. "github.com/wille/review-app-operator/api/v1alpha1"
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
	webhook WebhookBody,
	w http.ResponseWriter,
) (*PullRequest, error) {
	desiredPr := utils.PullRequestFor(*reviewApp, utils.PullRequestCreationOptions{
		Image:             webhook.Image,
		BranchName:        webhook.BranchName,
		DeployedBy:        webhook.Sender,
		RepositoryURL:     webhook.RepositoryURL,
		PullRequestURL:    webhook.PullRequestURL,
		PullRequestNumber: webhook.PullRequestNumber,
	})

	key := types.NamespacedName{
		Namespace: desiredPr.Namespace,
		Name:      desiredPr.Name,
	}

	fmt.Println("Desired PR labels:", desiredPr.ObjectMeta.Labels, "___", reviewApp.ObjectMeta.Labels)

	for _, deployment := range reviewApp.Spec.Deployments {
		for _, container := range deployment.Template.Spec.Containers {
			// The image repo and name defined in the ReviewAppConfig must match the deployed image
			if container.Name == deployment.TargetContainerName && !utils.IsSameImageRepo(container.Image, webhook.Image) {
				err := fmt.Errorf("The image repository is immutable: \"%s\" cannot be changed to \"%s\"", container.Image, webhook.Image)
				log.Error(err, "The image repository is immutable")
				w.WriteHeader(http.StatusForbidden)
				return nil, err
			}
		}
	}

	var existingPr PullRequest

	if err := c.Get(ctx, key, &existingPr); err != nil {
		// Create the PullRequest if it is not found
		if apierrors.IsNotFound(err) {
			// for _, deployment := range reviewApp.Spec.Deployments {
			// 	desiredPr.Status.Deployments[deployment.Name] = &DeploymentStatus{
			// 		LastActive: metav1.Now(),
			// 		IsActive:   deployment.StartOnDeploy || reviewApp.Spec.StartOnDeploy,
			// 	}
			// }

			// log.Info("Create debug", "desiredPr", desiredPr.Status.Deployments)

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

		existingPr.ObjectMeta.Labels = desiredPr.ObjectMeta.Labels
		existingPr.Spec = desiredPr.Spec
		existingPr.Status = desiredPr.Status

		// Update the PullRequest if it is found
		if err := c.Patch(ctx, &existingPr, patch); err != nil {
			log.Error(err, "Error updating pull request", "name", key.Name)
			return nil, err
		}

		for _, deployment := range reviewApp.Spec.Deployments {
			existingPr.Status.Deployments[deployment.Name].LastActive = metav1.Now()
			if !existingPr.Status.Deployments[deployment.Name].IsActive {
				existingPr.Status.Deployments[deployment.Name].IsActive = deployment.StartOnDeploy || reviewApp.Spec.StartOnDeploy
			}
		}

		if err := c.Status().Patch(ctx, &existingPr, patch); err != nil {
			log.Error(err, "Failed to update status")
			return nil, err
		}

		w.WriteHeader(http.StatusAccepted)
		writeFlush(w, fmt.Sprintf("Updated pull request for branch \"%s\"\n", desiredPr.Spec.BranchName))
	}

	attempts := 0
	for {
		finished := true

		select {
		case <-ctx.Done():
			return &desiredPr, nil
		default:
		}

		for _, deploymentSpec := range reviewApp.Spec.Deployments {
			deploymentName := utils.GetDeploymentName(reviewApp, &desiredPr, deploymentSpec.Name)

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
