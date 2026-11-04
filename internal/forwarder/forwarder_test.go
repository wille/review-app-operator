package forwarder

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is idiomatic for Ginkgo specs
	. "github.com/onsi/gomega"    //nolint:staticcheck // dot-import is idiomatic for Gomega matchers
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/utils"
)

func TestForwarder(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Forwarder")
}

var _ = Describe("Forwarder", func() {
	var (
		fwd Forwarder

		// The number of connection attempts to the review app that are refused
		// before connecting, like while it is scaling up
		refusedDials int32
		dials        atomic.Int32
	)

	// upstream starts the review app and routes all forwarded connections to it
	upstream := func(handler http.HandlerFunc) {
		server := httptest.NewServer(handler)
		DeferCleanup(server.Close)

		// Nothing listens on a closed listener's address, so connections are refused
		closed, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		Expect(closed.Close()).To(Succeed())

		defaultClient := httpClient
		DeferCleanup(func() { httpClient = defaultClient })

		httpClient = &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					addr := server.Listener.Addr().String()
					if dials.Add(1) <= refusedDials {
						addr = closed.Addr().String()
					}
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				},
			},
			CheckRedirect: defaultClient.CheckRedirect,
		}
	}

	BeforeEach(func() {
		refusedDials = 0
		dials.Store(0)

		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(reviewapps.AddToScheme(scheme)).To(Succeed())

		reviewApp := &reviewapps.ReviewAppConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec: reviewapps.ReviewAppConfigSpec{
				Deployments: []reviewapps.Deployments{{
					Name:                "web",
					TargetContainerName: "web",
					TargetContainerPort: 80,
					HostTemplates:       []string{"{{.BranchName}}.example.com"},
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "web", Image: "nginx:1"}},
						},
					},
				}},
			},
		}

		pr := &reviewapps.PullRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "app-feature", Namespace: "default"},
			Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: "app", BranchName: "feature"},
			Status: reviewapps.PullRequestStatus{
				Deployments: map[string]*reviewapps.DeploymentStatus{
					"web": {IsActive: true, LastActive: metav1.Now(), Hostnames: []string{"feature.example.com"}},
				},
			},
		}

		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(reviewApp, pr).
			WithStatusSubresource(&reviewapps.PullRequest{}).
			WithIndex(&reviewapps.PullRequest{}, utils.HostIndexFieldName, func(obj client.Object) []string {
				deployments := obj.(*reviewapps.PullRequest).Status.Deployments
				hosts := make([]string, 0, len(deployments))
				for _, d := range deployments {
					hosts = append(hosts, d.Hostnames...)
				}
				return hosts
			}).
			Build()

		fwd = Forwarder{Client: c, ConnectionTimeout: 5 * time.Second}
	})

	It("re-sends the request body after the connection is refused", func() {
		refusedDials = 2
		upstream(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(w, r.Body)
		})

		req := httptest.NewRequest(http.MethodPost, "http://feature.example.com/", requestBody("hello"))
		rec := httptest.NewRecorder()
		fwd.ServeHTTP(rec, req)

		Expect(rec.Code).To(Equal(http.StatusOK))
		Expect(rec.Body.String()).To(Equal("hello"))
		Expect(dials.Load()).To(BeEquivalentTo(3))
	})

	It("streams the request body instead of buffering it", func() {
		received := make(chan string, 1)
		upstream(func(w http.ResponseWriter, r *http.Request) {
			buf := make([]byte, 5)
			_, _ = io.ReadFull(r.Body, buf)
			received <- string(buf)
			_, _ = io.Copy(io.Discard, r.Body)
		})

		body, bodyWriter := io.Pipe()
		DeferCleanup(func() { _ = bodyWriter.Close() })

		req := httptest.NewRequest(http.MethodPost, "http://feature.example.com/", body)
		go fwd.ServeHTTP(httptest.NewRecorder(), req)

		// The review app receives the start of the body while the client is still sending it
		_, err := bodyWriter.Write([]byte("hello"))
		Expect(err).NotTo(HaveOccurred())
		Eventually(received, 3*time.Second).Should(Receive(Equal("hello")))
	})

	It("does not re-send a request that reached the review app", func() {
		var requests atomic.Int32
		upstream(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)

			// Drop the connection without responding, like a crashing review app
			conn, _, err := http.NewResponseController(w).Hijack()
			Expect(err).NotTo(HaveOccurred())
			_ = conn.Close()
		})

		req := httptest.NewRequest(http.MethodPost, "http://feature.example.com/", requestBody("hello"))
		rec := httptest.NewRecorder()
		fwd.ServeHTTP(rec, req)

		Expect(rec.Code).To(Equal(http.StatusBadGateway))
		Expect(requests.Load()).To(BeEquivalentTo(1))
	})
})

// requestBody returns a request body that like a server request body
// can't be read after it is closed
func requestBody(s string) io.ReadCloser {
	r, w := io.Pipe()
	go func() {
		_, _ = w.Write([]byte(s))
		_ = w.Close()
	}()
	return r
}
