package downscaler

import (
	"context"
	"time"

	. "github.com/wille/review-app-operator/api/v1alpha1"
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
	log.Info("starting downscaler", "scaleDownAfter")

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
	var list ReviewAppConfigList
	if err := ds.Client.List(ctx, &list); err != nil {
		log.Error(err, "Failed to list review apps")
		return
	}

	for _, reviewApp := range list.Items {
		var prs PullRequestList
		if err := ds.Client.List(ctx, &prs, client.MatchingFields{"spec.reviewAppRef": reviewApp.Name}); err != nil {
			log.Error(err, "Failed to list pull requests for review app", "reviewApp", reviewApp.Name)
			break
		}

		for _, pr := range prs.Items {
			ds.process(reviewApp, pr, ctx)
		}
	}
}

func (ds Downscaler) process(reviewApp ReviewAppConfig, pr PullRequest, ctx context.Context) {
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
