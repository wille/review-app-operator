package forwarder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/utils"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Hop-by-hop headers. These are removed when sent to the backend.
// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te", // canonicalized version of "TE"
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func delHopHeaders(header http.Header) {
	for _, h := range hopHeaders {
		header.Del(h)
	}
}

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
}

func getClusterDomain() string {
	if env := os.Getenv("KUBERNETES_CLUSTER_DOMAIN"); env != "" {
		return env
	}

	return "cluster.local"
}

var requestID int64

// httpClient is shared across all proxied requests so that connections to the
// upstream review app services can be reused.
var httpClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (fwd Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := fwd.Client

	// List all deployments indexed by the host
	var list PullRequestList
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
	var reviewApp ReviewAppConfig
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

	serviceHostname := target

	timeout, cancel := context.WithTimeout(r.Context(), fwd.ConnectionTimeout)
	defer cancel()

	//http: Request.RequestURI can't be set in client requests.
	//http://golang.org/src/pkg/net/http/client.go
	r.RequestURI = ""
	r.URL.Scheme = "http"
	r.URL.Host = serviceHostname
	delHopHeaders(r.Header)

	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		appendHostToXForwardHeader(r.Header, clientIP)
	}

	// The label values are "normalized"
	r.Header.Set("X-Branch", pr.Labels["app.kubernetes.io/name"])
	r.Header.Set("X-Review-App", pr.Labels["app.kubernetes.io/part-of"])

	// Buffer the request body so that it can be re-sent on each connection
	// attempt while we wait for the review app to scale up. Without this, a
	// retried request (the common cold-start path) would forward an empty body.
	var bodyBytes []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			log.Error(err, "Error reading request body")
			http.Error(w, "Error reading request body", http.StatusBadRequest)
			return
		}
		bodyBytes = b
	}

	attempt := 0

	for {
		select {
		// connectionTimeout
		case <-timeout.Done():
			// Update the deployment to be able to read the latest status
			var deployment appsv1.Deployment
			if err := c.Get(r.Context(), client.ObjectKey{
				Namespace: pr.Namespace,
				Name:      deploymentName,
			}, &deployment); err != nil {
				log.Error(err, "Waiting for deployment")
				http.Error(w, "Waiting for deployment", http.StatusInternalServerError)
				return
			}
			status, _, _ := utils.GetDeploymentStatus(&deployment)

			log.Info(fmt.Sprintf("Upstream timeout: %s", strings.Trim(status, "\n")))

			// This page is sent when the Review App was not scaled up within the `connectionTimeout` limit.
			// Later we can expand it to automatically read deployment status updates and automatically refresh.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, `<html>
			<head>
				<title>Loading...</title>
				<meta http-equiv="refresh" content="5">
			</head>
			<body>
				<p>Connecting to review app...</p>
				<p>%s</p>
			</body>
			</html>`, status)
			return
		// Client connection reset
		case <-r.Context().Done():
			log.Info("Connection reset")
			return
		default:
		}

		// Build a fresh outbound request for every attempt with its own body
		// reader so retries re-send the buffered body.
		outReq := r.Clone(r.Context())
		outReq.RequestURI = ""
		if bodyBytes != nil {
			outReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			outReq.ContentLength = int64(len(bodyBytes))
		}

		resp, err := httpClient.Do(outReq)
		if err != nil {
			if attempt%5 == 0 {
				var deployment appsv1.Deployment
				if _err := c.Get(r.Context(), types.NamespacedName{
					Namespace: pr.Namespace,
					Name:      deploymentName,
				}, &deployment); _err != nil {
					log.Error(_err, "Waiting for deployment")
					http.Error(w, "Waiting for deployment", http.StatusInternalServerError)
				} else {
					status, _, _ := utils.GetDeploymentStatus(&deployment)
					log.Info(fmt.Sprintf("Connection attempt: %s: %s", strings.Trim(status, "\n"), err.Error()))
				}

			}

			attempt++

			// Delay before trying again
			time.Sleep(time.Second)
			continue
		}
		defer resp.Body.Close()

		delHopHeaders(resp.Header)

		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)

		_, err = io.Copy(w, resp.Body)
		if err != nil {
			log.Error(err, "Error copying response body")
			return
		}

		log.Info("Request finished", "status", resp.Status)
		break
	}
}

func (fw Forwarder) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("forwarder")

	log.Info("Starting forwarding proxy", "addr", fw.Addr)

	srv := http.Server{Addr: fw.Addr, Handler: fw}

	go func() {
		<-ctx.Done()
		log.Info("Shutting down forwarding proxy")

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}
