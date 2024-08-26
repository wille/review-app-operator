package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/controller"
	"github.com/wille/review-app-operator/internal/downscaler"
	"github.com/wille/review-app-operator/internal/forwarder"
	webhooks "github.com/wille/review-app-operator/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(reviewapps.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var enableForwarder bool
	var enableDeployWebhook bool
	var enableController bool

	var connectionTimeout time.Duration
	flag.DurationVar(&connectionTimeout, "connection-timeout", time.Second*10, "Timeout connecting to PR apps")

	var scaleDownAfter time.Duration
	flag.DurationVar(&scaleDownAfter, "scale-down-after", time.Hour, "Scale down deployments that has not been accessed for some time")

	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var secureMetrics bool
	var enableHTTP2 bool
	flag.BoolVar(&enableForwarder, "forwarder", false, "Enable the forwarding proxy")
	flag.BoolVar(&enableDeployWebhook, "webhook", false, "Enable the deployment webhook")
	flag.BoolVar(&enableController, "controller", false, "Enable the controller webhook")

	cacheOpts := cache.Options{
		DefaultNamespaces: map[string]cache.Config{},
	}
	flag.Func("namespaces", "Namespaces", func(ns string) error {
		if ns == "" {
			return errors.New("No namespaces set")
		}
		for _, ns := range strings.Split(ns, ",") {
			cacheOpts.DefaultNamespaces[ns] = cache.Config{}
		}
		return nil
	})

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metric endpoint binds to. "+
		"Use the port :8080. If not set, it will be 0 in order to disable the metrics server")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set the metrics endpoint is served securely")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	tlsOpts := []func(*tls.Config){}
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			SecureServing: secureMetrics,
			TLSOpts:       tlsOpts,
		},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,

		// Only the controller-manager needs leader election
		LeaderElection:   enableController && enableLeaderElection,
		LeaderElectionID: "80807133.reviewapps.william.nu",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
		Cache: cacheOpts,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if enableController || enableForwarder {
		if err := controller.SetupHostIndex(mgr); err != nil {
			setupLog.Error(err, "Failed to index host field")
			os.Exit(1)
		}
	}

	if enableController {
		if err = (&controller.ReviewAppConfigReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ReviewAppConfig")
			os.Exit(1)
		}
		if err = (&controller.PullRequestReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "PullRequest")
			os.Exit(1)
		}
		// +kubebuilder:scaffold:builder
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	webhookSecret := os.Getenv("WEBHOOK_SECRET")
	if enableDeployWebhook && webhookSecret == "" {
		setupLog.Error(err, "env WEBHOOK_SECRET not set")
		os.Exit(1)
	}

	if enableDeployWebhook {
		if err := mgr.Add(webhooks.WebhookServer{Client: mgr.GetClient(), Addr: ":8080", WebhookSecret: webhookSecret}); err != nil {
			setupLog.Error(err, "unable to create pull request webhook server")
			os.Exit(1)
		}
	}

	if enableForwarder {
		if err := mgr.Add(forwarder.Forwarder{Client: mgr.GetClient(), Addr: ":6969", ConnectionTimeout: connectionTimeout}); err != nil {
			setupLog.Error(err, "unable to setup the forwarding proxy")
			os.Exit(1)
		}
	}

	if enableController {
		if err := mgr.Add(downscaler.Downscaler{Client: mgr.GetClient(), ScaleDownAfter: scaleDownAfter}); err != nil {
			setupLog.Error(err, "unable to create downscaler")
			os.Exit(1)
		}
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
