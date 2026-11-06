package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func writeFlush(w http.ResponseWriter, s string) {
	_, _ = w.Write([]byte(s))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// createOrUpdatePullRequest ensures that the desired pull request exists and is up to date
// and streams the status of the deployments to the response writer
func createOrUpdatePullRequest(
	ctx context.Context,
	c client.Client,
	pods podInspector,
	reviewApp *reviewapps.ReviewAppConfig,
	webhook WebhookBody,
	w http.ResponseWriter,
) (*reviewapps.PullRequest, error) {
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

	for _, deployment := range reviewApp.Spec.Deployments {
		for _, container := range deployment.Template.Spec.Containers {
			// The image repo and name defined in the ReviewAppConfig must match the deployed image
			if container.Name == deployment.TargetContainerName && !utils.IsSameImageRepo(container.Image, webhook.Image) {
				err := fmt.Errorf("the image repository is immutable: \"%s\" cannot be changed to \"%s\"", container.Image, webhook.Image)
				log.Error(err, "The image repository is immutable")
				w.WriteHeader(http.StatusForbidden)
				return nil, err
			}
		}
	}

	if err := checkNameConflict(ctx, c, reviewApp, &desiredPr); err != nil {
		if errors.Is(err, errNameConflict) {
			log.Error(err, "Review app name conflict")
			w.WriteHeader(http.StatusConflict)
		}
		return nil, err
	}

	// The PullRequest as stored, with the UID that {{.RandomID}} is generated from
	var pr *reviewapps.PullRequest
	var existingPr reviewapps.PullRequest

	if err := c.Get(ctx, key, &existingPr); err != nil {
		// Create the PullRequest if it is not found
		if apierrors.IsNotFound(err) {
			pr = desiredPr.DeepCopy()
			pr.Status = reviewapps.PullRequestStatus{}

			if err := c.Create(ctx, pr); err != nil {
				return nil, err
			}

			// The status is ignored on create, so the deploy information is patched separately.
			// The deployment statuses are initialized by the controller, but the field is
			// required and an empty map leaves any statuses the controller already set intact
			statusPatch := client.MergeFrom(pr.DeepCopy())
			setDeployInfo(&pr.Status, desiredPr.Status)
			pr.Status.Deployments = map[string]*reviewapps.DeploymentStatus{}

			if err := c.Status().Patch(ctx, pr, statusPatch); err != nil {
				log.Error(err, "Failed to update status")
				return nil, err
			}

			w.WriteHeader(http.StatusCreated)
			writeFlush(w, fmt.Sprintf("Created pull request \"%s\" for branch \"%s\"\n", desiredPr.Name, desiredPr.Spec.BranchName))
		} else {
			log.Error(err, "Error getting pull request", "name", key.Name)
			return nil, err
		}
	} else {
		// Patch spec and labels without touching the status subresource so we
		// don't clobber controller-maintained fields such as the hostnames the
		// forwarder's host index depends on.
		specPatch := client.MergeFrom(existingPr.DeepCopy())

		// Keep the overrides of the deployments, which are set on the PullRequest by hand
		existingPr.Labels = desiredPr.Labels
		existingPr.Spec.ImageName = desiredPr.Spec.ImageName
		existingPr.Spec.BranchName = desiredPr.Spec.BranchName
		existingPr.Spec.ReviewAppConfigRef = desiredPr.Spec.ReviewAppConfigRef

		// Update the PullRequest if it is found
		if err := c.Patch(ctx, &existingPr, specPatch); err != nil {
			log.Error(err, "Error updating pull request", "name", key.Name)
			return nil, err
		}

		// Set the deploy information on the existing status, preserving the hostnames and any
		// other status set by the controller. The deploy time starts the deployments that start
		// on deploy
		statusPatch := client.MergeFrom(existingPr.DeepCopy())
		setDeployInfo(&existingPr.Status, desiredPr.Status)

		if existingPr.Status.Deployments == nil {
			existingPr.Status.Deployments = make(map[string]*reviewapps.DeploymentStatus)
		}

		if err := c.Status().Patch(ctx, &existingPr, statusPatch); err != nil {
			log.Error(err, "Failed to update status")
			return nil, err
		}
		pr = &existingPr

		w.WriteHeader(http.StatusAccepted)
		writeFlush(w, fmt.Sprintf("Updated pull request for branch \"%s\"\n", desiredPr.Spec.BranchName))
	}

	return pr, waitForRollout(ctx, c, pods, reviewApp, &desiredPr, w)
}

// waitForRollout streams the status of the deployments of the pull request until they all run
// its image, and returns why they don't as soon as the controller or the new pods fail
func waitForRollout(
	ctx context.Context,
	c client.Client,
	pods podInspector,
	reviewApp *reviewapps.ReviewAppConfig,
	pr *reviewapps.PullRequest,
	w http.ResponseWriter,
) error {
	attempts := 0
	for {
		finished := true

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := controllerError(ctx, c, client.ObjectKeyFromObject(pr)); err != nil {
			return err
		}

		for _, deploymentSpec := range reviewApp.Spec.Deployments {
			deploymentName := utils.GetDeploymentName(reviewApp, pr, deploymentSpec.Name)

			var deployment = appsv1.Deployment{}
			if err := c.Get(ctx, types.NamespacedName{
				Name:      deploymentName,
				Namespace: pr.Namespace,
			}, &deployment); err != nil {
				if apierrors.IsNotFound(err) {
					finished = false
					writeFlush(w, fmt.Sprintf("Waiting for deployment \"%s\" to be created...\n", deploymentName))
					continue
				}

				return err
			}

			// Until the controller has applied the new image the deployment status
			// describes the previous rollout
			if !hasContainerImage(&deployment, deploymentSpec.TargetContainerName, pr.Spec.ImageName) {
				finished = false
				writeFlush(w, fmt.Sprintf("Waiting for deployment \"%s\" to be updated...\n", deploymentName))
				continue
			}

			status, done, err := utils.GetDeploymentStatus(&deployment)

			if err != nil {
				return err
			}

			writeFlush(w, status)

			if !done {
				finished = false
				log.Info("Deployment in progress: " + status)

				failure, err := pods.failure(ctx, &deployment, deploymentSpec.TargetContainerName, pr.Spec.ImageName)
				if err != nil {
					// Keep waiting for the rollout, like before the pods could be inspected
					log.Error(err, "Failed to inspect the pods", "deployment", deploymentName)
				} else if failure != nil {
					if failure.logs != "" {
						writeFlush(w, fmt.Sprintf("Last logs of container %q in pod %s:\n%s\n", failure.container, failure.pod, failure.logs))
					}

					return fmt.Errorf("deployment %q fails to start: %s", deploymentName, failure.reason)
				}
			}
		}

		if finished {
			return nil
		}

		attempts++

		// TODO configuration option or use DeploymentSpec.ProgressDeadlineSeconds
		// The default value for ProgressDeadlineSeconds is 600
		if attempts > 600 {
			log.Info("Timeout waiting for deployments to be ready")

			return errors.New("timeout waiting for deployments to be ready")
		}

		time.Sleep(1 * time.Second)
	}
}

// checkNameConflict returns errNameConflict if the PullRequest or deployment names are used by another
// branch, as branch names are normalized and truncated to different lengths in the resource names
func checkNameConflict(ctx context.Context, c client.Client, reviewApp *reviewapps.ReviewAppConfig, pr *reviewapps.PullRequest) error {
	var existingPr reviewapps.PullRequest
	err := c.Get(ctx, client.ObjectKeyFromObject(pr), &existingPr)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	if err == nil && existingPr.Spec.BranchName != pr.Spec.BranchName {
		return fmt.Errorf("%w: branch \"%s\" uses the name \"%s\"", errNameConflict, existingPr.Spec.BranchName, pr.Name)
	}

	for _, deploymentSpec := range reviewApp.Spec.Deployments {
		deploymentName := utils.GetDeploymentName(reviewApp, pr, deploymentSpec.Name)

		var deployment appsv1.Deployment
		err := c.Get(ctx, types.NamespacedName{Namespace: pr.Namespace, Name: deploymentName}, &deployment)
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		if err != nil {
			continue
		}

		if owner := metav1.GetControllerOf(&deployment); owner == nil || owner.Kind != "PullRequest" || owner.Name != pr.Name {
			return fmt.Errorf("%w: the deployment \"%s\" belongs to another review app", errNameConflict, deploymentName)
		}
	}

	return nil
}

// setDeployInfo sets the information about the latest deploy on status
func setDeployInfo(status *reviewapps.PullRequestStatus, deploy reviewapps.PullRequestStatus) {
	status.DeployedAt = deploy.DeployedAt
	status.DeployedBy = deploy.DeployedBy
	status.RepositoryURL = deploy.RepositoryURL
	status.PullRequestURL = deploy.PullRequestURL
	status.PullRequestNumber = deploy.PullRequestNumber
}

// hasContainerImage reports whether the target container of the deployment runs image.
// A deployment without the target container never gets its image updated by the
// controller, so it is considered up to date.
func hasContainerImage(deployment *appsv1.Deployment, containerName, image string) bool {
	return runsImage(deployment.Spec.Template.Spec.Containers, containerName, image)
}

// runsImage reports whether the target container of a pod spec runs image. Without the target
// container the controller never updates the image, so any image counts
func runsImage(containers []corev1.Container, containerName, image string) bool {
	for _, container := range containers {
		if container.Name == containerName {
			return container.Image == image
		}
	}

	return true
}

// reconcileErrorGracePeriod is how long the controller must have failed to reconcile the
// PullRequest before the deploy fails, as errors like conflicting updates are retried right away
var reconcileErrorGracePeriod = 10 * time.Second

// controllerError returns the error of the controller when it fails to reconcile the current spec
// of the PullRequest, like an invalid host template
func controllerError(ctx context.Context, c client.Client, key types.NamespacedName) error {
	var pr reviewapps.PullRequest
	if err := c.Get(ctx, key, &pr); err != nil {
		return nil
	}

	condition := meta.FindStatusCondition(pr.Status.Conditions, reviewapps.ConditionReconciled)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.ObservedGeneration != pr.Generation ||
		time.Since(condition.LastTransitionTime.Time) < reconcileErrorGracePeriod {
		return nil
	}

	return fmt.Errorf("the review app can't be deployed: %s", condition.Message)
}
