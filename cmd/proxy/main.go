package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wille/rac/internal/utils"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

type proxy struct {
	client.Client
}

func getClusterDomain() string {
	env := os.Getenv("KUBERNETES_CLUSTER_DOMAIN")

	if env != "" {
		return env
	}

	return "cluster.local"
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := p.Client

	// List all services indexed by the host
	var list corev1.ServiceList
	if err := c.List(context.Background(), &list, client.MatchingFields{utils.HostIndexFieldName: r.Host}); err != nil {
		panic(err)
	}

	if len(list.Items) == 0 {
		// No services indexed for this host found
		fmt.Println("No service found for host", r.Host)
		http.Error(w, fmt.Sprintf("No review app found for host %s", r.Host), http.StatusNotFound)
		return
	}

	if len(list.Items) > 1 {
		// More than one service indexed for this host found
		fmt.Println("More than one service found for host", r.Host)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	svc := list.Items[0]

	// Read the deployment that the service routes to.
	// This assumes that the deployment has the same name as the service.
	var deployment appsv1.Deployment
	if err := c.Get(r.Context(), types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, &deployment); err != nil {
		fmt.Println("Error getting deployment:", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	doUpdate := false
	patch := client.MergeFrom(deployment.DeepCopy())

	// Scale up the deployment
	if *deployment.Spec.Replicas == 0 {
		var replicas int32 = 1
		deployment.Spec.Replicas = &replicas

		log.Println("Scaling up deployment", deployment.Name)
		doUpdate = true
	}

	if deployment.ObjectMeta.Annotations == nil {
		deployment.ObjectMeta.Annotations = make(map[string]string)
	}

	// Update the deployment's last request annotation only if it has changed to avoid excessive requests
	timestamp := strconv.Itoa(int(time.Now().Unix()))
	if timestamp != deployment.ObjectMeta.Annotations[utils.LastRequestTimeAnnotation] {
		deployment.ObjectMeta.Annotations[utils.LastRequestTimeAnnotation] = timestamp
		doUpdate = true
	}

	if doUpdate {
		if err := c.Patch(context.Background(), &deployment, patch); err != nil {
			fmt.Println("Error updating deployment:", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}

	// Format the target service to forward the request to
	// eg. review-app-1.default.svc.cluster.local:8080
	port := svc.Spec.Ports[0].TargetPort.String()
	hostname := fmt.Sprintf("%s.%s.svc.%s:%s", svc.Name, svc.Namespace, getClusterDomain(), port)

	retries := 0

	for {
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
			retries++
			if retries > 60 {
				log.Println("Failed to connect to review app", err)
				http.Error(w, "Connection error", http.StatusGatewayTimeout)
				return
			}
			time.Sleep(1 * time.Second)
			continue
		}
		defer resp.Body.Close()

		delHopHeaders(resp.Header)

		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)

		log.Println(fmt.Sprintf("%s %s %s %s %s->%s", r.RemoteAddr, r.Method, r.URL, resp.Status, r.Host, hostname))
		break
	}

}

func Start(client client.Client) {
	http.ListenAndServe(":6969", &proxy{client})
}
