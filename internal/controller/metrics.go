package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/metrics"
	"github.com/wille/review-app-operator/internal/utils"
)

// Whether a deployment of a review app runs
const (
	stateRunning    = "running"
	stateScaledDown = "scaled_down"
)

// recordScale counts a deployment of a review app that was started, or scaled down, by the
// replicas it had and has
func recordScale(reviewApp, deployment string, from, to int32) {
	switch {
	case from == 0 && to > 0:
		metrics.Scaled.WithLabelValues(reviewApp, deployment, "up").Inc()
	case from > 0 && to == 0:
		metrics.Scaled.WithLabelValues(reviewApp, deployment, "down").Inc()
	}
}

var deploymentsDesc = prometheus.NewDesc(
	"review_app_deployments",
	"Deployments of review apps, by whether they run or are scaled down",
	[]string{metrics.ReviewAppConfig, metrics.Deployment, "state"}, nil,
)

// DeploymentsCollector reports the deployments of the review apps by whether they run, from the
// cache. Only the leading controller reports them, so that they aren't counted once per replica
type DeploymentsCollector struct {
	Client client.Reader

	// Elected is closed once the controller leads
	Elected <-chan struct{}
}

func (c DeploymentsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- deploymentsDesc
}

func (c DeploymentsCollector) Collect(ch chan<- prometheus.Metric) {
	select {
	case <-c.Elected:
	default:
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	counts, err := countDeployments(ctx, c.Client)
	if err != nil {
		ctrl.Log.WithName("metrics").Error(err, "Failed to count the deployments of the review apps")
		return
	}

	for key, count := range counts {
		ch <- prometheus.MustNewConstMetric(deploymentsDesc, prometheus.GaugeValue, float64(count),
			key.reviewApp, key.deployment, key.state)
	}
}

// deploymentsKey is a deployment of a ReviewAppConfig in a state. ReviewAppConfigs of the same
// name in different namespaces are counted together, like in the other metrics
type deploymentsKey struct {
	reviewApp, deployment, state string
}

// countDeployments counts the deployments of the review apps of every ReviewAppConfig by state,
// including zero for the states without deployments
func countDeployments(ctx context.Context, c client.Reader) (map[deploymentsKey]int, error) {
	var reviewAppList reviewapps.ReviewAppConfigList
	if err := c.List(ctx, &reviewAppList); err != nil {
		return nil, err
	}

	var pullRequests reviewapps.PullRequestList
	if err := c.List(ctx, &pullRequests); err != nil {
		return nil, err
	}

	counts := map[deploymentsKey]int{}
	reviewApps := map[types.NamespacedName]*reviewapps.ReviewAppConfig{}
	for i := range reviewAppList.Items {
		reviewApp := &reviewAppList.Items[i]
		reviewApps[client.ObjectKeyFromObject(reviewApp)] = reviewApp

		for _, deployment := range reviewApp.Spec.Deployments {
			counts[deploymentsKey{reviewApp.Name, deployment.Name, stateRunning}] += 0
			counts[deploymentsKey{reviewApp.Name, deployment.Name, stateScaledDown}] += 0
		}
	}

	for i := range pullRequests.Items {
		pr := &pullRequests.Items[i]
		reviewApp := reviewApps[types.NamespacedName{Namespace: pr.Namespace, Name: pr.Spec.ReviewAppConfigRef}]
		if reviewApp == nil {
			continue
		}

		for _, spec := range reviewApp.Spec.Deployments {
			var deployment appsv1.Deployment
			key := types.NamespacedName{Namespace: pr.Namespace, Name: utils.GetDeploymentName(reviewApp, pr, spec.Name)}
			if err := c.Get(ctx, key, &deployment); err != nil || !metav1.IsControlledBy(&deployment, pr) {
				continue
			}

			state := stateRunning
			if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
				state = stateScaledDown
			}
			counts[deploymentsKey{reviewApp.Name, spec.Name, state}]++
		}
	}

	return counts, nil
}
