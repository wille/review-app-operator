package downscaler

import (
	"context"
	"time"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

var log = ctrl.Log.WithName("downscaler")

// Downscaler watches deployments and scales them down if unused for some time
type Downscaler struct {
	client.Client
}

var _ manager.LeaderElectionRunnable = &Downscaler{}

// Only run the downscaler on the lead manager
func (ds Downscaler) NeedLeaderElection() bool {
	return true
}

// Starts the ticker to watch deployments
func (ds Downscaler) Start(ctx context.Context) error {
	log.Info("starting downscaler")

	ticker := time.NewTicker(time.Minute)

	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping downscaler")
			return nil
		case <-ticker.C:
			ds.run(ctx)
		}
	}
}

func (ds Downscaler) run(ctx context.Context) {
	// List all deployments owned by the Review App Operator
	var list reviewapps.ReviewAppConfigList
	if err := ds.List(ctx, &list); err != nil {
		log.Error(err, "Failed to list review apps")
		return
	}

	for _, reviewApp := range list.Items {
		var prs reviewapps.PullRequestList
		if err := ds.List(ctx, &prs,
			client.InNamespace(reviewApp.Namespace),
			client.MatchingFields{utils.ReviewAppRefField: reviewApp.Name},
		); err != nil {
			log.Error(err, "Failed to list pull requests for review app", "reviewApp", reviewApp.Name)
			continue
		}

		for _, pr := range prs.Items {
			if ds.deleteInactive(ctx, reviewApp, pr) {
				continue
			}

			ds.process(reviewApp, pr, ctx)
		}
	}
}

func (ds Downscaler) process(reviewApp reviewapps.ReviewAppConfig, pr reviewapps.PullRequest, ctx context.Context) {
	for deploymentName, status := range pr.Status.Deployments {
		if !status.IsActive {
			continue
		}

		dur := reviewApp.Spec.ScaleDownAfter.Duration

		for _, deploymentSpec := range reviewApp.Spec.Deployments {
			if deploymentSpec.Name == deploymentName && deploymentSpec.ScaleDownAfter.Duration != 0 {
				dur = deploymentSpec.ScaleDownAfter.Duration
			}
		}

		if dur == 0 {
			continue
		}

		// If the latest request was more than TimeoutSeconds ago, scale down the deployment
		if status.LastActive.Add(dur).Before(time.Now()) {
			patch := client.MergeFrom(pr.DeepCopy())
			pr.Status.Deployments[deploymentName].IsActive = false

			// TODO Add event to the deployment
			if err := ds.Client.Status().Patch(ctx, &pr, patch); err != nil {
				log.Error(err, "Failed to downscale deployment", "deployment", pr.Name)
				continue
			}

			log.Info("Scaling down", "name", pr.Name, "lastActive", status.LastActive, "deployment", deploymentName, "scaleDownAfter", dur)
		}
	}
}

// lastActivity is when the review app of a pull request was last created, deployed or visited
func lastActivity(pr reviewapps.PullRequest) time.Time {
	last := pr.CreationTimestamp.Time

	if pr.Status.DeployedAt.After(last) {
		last = pr.Status.DeployedAt.Time
	}

	for _, status := range pr.Status.Deployments {
		if status != nil && status.LastActive.After(last) {
			last = status.LastActive.Time
		}
	}

	return last
}

// +kubebuilder:rbac:groups=reviewapps.william.nu,resources=pullrequests,verbs=delete

// deleteInactive deletes the review app of a pull request that has been inactive for deleteAfter,
// like when the webhook could not delete it as the pull request closed. It reports whether the pull
// request is done with for this run, as it was deleted or changed since it was listed
func (ds Downscaler) deleteInactive(ctx context.Context, reviewApp reviewapps.ReviewAppConfig, pr reviewapps.PullRequest) bool {
	deleteAfter := reviewApp.Spec.DeleteAfter.Duration
	if deleteAfter == 0 || !pr.DeletionTimestamp.IsZero() {
		return false
	}

	last := lastActivity(pr)
	if time.Since(last) < deleteAfter {
		return false
	}

	// The preconditions keep a review app that was deployed or visited since it was listed
	err := ds.Delete(ctx, &pr, client.Preconditions{UID: &pr.UID, ResourceVersion: &pr.ResourceVersion})
	switch {
	case apierrors.IsConflict(err):
		return true
	case client.IgnoreNotFound(err) != nil:
		log.Error(err, "Failed to delete inactive review app", "name", pr.Name, "namespace", pr.Namespace)
		return false
	}

	log.Info("Deleted inactive review app",
		"name", pr.Name, "namespace", pr.Namespace, "branch", pr.Spec.BranchName,
		"lastActivity", last, "deleteAfter", deleteAfter)
	return true
}
