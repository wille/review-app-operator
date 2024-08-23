package forwarder

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/wille/review-app-operator/internal/utils"
	appsv1 "k8s.io/api/apps/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ctrl "sigs.k8s.io/controller-runtime"
)

var log = ctrl.Log.WithName("forwarder")

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

func (fwd Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := fwd.Client

	// List all deployments indexed by the host
	var list appsv1.DeploymentList
	if err := c.List(r.Context(), &list, client.MatchingFields{utils.HostIndexFieldName: r.Host}); err != nil {
		panic(err)
	}

	log = ctrl.Log.WithName("forwarder").WithValues("host", r.Host, "request", r.Method+" "+r.URL.String())

	if len(list.Items) == 0 {
		// No deployments indexed for this host found
		// TODO BETTER ERRORS
		log.Error(nil, "No deployment found", "req", r.URL)
		http.Error(w, fmt.Sprintf("No review app found for host %s", r.Host), http.StatusNotFound)
		return
	}

	if len(list.Items) > 1 {
		// More than one deployment indexed for this host found
		log.Error(nil, "More than one deployment found for host", "list", list)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	deployment := list.Items[0]

	log = log.WithValues("deployment", deployment.Name)

	doUpdate := false
	patch := client.MergeFrom(deployment.DeepCopy())

	// Scale up the deployment
	if *deployment.Spec.Replicas == 0 {
		var replicas int32 = 1
		deployment.Spec.Replicas = &replicas

		log.Info("Scaling up")
		doUpdate = true
	}

	timestamp := time.Now().Format(time.RFC3339)

	// Update the deployment's last request annotation once a minute avoid excessive patch requests
	currentTimestamp, err := time.Parse(time.RFC3339, deployment.ObjectMeta.Annotations[utils.LastRequestTimeAnnotation])
	if err != nil || currentTimestamp.Add(time.Minute).Before(time.Now()) {
		deployment.ObjectMeta.Annotations[utils.LastRequestTimeAnnotation] = timestamp
		doUpdate = true
	}

	if doUpdate {
		if err := c.Patch(r.Context(), &deployment, patch); err != nil {
			log.Error(err, "Error updating deployment")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}

	// Format the target service to forward the request to
	// eg. review-app-1.default.svc.cluster.local:80
	// Always uses port 80
	port := 80
	serviceHostname := fmt.Sprintf("%s.%s.svc.%s:%d", deployment.Name, deployment.Namespace, getClusterDomain(), port)

	// or := r.Clone(r.Context())

	timeout, _ := context.WithTimeout(r.Context(), fwd.ConnectionTimeout)

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
	r.Header.Set("X-Branch", deployment.Labels["app.kubernetes.io/name"])
	r.Header.Set("X-Review-App", deployment.Labels["app.kubernetes.io/part-of"])

	attempt := 0

	for {
		select {
		// connectionTimeout
		case <-timeout.Done():
			// Update the deployment to be able to read the latest status
			c.Update(r.Context(), &deployment)
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
		client := &http.Client{}

		resp, err := client.Do(r)
		if err != nil {
			if attempt%5 == 0 {
				status, _, _ := utils.GetDeploymentStatus(&deployment)
				log.Info(fmt.Sprintf("Connection attempt: %s: %s", strings.Trim(status, "\n"), err.Error()))
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

		log.WithValues("status", resp.Status).Info("Request finished")
		break
	}
}

func (fw Forwarder) Start(ctx context.Context) error {
	log.Info("Starting forwarding proxy", "addr", fw.Addr)

	srv := http.Server{Addr: fw.Addr, Handler: fw}

	go func() {
		<-ctx.Done()
		log.Info("Shutting down forwarding proxy")

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	// TODO will error when closed
	return srv.ListenAndServe()
}
