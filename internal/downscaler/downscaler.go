package downscaler

import (
	"context"
	"time"

	"github.com/wille/review-app-operator/internal/utils"
	appsv1 "k8s.io/api/apps/v1"
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

	ticker := time.NewTicker(time.Second * 10)

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
	var list appsv1.DeploymentList
	if err := ds.Client.List(ctx, &list, utils.MatchingLabels); err != nil {
		log.Error(err, "Failed to list deployments")
		return
	}

	for _, deployment := range list.Items {
		// Ignore downscaled deployments
		if *deployment.Spec.Replicas == 0 {
			continue
		}

		// Ignore deployments with the annotation not set
		// This is fine for now since new PRs are downscaled by default
		d := deployment.Annotations[utils.LastRequestTimeAnnotation]
		if d == "" {
			continue
		}

		timestamp, err := time.Parse(time.RFC3339, d)
		if err != nil {
			continue
		}

		// If the latest request was more than TimeoutSeconds ago, scale down the deployment
		if timestamp.Add(dur).Before(time.Now()) {
			var replicas int32 = 0
			deployment.Spec.Replicas = &replicas

			// TODO Add event to the deployment
			if err := ds.Client.Update(ctx, &deployment); err != nil {
				log.Error(err, "Failed to downscale deployment", "deployment", deployment.Name)
				continue
			}

			log.Info("Scaled down", "name", deployment.Name)
		}
	}
}
