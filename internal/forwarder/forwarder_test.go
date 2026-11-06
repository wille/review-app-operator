package forwarder

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is idiomatic for Ginkgo specs
	. "github.com/onsi/gomega"    //nolint:staticcheck // dot-import is idiomatic for Gomega matchers
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/metrics"
	"github.com/wille/review-app-operator/internal/utils"
)

func TestForwarder(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Forwarder")
}

var _ = Describe("Forwarder", func() {
	var (
		fwd Forwarder

		// pullRequest returns the PullRequest of the review app, and newClient a client that has it
		pullRequest func() *reviewapps.PullRequest
		newClient   func(funcs interceptor.Funcs, objs ...client.Object) client.Client

		// The number of connection attempts to the review app that are refused
		// before connecting, like while it is scaling up
		refusedDials atomic.Int32
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
				if dials.Add(1) <= refusedDials.Load() {
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
		refusedDials.Store(0)
		renewals = newRenewThrottle()
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

		pullRequest = func() *reviewapps.PullRequest {
			return &reviewapps.PullRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "app-feature", Namespace: "default"},
				Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: "app", BranchName: "feature"},
				Status: reviewapps.PullRequestStatus{
					Deployments: map[string]*reviewapps.DeploymentStatus{
						"web": {IsActive: true, LastActive: metav1.Now(), Hostnames: []string{"feature.example.com"}},
					},
				},
			}
		}

		newClient = func(funcs interceptor.Funcs, objs ...client.Object) client.Client {
			return fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(append(objs, reviewApp.DeepCopy())...).
				WithStatusSubresource(&reviewapps.PullRequest{}).
				WithIndex(&reviewapps.PullRequest{}, utils.HostIndexFieldName, func(obj client.Object) []string {
					deployments := obj.(*reviewapps.PullRequest).Status.Deployments
					hosts := make([]string, 0, len(deployments))
					for _, d := range deployments {
						hosts = append(hosts, d.Hostnames...)
					}
					return hosts
				}).
				WithInterceptorFuncs(funcs).
				Build()
		}

		fwd = Forwarder{Client: newClient(interceptor.Funcs{}, pullRequest()), ConnectionTimeout: 5 * time.Second}
	})

	It("re-sends the request body after the connection is refused", func() {
		refusedDials.Store(2)
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

	It("records the responses of the review app, and how long requests waited for it to start", func() {
		refusedDials.Store(2)
		upstream(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
		responses := metrics.Requests.WithLabelValues("app", "web", "418")
		wait := metrics.StartWait.WithLabelValues("app", "web")
		requestsBefore, waitBefore := testutil.ToFloat64(responses), histogramCount(wait)

		rec := httptest.NewRecorder()
		fwd.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://feature.example.com/", nil))

		Expect(rec.Code).To(Equal(http.StatusTeapot))
		Expect(testutil.ToFloat64(responses) - requestsBefore).To(BeEquivalentTo(1))
		Expect(histogramCount(wait) - waitBefore).To(BeEquivalentTo(1))

		// A review app that accepts the connection right away doesn't make the request wait
		fwd.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://feature.example.com/", nil))
		Expect(testutil.ToFloat64(responses) - requestsBefore).To(BeEquivalentTo(2))
		Expect(histogramCount(wait) - waitBefore).To(BeEquivalentTo(1))
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

	Context("when recording visits", func() {
		var renewals atomic.Int32

		BeforeEach(func() {
			renewals.Store(0)
			upstream(func(w http.ResponseWriter, r *http.Request) {})
		})

		// The Lease that the controller creates for the deployment of the review app
		lease := func() *coordinationv1.Lease {
			return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "app-web-feature", Namespace: "default"}}
		}

		// countRenewals counts the Lease renewals before renew makes them
		countRenewals := func(renew func(n int32) error) interceptor.Funcs {
			return interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*coordinationv1.Lease); ok {
						if err := renew(renewals.Add(1)); err != nil {
							return err
						}
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					Fail("the forwarder writes the visits to the Lease, not the PullRequest")
					return nil
				},
			}
		}
		succeed := func(int32) error { return nil }

		renewed := func(c client.Client) *coordinationv1.Lease {
			stored := lease()
			Expect(c.Get(context.Background(), client.ObjectKeyFromObject(stored), stored)).To(Succeed())
			return stored
		}

		// visit sends requests to the review app at the same time
		visit := func(requests int) {
			var wg sync.WaitGroup
			for range requests {
				wg.Go(func() {
					fwd.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://feature.example.com/", nil))
				})
			}
			wg.Wait()
		}

		It("renews the Lease of the visited deployment", func() {
			c := newClient(countRenewals(succeed), pullRequest(), lease())
			fwd.Client = c

			visit(1)

			Eventually(func(g Gomega) {
				stored := renewed(c)
				g.Expect(stored.Spec.RenewTime).NotTo(BeNil())
				g.Expect(stored.Spec.RenewTime.Time).To(BeTemporally("~", time.Now(), 5*time.Second))
				g.Expect(stored.Spec.HolderIdentity).To(HaveValue(Equal(holderIdentity)))
			}).Should(Succeed())
		})

		It("renews the Lease once a renewal interval, also with many visits at once", func() {
			fwd.Client = newClient(countRenewals(succeed), pullRequest(), lease())

			visit(20)
			Eventually(renewals.Load).Should(BeEquivalentTo(1))

			visit(20)
			Consistently(renewals.Load, 500*time.Millisecond).Should(BeEquivalentTo(1))
		})

		It("renews the Lease again when renewing it failed", func() {
			c := newClient(countRenewals(func(n int32) error {
				if n == 1 {
					return errors.New("the API server is unavailable")
				}
				return nil
			}), pullRequest(), lease())
			fwd.Client = c

			visit(1)
			Eventually(renewals.Load).Should(BeEquivalentTo(1))

			visit(1)
			Eventually(renewals.Load).Should(BeEquivalentTo(2))
			Eventually(func() *metav1.MicroTime { return renewed(c).Spec.RenewTime }).ShouldNot(BeNil())
		})

		It("forwards requests without waiting for the Lease to be renewed", func() {
			renew := make(chan struct{})
			DeferCleanup(func() { close(renew) })

			fwd.Client = newClient(countRenewals(func(int32) error {
				<-renew
				return nil
			}), pullRequest(), lease())

			done := make(chan int, 1)
			go func() {
				rec := httptest.NewRecorder()
				fwd.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://feature.example.com/", nil))
				done <- rec.Code
			}()

			Eventually(done, 3*time.Second).Should(Receive(Equal(http.StatusOK)))
			Expect(renewals.Load()).To(BeEquivalentTo(1))
		})
	})

	DescribeTable("renews Leases often enough for deployments not to scale down while they are visited",
		func(scaleDownAfter, interval time.Duration) {
			Expect(renewInterval(scaleDownAfter)).To(Equal(interval))
		},
		Entry("a tenth of scaleDownAfter", 10*time.Minute, time.Minute),
		Entry("at most every 5 minutes", 24*time.Hour, 5*time.Minute),
		Entry("at least 5 seconds apart", 10*time.Second, 5*time.Second),
		Entry("every 5 minutes when deployments never scale down", time.Duration(0), 5*time.Minute),
	)

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
			refusedDials.Store(math.MaxInt32)
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

		It("records the requests that the review app didn't start in time for", func() {
			timeouts := metrics.StartTimeouts.WithLabelValues("app", "web")
			responses := metrics.Requests.WithLabelValues("app", "web", "503")
			timeoutsBefore, responsesBefore := testutil.ToFloat64(timeouts), testutil.ToFloat64(responses)

			fwd.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://feature.example.com/api", nil))

			Expect(testutil.ToFloat64(timeouts) - timeoutsBefore).To(BeEquivalentTo(1))
			Expect(testutil.ToFloat64(responses) - responsesBefore).To(BeEquivalentTo(1))
		})

		It("waits for the connection timeout of the ReviewAppConfig instead", func() {
			fwd.ConnectionTimeout = time.Minute

			var reviewApp reviewapps.ReviewAppConfig
			Expect(fwd.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "app"}, &reviewApp)).To(Succeed())
			reviewApp.Spec.ConnectionTimeout = metav1.Duration{Duration: time.Second}
			Expect(fwd.Update(context.Background(), &reviewApp)).To(Succeed())

			req := httptest.NewRequest(http.MethodGet, "http://feature.example.com/api", nil)
			rec := httptest.NewRecorder()
			start := time.Now()
			fwd.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
			Expect(time.Since(start)).To(BeNumerically("~", time.Second, 2*time.Second))
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

// histogramCount returns how many values a histogram observed
func histogramCount(observer prometheus.Observer) uint64 {
	var metric dto.Metric
	Expect(observer.(prometheus.Histogram).Write(&metric)).To(Succeed())
	return metric.GetHistogram().GetSampleCount()
}
