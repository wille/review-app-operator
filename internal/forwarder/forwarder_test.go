package forwarder

import (
	"bufio"
	"context"
	"io"
	"math"
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

		defaultTransport := transport
		DeferCleanup(func() { transport = defaultTransport })

		transport = &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				addr := server.Listener.Addr().String()
				if dials.Add(1) <= refusedDials {
					addr = closed.Addr().String()
				}
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}
	}

	// serve runs the forwarder on a server, for what needs a real connection to the client
	serve := func() *httptest.Server {
		server := httptest.NewServer(fwd)
		DeferCleanup(server.Close)
		return server
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

	It("forwards the host and the forwarded headers of the ingress", func() {
		received := make(chan *http.Request, 1)
		upstream(func(w http.ResponseWriter, r *http.Request) {
			received <- r
		})

		req := httptest.NewRequest(http.MethodGet, "http://feature.example.com/", nil)
		req.RemoteAddr = "10.0.0.2:1234"
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "feature.example.com")
		fwd.ServeHTTP(httptest.NewRecorder(), req)

		var forwarded *http.Request
		Eventually(received).Should(Receive(&forwarded))
		Expect(forwarded.Host).To(Equal("feature.example.com"))
		Expect(forwarded.Header.Get("X-Forwarded-For")).To(Equal("203.0.113.7, 10.0.0.2"))
		Expect(forwarded.Header.Get("X-Forwarded-Proto")).To(Equal("https"))
		Expect(forwarded.Header.Get("X-Forwarded-Host")).To(Equal("feature.example.com"))
	})

	It("passes WebSocket upgrades through", func() {
		upstream(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Upgrade") != "echo" {
				http.Error(w, "Upgrade required", http.StatusUpgradeRequired)
				return
			}

			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()

			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
			_ = rw.Flush()
			_, _ = io.Copy(conn, rw.Reader)
		})
		server := serve()

		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(conn.Close)
		Expect(conn.SetDeadline(time.Now().Add(5 * time.Second))).To(Succeed())

		_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: feature.example.com\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n"))
		Expect(err).NotTo(HaveOccurred())

		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusSwitchingProtocols))

		_, err = conn.Write([]byte("ping"))
		Expect(err).NotTo(HaveOccurred())

		echo := make([]byte, 4)
		_, err = io.ReadFull(reader, echo)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(echo)).To(Equal("ping"))
	})

	DescribeTable("streams responses as the review app sends them",
		func(contentType string) {
			upstream(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				_, _ = w.Write([]byte("first\n"))
				_ = http.NewResponseController(w).Flush()

				// Keep the response open, like a stream that goes on
				<-r.Context().Done()
			})
			server := serve()

			req, err := http.NewRequest(http.MethodGet, server.URL, nil)
			Expect(err).NotTo(HaveOccurred())
			req.Host = "feature.example.com"

			ctx, cancel := context.WithCancel(context.Background())
			DeferCleanup(cancel)

			received := make(chan string, 1)
			go func() {
				defer GinkgoRecover()

				resp, err := http.DefaultClient.Do(req.WithContext(ctx))
				if err != nil {
					return
				}
				defer func() { _ = resp.Body.Close() }()

				line, _ := bufio.NewReader(resp.Body).ReadString('\n')
				received <- line
			}()

			Eventually(received, 3*time.Second).Should(Receive(Equal("first\n")))
		},
		Entry("server-sent events", "text/event-stream"),
		Entry("streamed HTML", "text/html; charset=utf-8"),
	)

	Context("when the review app doesn't start within the connection timeout", func() {
		BeforeEach(func() {
			refusedDials = math.MaxInt32
			fwd.ConnectionTimeout = time.Second
			upstream(func(w http.ResponseWriter, r *http.Request) {})
		})

		It("shows browsers a page that reloads until it has started", func() {
			req := httptest.NewRequest(http.MethodGet, "http://feature.example.com/", nil)
			req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
			rec := httptest.NewRecorder()
			fwd.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusAccepted))
			Expect(rec.Header().Get("Content-Type")).To(HavePrefix("text/html"))
			Expect(rec.Header().Get("Cache-Control")).To(Equal("no-store"))
			Expect(rec.Body.String()).To(ContainSubstring(`http-equiv="refresh"`))
		})

		It("tells other requests to retry later", func() {
			req := httptest.NewRequest(http.MethodPost, "http://feature.example.com/api", requestBody("{}"))
			req.Header.Set("Accept", "application/json")
			rec := httptest.NewRecorder()
			fwd.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
			Expect(rec.Header().Get("Retry-After")).NotTo(BeEmpty())
		})
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
