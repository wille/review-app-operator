package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is idiomatic for Ginkgo specs
	. "github.com/onsi/gomega"    //nolint:staticcheck // dot-import is idiomatic for Gomega matchers
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/utils"
)

var _ = Describe("Activity", func() {
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	leaseRenewedAt := func(renewed time.Time, duration *int32) *coordinationv1.Lease {
		lease := &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{LeaseDurationSeconds: duration}}
		if !renewed.IsZero() {
			t := metav1.NewMicroTime(renewed)
			lease.Spec.RenewTime = &t
		}
		return lease
	}

	DescribeTable("runs a deployment until scaleDownAfter after it was last active",
		func(lastActive time.Time, scaleDownAfter time.Duration, running bool) {
			runs, _ := runsUntil(lastActive, scaleDownAfter, now)
			Expect(runs).To(Equal(running))
		},
		Entry("never active", time.Time{}, time.Hour, false),
		Entry("active within scaleDownAfter", ago(time.Minute), time.Hour, true),
		Entry("active before scaleDownAfter", ago(2*time.Hour), time.Hour, false),
		Entry("active once, without scaleDownAfter", ago(48*time.Hour), time.Duration(0), true),
	)

	It("counts the deploy as activity of the deployments that start on deploy", func() {
		pr := &reviewapps.PullRequest{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(ago(time.Hour))}}
		pr.Status.DeployedAt = metav1.NewTime(ago(time.Minute))
		lease := leaseRenewedAt(ago(30*time.Minute), nil)

		Expect(lastActive(lease, pr, true)).To(BeTemporally("==", ago(time.Minute)))
		Expect(lastActive(lease, pr, false)).To(BeTemporally("==", ago(30*time.Minute)))

		// Pull requests that the webhook did not deploy were deployed when they were created
		pr.Status.DeployedAt = metav1.Time{}
		Expect(lastActive(leaseRenewedAt(time.Time{}, nil), pr, true)).To(BeTemporally("==", ago(time.Hour)))
	})

	Describe("carrying over the activity that earlier versions recorded on the PullRequest", func() {
		pullRequest := func(status *reviewapps.DeploymentStatus) *reviewapps.PullRequest {
			return &reviewapps.PullRequest{Status: reviewapps.PullRequestStatus{
				Deployments: map[string]*reviewapps.DeploymentStatus{"web": status},
			}}
		}

		It("keeps a running deployment running for what is left of scaleDownAfter", func() {
			renewTime := migratedRenewTime(pullRequest(&reviewapps.DeploymentStatus{IsActive: true, LastActive: metav1.NewTime(ago(time.Minute))}), "web", time.Hour, now)
			Expect(renewTime.Time).To(BeTemporally("==", ago(time.Minute)))
		})

		It("keeps a deployment scaled down that was deployed recently without starting", func() {
			renewTime := migratedRenewTime(pullRequest(&reviewapps.DeploymentStatus{LastActive: metav1.NewTime(ago(time.Minute))}), "web", time.Hour, now)
			Expect(leaseHeld(&coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{RenewTime: renewTime, LeaseDurationSeconds: leaseDuration(time.Hour)}}, now)).To(BeFalse())
			Expect(renewTime.Time).To(BeTemporally("==", ago(time.Hour)))
		})

		It("keeps the visit of a scaled down deployment for deleteAfter", func() {
			renewTime := migratedRenewTime(pullRequest(&reviewapps.DeploymentStatus{LastActive: metav1.NewTime(ago(72 * time.Hour))}), "web", time.Hour, now)
			Expect(renewTime.Time).To(BeTemporally("==", ago(72*time.Hour)))
		})

		It("keeps a scaled down deployment that never scales down scaled down", func() {
			Expect(migratedRenewTime(pullRequest(&reviewapps.DeploymentStatus{LastActive: metav1.NewTime(ago(time.Minute))}), "web", 0, now)).To(BeNil())
		})

		It("carries over nothing without activity", func() {
			Expect(migratedRenewTime(pullRequest(&reviewapps.DeploymentStatus{}), "web", time.Hour, now)).To(BeNil())
			Expect(migratedRenewTime(&reviewapps.PullRequest{}, "web", time.Hour, now)).To(BeNil())
		})
	})

	DescribeTable("only reconciles the Lease changes that start a deployment",
		func(old, renewed *coordinationv1.Lease, reconciles bool) {
			Expect(wakeUpPredicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: renewed})).To(Equal(reconciles))
		},
		Entry("the first visit", leaseRenewedAt(time.Time{}, leaseDuration(time.Hour)), leaseRenewedAt(now, leaseDuration(time.Hour)), true),
		Entry("a visit after scaleDownAfter", leaseRenewedAt(ago(2*time.Hour), leaseDuration(time.Hour)), leaseRenewedAt(now, leaseDuration(time.Hour)), true),
		Entry("a visit to a running deployment", leaseRenewedAt(ago(time.Minute), leaseDuration(time.Hour)), leaseRenewedAt(now, leaseDuration(time.Hour)), false),
		Entry("a visit to a deployment that never scales down", leaseRenewedAt(ago(time.Minute), nil), leaseRenewedAt(now, nil), false),
		Entry("a new scaleDownAfter", leaseRenewedAt(ago(time.Minute), leaseDuration(time.Hour)), leaseRenewedAt(ago(time.Minute), leaseDuration(time.Minute)), true),
	)

	Context("in a cluster", func() {
		const namespace = "default"

		// reviewApp creates a review app with a single deployment and returns its Deployment and Lease
		reviewApp := func(name string, configure func(*reviewapps.ReviewAppConfigSpec)) (*reviewapps.PullRequest, types.NamespacedName) {
			reviewApp := &reviewapps.ReviewAppConfig{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: reviewapps.ReviewAppConfigSpec{
					Deployments: []reviewapps.Deployments{{
						Name:                "web",
						TargetContainerName: "app",
						TargetContainerPort: 8080,
						HostTemplates:       []string{"{{.BranchName}}." + name + ".example.com"},
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}}},
						},
					}},
				},
			}
			configure(&reviewApp.Spec)
			Expect(k8sClient.Create(ctx, reviewApp)).To(Succeed())

			pr := &reviewapps.PullRequest{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-feature", Namespace: namespace},
				Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: name, BranchName: "feature", ImageName: "nginx:test"},
			}
			Expect(k8sClient.Create(ctx, pr)).To(Succeed())

			key := types.NamespacedName{Name: utils.GetDeploymentName(reviewApp, pr, "web"), Namespace: namespace}
			DeferCleanup(func() {
				// envtest runs no garbage collector to delete what the PullRequest owns
				objectMeta := metav1.ObjectMeta{Name: key.Name, Namespace: namespace}
				for _, obj := range []client.Object{pr, reviewApp, &appsv1.Deployment{ObjectMeta: objectMeta}, &corev1.Service{ObjectMeta: objectMeta}, &coordinationv1.Lease{ObjectMeta: objectMeta}} {
					Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
				}
			})

			return pr, key
		}

		replicas := func(key types.NamespacedName) func(Gomega) int32 {
			return func(g Gomega) int32 {
				var deployment appsv1.Deployment
				g.Expect(k8sClient.Get(ctx, key, &deployment)).To(Succeed())
				return *deployment.Spec.Replicas
			}
		}

		// visit renews the Lease like the forwarder does when the deployment is visited
		visit := func(key types.NamespacedName) {
			Eventually(func() error {
				lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
				return k8sClient.Patch(ctx, lease, client.RawPatch(types.MergePatchType,
					[]byte(`{"spec":{"renewTime":"`+metav1.NowMicro().UTC().Format(metav1.RFC3339Micro)+`"}}`)))
			}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
		}

		It("records the visits of a deployment in a Lease owned by the pull request", func() {
			pr, key := reviewApp("lease-owner", func(spec *reviewapps.ReviewAppConfigSpec) {
				spec.ScaleDownAfter = metav1.Duration{Duration: time.Hour}
			})

			Eventually(func(g Gomega) {
				var lease coordinationv1.Lease
				g.Expect(k8sClient.Get(ctx, key, &lease)).To(Succeed())
				g.Expect(lease.Spec.LeaseDurationSeconds).To(HaveValue(BeEquivalentTo(3600)))
				g.Expect(lease.Spec.RenewTime).To(BeNil())
				g.Expect(lease.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "review-app-controller"))

				var stored reviewapps.PullRequest
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pr), &stored)).To(Succeed())
				g.Expect(metav1.IsControlledBy(&lease, &stored)).To(BeTrue())
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		})

		It("starts a deployment when it is visited", func() {
			_, key := reviewApp("lease-wake", func(spec *reviewapps.ReviewAppConfigSpec) {
				spec.ScaleDownAfter = metav1.Duration{Duration: time.Hour}
			})
			Eventually(replicas(key), 10*time.Second, 250*time.Millisecond).Should(BeEquivalentTo(0))

			visit(key)

			Eventually(replicas(key), 10*time.Second, 250*time.Millisecond).Should(BeEquivalentTo(1))
		})

		It("scales a deployment down when it is not visited for scaleDownAfter", func() {
			_, key := reviewApp("lease-expire", func(spec *reviewapps.ReviewAppConfigSpec) {
				spec.StartOnDeploy = true
				spec.ScaleDownAfter = metav1.Duration{Duration: 3 * time.Second}
			})
			Eventually(replicas(key), 10*time.Second, 250*time.Millisecond).Should(BeEquivalentTo(1))

			Eventually(replicas(key), 10*time.Second, 250*time.Millisecond).Should(BeEquivalentTo(0))
		})

		It("keeps a deployment running while it is visited", func() {
			_, key := reviewApp("lease-renew", func(spec *reviewapps.ReviewAppConfigSpec) {
				spec.ScaleDownAfter = metav1.Duration{Duration: 3 * time.Second}
			})
			visit(key)
			Eventually(replicas(key), 10*time.Second, 250*time.Millisecond).Should(BeEquivalentTo(1))

			Consistently(func(g Gomega) int32 {
				visit(key)
				return replicas(key)(g)
			}, 6*time.Second, 500*time.Millisecond).Should(BeEquivalentTo(1))
		})

		It("deletes a review app that has neither been deployed nor visited for deleteAfter", func() {
			pr, _ := reviewApp("lease-delete", func(spec *reviewapps.ReviewAppConfigSpec) {
				spec.DeleteAfter = metav1.Duration{Duration: 3 * time.Second}
			})

			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(pr), &reviewapps.PullRequest{}))
			}, 15*time.Second, 250*time.Millisecond).Should(BeTrue())
		})

		It("keeps a review app that is visited", func() {
			pr, key := reviewApp("lease-keep", func(spec *reviewapps.ReviewAppConfigSpec) {
				spec.DeleteAfter = metav1.Duration{Duration: 3 * time.Second}
			})

			Consistently(func(g Gomega) {
				visit(key)
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pr), &reviewapps.PullRequest{})).To(Succeed())
			}, 6*time.Second, 500*time.Millisecond).Should(Succeed())
		})
	})
})
