package downscaler

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wille/rac/internal/utils"
	appsv1 "k8s.io/api/apps/v1"
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
	scaleDownAfter := os.Getenv("SCALE_DOWN_AFTER")

	if strings.ToLower(scaleDownAfter) == "Never" {
		log.Info("downscaler disabled")
		return nil
	}

	if scaleDownAfter == "" {
		scaleDownAfter = "1h"
	}

	dur, err := time.ParseDuration(scaleDownAfter)
	if err != nil {
		log.Error(err, "unable to parse SCALE_DOWN_AFTER")
		return err
	}

	log.Info("starting downscaler", "scaleDownAfter", dur)

	ticker := time.NewTicker(time.Second * 10)
	for range ticker.C {
		ds.run(dur)
	}

	return nil
}

func (ds Downscaler) run(dur time.Duration) {
	// List all deployments owned by the Review App Operator
	var list appsv1.DeploymentList
	if err := ds.Client.List(context.Background(), &list, utils.MatchingLabels); err != nil {
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

		timestamp, err := strconv.Atoi(d)
		if err != nil {
			continue
		}

		lastRequest := time.Unix(int64(timestamp), 0)
		treshold := lastRequest.Add(dur)

		// If the latest request was more than TimeoutSeconds ago, scale down the deployment
		if treshold.Before(time.Now()) {
			var replicas int32 = 0
			deployment.Spec.Replicas = &replicas

			// TODO Add event to the deployment
			if err := ds.Client.Update(context.Background(), &deployment); err != nil {
				log.Error(err, "Failed to downscale deployment", "deployment", deployment.Name)
				continue
			}

			log.Info("Scaled down", "name", deployment.Name)
		}
	}
}
