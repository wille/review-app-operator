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

const (
	maxRetries        = 120
	retryDelaySeconds = 1
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
}

func getClusterDomain() string {
	env := os.Getenv("KUBERNETES_CLUSTER_DOMAIN")

	if env != "" {
		return env
	}

	return "cluster.local"
}

func (fwd Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := fwd.Client

	// List all deployments indexed by the host
	var list appsv1.DeploymentList
	if err := c.List(context.Background(), &list, client.MatchingFields{utils.HostIndexFieldName: r.Host}); err != nil {
		panic(err)
	}

	if len(list.Items) == 0 {
		// No deployments indexed for this host found
		fmt.Println("No deployment found for host", r.Host)
		http.Error(w, fmt.Sprintf("No review app found for host %s", r.Host), http.StatusNotFound)
		return
	}

	if len(list.Items) > 1 {
		// More than one deployment indexed for this host found
		fmt.Println("More than one deployment found for host", r.Host)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	deployment := list.Items[0]

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
	hostname := fmt.Sprintf("%s.%s.svc.%s:%d", deployment.Name, deployment.Namespace, getClusterDomain(), port)

	retries := 0

	for {
		select {
		// Connection closed
		case <-r.Context().Done():
			return
		default:
		}
		client := &http.Client{}

		//http: Request.RequestURI can't be set in client requests.
		//http://golang.org/src/pkg/net/http/client.go
		r.RequestURI = ""
		r.URL.Scheme = "http"
		r.URL.Host = hostname

		delHopHeaders(r.Header)

		if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			appendHostToXForwardHeader(r.Header, clientIP)
		}

		resp, err := client.Do(r)
		if err != nil {
			if retries == 0 {
				log.Error(err, "Error connecting to review app, retrying...", "hostname", hostname, "deployment", deployment.Name)
			}
			retries++
			if retries > maxRetries {
				log.Error(err, "Giving up connecting to review app", "hostname", hostname, "deployment", deployment.Name)
				http.Error(w, "Connection error", http.StatusGatewayTimeout)
				return
			}

			// Delay before trying again
			time.Sleep(retryDelaySeconds * time.Second)
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

		log.Info(fmt.Sprintf("%s %s %s %s %s->%s", r.RemoteAddr, r.Method, r.URL, resp.Status, r.Host, hostname))
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
