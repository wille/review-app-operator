package forwarder

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/utils"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ctrl "sigs.k8s.io/controller-runtime"
)

// forwardedHeaders are set by the ingress in front of the forwarder. The reverse proxy removes
// them from the request to the review app, so they are copied over
var forwardedHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

func appendHostToXForwardHeader(header http.Header, host string) {
	if prior, ok := header["X-Forwarded-For"]; ok {
		host = strings.Join(prior, ", ") + ", " + host
	}
	header.Set("X-Forwarded-For", host)
}

type Forwarder struct {
	Addr string
	client.Client
	ConnectionTimeout time.Duration

	// Serving reports whether the forwarder accepts requests, for its readiness probe
	Serving *utils.Serving

	// ShutdownTimeout is how long the forwarder waits for the requests in flight when it stops
	ShutdownTimeout time.Duration
}

func getClusterDomain() string {
	if env := os.Getenv("KUBERNETES_CLUSTER_DOMAIN"); env != "" {
		return env
	}

	return "cluster.local"
}

var requestID int64

// readHeaderTimeout limits how long clients may take to send the request headers
const readHeaderTimeout = 10 * time.Second

// retryInterval is how often the forwarder tries to connect to a review app that is starting
const retryInterval = time.Second

// transport is shared across all proxied requests so that connections to the
// upstream review app services can be reused.
var transport http.RoundTripper = http.DefaultTransport.(*http.Transport).Clone()

// errNotStarted is returned when a review app could not be connected to within the connection timeout
var errNotStarted = errors.New("the review app did not start in time")

// startingTransport connects to a review app, and retries while it can't be connected to
// because it is starting
type startingTransport struct {
	timeout time.Duration

	// waiting is called for every failed connection attempt
	waiting func(attempt int, err error)
}

func (t startingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The transport closes the request body even when it fails to connect, but nothing has
	// been read from it then, so the body is reused for every connection attempt while
	// the review app is scaling up
	if req.Body != nil && req.Body != http.NoBody {
		req.Body = io.NopCloser(req.Body)
	}

	timeout := time.NewTimer(t.timeout)
	defer timeout.Stop()

	for attempt := 0; ; attempt++ {
		resp, err := transport.RoundTrip(req.Clone(req.Context()))

		// Only retry while the review app can't be connected to. Once the request
		// has been sent it may have been processed, and the body has been read
		if err == nil || !isDialError(err) {
			return resp, err
		}

		t.waiting(attempt, err)

		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-timeout.C:
			return nil, fmt.Errorf("%w: %w", errNotStarted, err)
		case <-time.After(retryInterval):
		}
	}
}

func (fwd Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := fwd.Client

	// List all deployments indexed by the host
	var list reviewapps.PullRequestList
	if err := c.List(r.Context(), &list, client.MatchingFields{utils.HostIndexFieldName: r.Host}); err != nil {
		ctrl.Log.WithName("forwarder").Error(err, "Error listing pull requests", "host", r.Host)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	reqID := atomic.AddInt64(&requestID, 1)
	log := ctrl.Log.WithName("forwarder").WithValues("host", r.Host, "request", r.Method+" "+r.URL.String(), "id", reqID)

	if len(list.Items) == 0 {
		// No deployments indexed for this host found
		log.Info("No deployment found", "req", r.URL)
		http.Error(w, fmt.Sprintf("No review app found for host %s", r.Host), http.StatusNotFound)
		return
	}

	if len(list.Items) > 1 {
		// More than one deployment indexed for this host found
		log.Error(nil, "More than one deployment found for host", "list", list)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	pr := list.Items[0]

	// Get the ReviewAppConfig for the PR so we can run utils.GetDeploymentName below
	var reviewApp reviewapps.ReviewAppConfig
	if err := c.Get(
		r.Context(),
		client.ObjectKey{Namespace: pr.Namespace, Name: pr.Spec.ReviewAppConfigRef},
		&reviewApp,
	); err != nil {
		log.Error(err, "Error getting review app")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	doUpdate := false
	patch := client.MergeFrom(pr.DeepCopy())

	target := ""

	var deploymentName string
	for deploymentName2, status := range pr.Status.Deployments {
		if target != "" {
			break
		}
		for _, hostname := range status.Hostnames {
			if hostname == r.Host {
				deploymentName = utils.GetDeploymentName(&reviewApp, &pr, deploymentName2)

				target = fmt.Sprintf("%s.%s.svc.%s:%d", deploymentName, pr.Namespace, getClusterDomain(), 80)

				if !status.IsActive {
					status.IsActive = true
					doUpdate = true
				}

				// Only patch PullRequest last active timestamp once a minute
				if status.LastActive.Add(time.Minute).Before(time.Now()) {
					doUpdate = true
					status.LastActive = metav1.Now()
				}

				break
			}
		}

	}

	log = log.WithValues("deployment", deploymentName)

	if doUpdate {
		if err := c.Status().Patch(r.Context(), &pr, patch); err != nil {
			log.Error(err, "Error updating deployment")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}

	deployment := client.ObjectKey{Namespace: pr.Namespace, Name: deploymentName}

	// The reverse proxy passes WebSocket upgrades through and sends streamed responses,
	// like server-sent events, to the client as the review app writes them
	proxy := &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			// The Host header is kept, so that review apps can serve many hosts
			p.Out.URL.Scheme = "http"
			p.Out.URL.Host = target

			for _, header := range forwardedHeaders {
				if values, ok := p.In.Header[header]; ok {
					p.Out.Header[header] = values
				}
			}

			if clientIP, _, err := net.SplitHostPort(p.In.RemoteAddr); err == nil {
				appendHostToXForwardHeader(p.Out.Header, clientIP)
			}

			// The label values are "normalized"
			p.Out.Header.Set("X-Branch", pr.Labels["app.kubernetes.io/name"])
			p.Out.Header.Set("X-Review-App", pr.Labels["app.kubernetes.io/part-of"])
		},
		Transport: startingTransport{
			timeout: fwd.ConnectionTimeout,
			waiting: func(attempt int, err error) {
				if attempt%5 == 0 {
					log.Info(fmt.Sprintf("Connection attempt: %s: %s", fwd.deploymentStatus(r.Context(), log, deployment), err))
				}
			},
		},
		ModifyResponse: func(resp *http.Response) error {
			log.Info("Forwarding response", "status", resp.Status)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			switch {
			case errors.Is(err, errNotStarted):
				status := fwd.deploymentStatus(r.Context(), log, deployment)
				log.Info(fmt.Sprintf("Upstream timeout: %s", status))
				notStartedResponse(w, r, status)
			case errors.Is(err, context.Canceled):
				log.Info("Connection reset")
			default:
				log.Error(err, "Error forwarding request")
				http.Error(w, "Bad gateway", http.StatusBadGateway)
			}
		},
	}

	proxy.ServeHTTP(w, r)
}

// deploymentStatus describes the rollout of the deployment of a review app
func (fwd Forwarder) deploymentStatus(ctx context.Context, log logr.Logger, key client.ObjectKey) string {
	var deployment appsv1.Deployment
	if err := fwd.Get(ctx, key, &deployment); err != nil {
		log.Error(err, "Error getting deployment")
		return ""
	}

	status, _, _ := utils.GetDeploymentStatus(&deployment)
	return strings.Trim(status, "\n")
}

// notStartedResponse is sent when the review app was not scaled up within the connection timeout.
// Browsers loading a page get a page that reloads until the review app has started, while other
// requests, like API calls, are told to retry later
func notStartedResponse(w http.ResponseWriter, r *http.Request, status string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if r.Method != http.MethodGet || !strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "The review app is starting, retry later: "+status, http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintf(w, `<html>
	<head>
		<title>Loading...</title>
		<meta http-equiv="refresh" content="5">
	</head>
	<body>
		<p>Connecting to review app...</p>
		<p>%s</p>
	</body>
	</html>`, html.EscapeString(status))
}

// isDialError reports whether err is a failure to connect to the review app
func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

func (fw Forwarder) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("forwarder")

	log.Info("Starting forwarding proxy", "addr", fw.Addr)

	ln, err := net.Listen("tcp", fw.Addr)
	if err != nil {
		return err
	}

	srv := &http.Server{Handler: fw, ReadHeaderTimeout: readHeaderTimeout}
	err = utils.Serve(ctx, ln, srv, fw.Serving, fw.ShutdownTimeout)
	log.Info("Stopped forwarding proxy")
	return err
}
