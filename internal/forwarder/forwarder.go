package forwarder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/metrics"
	"github.com/wille/review-app-operator/internal/utils"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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

// The forwarder renews the Lease of a deployment that is visited, which keeps it running for
// scaleDownAfter. Renewals are throttled to a tenth of scaleDownAfter, within these bounds, so that
// a deployment that is in use doesn't scale down, without writing to the API server on every request
const (
	minRenewInterval = 5 * time.Second
	maxRenewInterval = 5 * time.Minute
)

// renewInterval is how often the Lease of a visited deployment that scales down after
// scaleDownAfter is renewed. Deployments that never scale down only need visits for deleteAfter
func renewInterval(scaleDownAfter time.Duration) time.Duration {
	if scaleDownAfter <= 0 {
		return maxRenewInterval
	}
	return min(max(scaleDownAfter/10, minRenewInterval), maxRenewInterval)
}

// renewThrottle lets a forwarder renew the Lease of a deployment once per interval
type renewThrottle struct {
	mu        sync.Mutex
	renewed   map[string]time.Time
	lastPrune time.Time
}

func newRenewThrottle() *renewThrottle {
	return &renewThrottle{renewed: make(map[string]time.Time)}
}

// claim reports whether the Lease is to be renewed, which it is when the forwarder has not renewed
// it in the last interval
func (t *renewThrottle) claim(key string, now time.Time, interval time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Forget the deployments that are no longer visited
	if now.Sub(t.lastPrune) >= maxRenewInterval {
		for k, renewed := range t.renewed {
			if now.Sub(renewed) >= maxRenewInterval {
				delete(t.renewed, k)
			}
		}
		t.lastPrune = now
	}

	if renewed, ok := t.renewed[key]; ok && now.Sub(renewed) < interval {
		return false
	}

	t.renewed[key] = now
	return true
}

// release lets the next visit renew the Lease, after renewing it failed
func (t *renewThrottle) release(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.renewed, key)
}

// renewals throttles the Lease renewals of all forwarders in the process
var renewals = newRenewThrottle()

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

	// started is called with how long the request waited for the review app to accept the
	// connection, when it couldn't at first
	started func(waited time.Duration)
}

func (t startingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The transport closes the request body even when it fails to connect, but nothing has
	// been read from it then, so the body is reused for every connection attempt while
	// the review app is scaling up
	if req.Body != nil && req.Body != http.NoBody {
		req.Body = io.NopCloser(req.Body)
	}

	start := time.Now()
	timeout := time.NewTimer(t.timeout)
	defer timeout.Stop()

	for attempt := 0; ; attempt++ {
		resp, err := transport.RoundTrip(req.Clone(req.Context()))

		// Only retry while the review app can't be connected to. Once the request
		// has been sent it may have been processed, and the body has been read
		if err == nil || !isDialError(err) {
			if attempt > 0 && t.started != nil {
				t.started(time.Since(start))
			}
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

	target := ""

	var deploymentName, deploymentSpecName string
	for name, deploymentStatus := range pr.Status.Deployments {
		if target != "" {
			break
		}
		for _, hostname := range deploymentStatus.Hostnames {
			if hostname == r.Host {
				deploymentSpecName = name
				deploymentName = utils.GetDeploymentName(&reviewApp, &pr, name)

				target = fmt.Sprintf("%s.%s.svc.%s:%d", deploymentName, pr.Namespace, getClusterDomain(), 80)

				break
			}
		}

	}

	log = log.WithValues("deployment", deploymentName)

	// Renew the Lease of the deployment, which starts it when it is scaled down, and keeps it
	// running. Most requests renew nothing, and none wait for the API server
	if deploymentName != "" {
		key := pr.Namespace + "/" + deploymentName
		renewals := renewals

		if renewals.claim(key, time.Now(), renewInterval(scaleDownAfter(&reviewApp, deploymentSpecName))) {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				if err := renewLease(ctx, c, pr.Namespace, deploymentName); err != nil {
					renewals.release(key)
					log.Error(err, "Error renewing the lease of the deployment")
				}
			}()
		}
	}

	deployment := client.ObjectKey{Namespace: pr.Namespace, Name: deploymentName}

	// The metrics of the review app, by the names of its ReviewAppConfig and deployment
	responded := func(code int) {
		metrics.Requests.WithLabelValues(reviewApp.Name, deploymentSpecName, strconv.Itoa(code)).Inc()
	}

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
			timeout: connectionTimeout(&reviewApp, fwd.ConnectionTimeout),
			waiting: func(attempt int, err error) {
				if attempt%5 == 0 {
					log.Info(fmt.Sprintf("Connection attempt: %s: %s", fwd.deploymentStatus(r.Context(), log, deployment), err))
				}
			},
			started: func(waited time.Duration) {
				metrics.StartWait.WithLabelValues(reviewApp.Name, deploymentSpecName).Observe(waited.Seconds())
			},
		},
		ModifyResponse: func(resp *http.Response) error {
			log.Info("Forwarding response", "status", resp.Status)
			responded(resp.StatusCode)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			switch {
			case errors.Is(err, errNotStarted):
				status := fwd.deploymentStatus(r.Context(), log, deployment)
				log.Info(fmt.Sprintf("Upstream timeout: %s", status))
				metrics.StartTimeouts.WithLabelValues(reviewApp.Name, deploymentSpecName).Inc()
				responded(notStartedResponse(w, r, status))
			case errors.Is(err, context.Canceled):
				// The client left, so there is no response
				log.Info("Connection reset")
			default:
				log.Error(err, "Error forwarding request")
				http.Error(w, "Bad gateway", http.StatusBadGateway)
				responded(http.StatusBadGateway)
			}
		},
	}

	proxy.ServeHTTP(w, r)
}

// scaleDownAfter is how long a deployment of a review app runs after it was last visited
func scaleDownAfter(reviewApp *reviewapps.ReviewAppConfig, deployment string) time.Duration {
	for _, spec := range reviewApp.Spec.Deployments {
		if spec.Name == deployment && spec.ScaleDownAfter.Duration != 0 {
			return spec.ScaleDownAfter.Duration
		}
	}
	return reviewApp.Spec.ScaleDownAfter.Duration
}

// connectionTimeout returns how long to wait for a review app to start, which the ReviewAppConfig
// can set for its review apps, like for ones that are slow to start
func connectionTimeout(reviewApp *reviewapps.ReviewAppConfig, defaultTimeout time.Duration) time.Duration {
	if reviewApp.Spec.ConnectionTimeout.Duration > 0 {
		return reviewApp.Spec.ConnectionTimeout.Duration
	}
	return defaultTimeout
}

// holderIdentity names the forwarder that last renewed a Lease, which is its pod name
var holderIdentity, _ = os.Hostname()

// renewLease records a visit to a deployment of a review app in its Lease, which the controller
// creates with the deployment
func renewLease(ctx context.Context, c client.Client, namespace, name string) error {
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"renewTime":      metav1.NowMicro(),
			"holderIdentity": holderIdentity,
		},
	})
	if err != nil {
		return err
	}

	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	return c.Patch(ctx, lease, client.RawPatch(types.MergePatchType, patch))
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
// requests, like API calls, are told to retry later. It returns the status code of the response
func notStartedResponse(w http.ResponseWriter, r *http.Request, status string) int {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if r.Method != http.MethodGet || !strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "The review app is starting, retry later: "+status, http.StatusServiceUnavailable)
		return http.StatusServiceUnavailable
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
	return http.StatusAccepted
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
