package forwarder

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/wille/rac/internal/utils"
	appsv1 "k8s.io/api/apps/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Downscaler watches deployments and scales them down if unused for some time
type Downscaler struct {
	client.Client
	TimeoutSeconds int
}

// StartWatching starts the ticker to watch deployments
func (ds Downscaler) StartWatching() {
	ticker := time.NewTicker(time.Second * 10)
	for range ticker.C {
		ds.run()
	}
}

func (ds Downscaler) run() {
	var list appsv1.DeploymentList
	if err := ds.Client.List(context.Background(), &list, utils.MatchingLabels); err != nil {
		panic(err)
	}

	for _, deployment := range list.Items {
		// Ignore downscaled deployments
		if *deployment.Spec.Replicas == 0 {
			continue
		}

		// Ignore deployments with the annotation not set
		d := deployment.Annotations[utils.LastRequestTimeAnnotation]
		if d == "" {
			continue
		}

		timestamp, err := strconv.Atoi(d)
		if err != nil {
			continue
		}

		lastRequest := time.Unix(int64(timestamp), 0)
		treshold := lastRequest.Add(time.Duration(ds.TimeoutSeconds) * time.Second)

		// If the latest request was more than TimeoutSeconds ago, scale down the deployment
		if treshold.Before(time.Now()) {
			var replicas int32 = 0
			deployment.Spec.Replicas = &replicas
			if err := ds.Client.Update(context.Background(), &deployment); err != nil {
				panic(err)
			}

			fmt.Println("Scaled down", deployment.Name)
		}
	}
}
