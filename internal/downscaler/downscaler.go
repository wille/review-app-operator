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
	ScaleDownAfter time.Duration
}

var _ manager.LeaderElectionRunnable = &Downscaler{}

// Only run the downscaler on the lead manager
func (ds Downscaler) NeedLeaderElection() bool {
	return true
}

// Starts the ticker to watch deployments
func (ds Downscaler) Start(ctx context.Context) error {
	log.Info("starting downscaler", "scaleDownAfter", ds.ScaleDownAfter)

	ticker := time.NewTicker(time.Minute)

	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping downscaler")
			return nil
		case <-ticker.C:
			ds.run(ds.ScaleDownAfter, ctx)
		}
	}
}

func (ds Downscaler) run(dur time.Duration, ctx context.Context) {
	// List all deployments owned by the Review App Operator
	var list PullRequestList
	if err := ds.Client.List(ctx, &list); err != nil {
		log.Error(err, "Failed to list pull requests")
		return
	}

	for _, pr := range list.Items {
		ds.processPullRequest(pr, dur, ctx)
	}
}

func (ds Downscaler) processPullRequest(pr PullRequest, dur time.Duration, ctx context.Context) error {
	for deploymentName, status := range pr.Status.Deployments {
		if !status.IsActive {
			continue
		}

		// TODO duraiton ReviewAppConfig.Spec.ScaleDownAfter || Deployment ScaleDownAfter

		// If the latest request was more than TimeoutSeconds ago, scale down the deployment
		if status.LastActive.Add(dur).Before(time.Now()) {
			patch := client.MergeFrom(pr.DeepCopy())
			pr.Status.Deployments[deploymentName].IsActive = false

			// TODO Add event to the deployment
			if err := ds.Client.Status().Patch(ctx, &pr, patch); err != nil {
				log.Error(err, "Failed to downscale deployment", "deployment", pr.Name)
				continue
			}

			log.Info("Scaling down", "name", pr.Name, "lastActive", status.LastActive, "now", time.Now().Format(time.RFC3339))
		}
	}

	return nil
}
