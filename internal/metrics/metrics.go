// Package metrics defines the metrics of the review apps, which the metrics endpoint of each
// component serves with those of controller-runtime.
//
// Their labels are the names of ReviewAppConfigs and their deployments, which are few, and never
// branch names or anything else that a request can choose
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// The label names
const (
	// ReviewAppConfig is the name of the ReviewAppConfig of the review app
	ReviewAppConfig = "review_app_config"
	// Deployment is the name of the deployment in the ReviewAppConfig
	Deployment = "deployment"
)

var (
	// Requests counts the responses to requests for review apps by their status code, from the
	// review apps or from the forwarder while they start
	Requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "review_app_requests_total",
		Help: "Responses of the forwarder to requests for review apps, by status code",
	}, []string{ReviewAppConfig, Deployment, "code"})

	// StartWait observes how long requests waited for review apps that were starting
	StartWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "review_app_start_wait_seconds",
		Help:    "How long requests waited for review apps that were starting to accept connections",
		Buckets: []float64{0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300},
	}, []string{ReviewAppConfig, Deployment})

	// StartTimeouts counts the requests that the review app did not start within the connection timeout for
	StartTimeouts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "review_app_start_timeouts_total",
		Help: "Requests that got the page that reloads, or 503 Service Unavailable, as their review app didn't start within the connection timeout",
	}, []string{ReviewAppConfig, Deployment})

	// Scaled counts the deployments of review apps that the controller started and scaled down
	Scaled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "review_app_scaled_total",
		Help: "Deployments of review apps that the controller started (up) and scaled down (down)",
	}, []string{ReviewAppConfig, Deployment, "direction"})

	// Deleted counts the review apps that the controller deleted after deleteAfter
	Deleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "review_app_deleted_total",
		Help: "Review apps that the controller deleted, as they were neither deployed nor visited for deleteAfter",
	}, []string{ReviewAppConfig})

	// Deploys counts the deploys of the webhook by their result
	Deploys = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "review_app_deploys_total",
		Help: "Deploys of the webhook, by result: succeeded, failed, or rejected for the ID token",
	}, []string{ReviewAppConfig, "result"})

	// DeployDuration observes how long deploys took until the review app ran the new image
	DeployDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "review_app_deploy_duration_seconds",
		Help:    "How long successful deploys took, from the webhook until the review app ran the new image",
		Buckets: []float64{5, 10, 20, 30, 60, 120, 180, 300, 600},
	}, []string{ReviewAppConfig})
)

// The results of deploys
const (
	DeploySucceeded = "succeeded"
	DeployFailed    = "failed"
	DeployRejected  = "rejected"
)

func init() {
	ctrlmetrics.Registry.MustRegister(Requests, StartWait, StartTimeouts, Scaled, Deleted, Deploys, DeployDuration)
}
