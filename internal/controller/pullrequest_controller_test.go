/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/utils"
)

var _ = Describe("PullRequest Controller", func() {
	Context("When reconciling a PullRequest with a valid ReviewAppConfig", func() {
		const reviewAppName = "pr-test-reviewapp"
		const prName = "pr-test-pullrequest"
		const namespace = "default"
		const branchName = "feature/test-branch"
		const expectedDeploymentName = "pr-test-reviewapp-web-feature-test-branch"

		reviewAppNN := types.NamespacedName{Name: reviewAppName, Namespace: namespace}
		prNN := types.NamespacedName{Name: prName, Namespace: namespace}
		deployNN := types.NamespacedName{Name: expectedDeploymentName, Namespace: namespace}

		BeforeEach(func() {
			By("creating the ReviewAppConfig")
			reviewApp := &reviewapps.ReviewAppConfig{}
			err := k8sClient.Get(ctx, reviewAppNN, reviewApp)
			if err != nil && errors.IsNotFound(err) {
				resource := &reviewapps.ReviewAppConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name:      reviewAppName,
						Namespace: namespace,
					},
					Spec: reviewapps.ReviewAppConfigSpec{
						StartOnDeploy: true,
						Deployments: []reviewapps.Deployments{
							{
								Name:                "web",
								TargetContainerName: "app",
								TargetContainerPort: 8080,
								Replicas:            1,
								HostTemplates:       []string{"{{.BranchName}}.test.example.com"},
								Template: corev1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{
												Name:  "app",
												Image: "nginx:latest",
											},
										},
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}

			By("creating the PullRequest")
			pr := &reviewapps.PullRequest{}
			err = k8sClient.Get(ctx, prNN, pr)
			if err != nil && errors.IsNotFound(err) {
				resource := &reviewapps.PullRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      prName,
						Namespace: namespace,
					},
					Spec: reviewapps.PullRequestSpec{
						ReviewAppConfigRef: reviewAppName,
						BranchName:         branchName,
						ImageName:          "nginx:test",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			pr := &reviewapps.PullRequest{}
			if err := k8sClient.Get(ctx, prNN, pr); err == nil {
				Expect(k8sClient.Delete(ctx, pr)).To(Succeed())
				Eventually(func() bool {
					return errors.IsNotFound(k8sClient.Get(ctx, prNN, &reviewapps.PullRequest{}))
				}, 10*time.Second, 250*time.Millisecond).Should(BeTrue())
			}

			// envtest runs no garbage collector to delete what the PullRequest owns
			objectMeta := metav1.ObjectMeta{Name: expectedDeploymentName, Namespace: namespace}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: objectMeta}))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: objectMeta}))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &coordinationv1.Lease{ObjectMeta: objectMeta}))).To(Succeed())

			reviewApp := &reviewapps.ReviewAppConfig{}
			if err := k8sClient.Get(ctx, reviewAppNN, reviewApp); err == nil {
				Expect(k8sClient.Delete(ctx, reviewApp)).To(Succeed())
				Eventually(func() bool {
					return errors.IsNotFound(k8sClient.Get(ctx, reviewAppNN, &reviewapps.ReviewAppConfig{}))
				}, 10*time.Second, 250*time.Millisecond).Should(BeTrue())
			}
		})

		It("should create a Deployment for the PullRequest", func() {
			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
				g.Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		})

		It("should create a Service for the PullRequest", func() {
			Eventually(func(g Gomega) {
				svc := &corev1.Service{}
				g.Expect(k8sClient.Get(ctx, deployNN, svc)).To(Succeed())
				g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(80)))
				g.Expect(svc.Spec.Ports[0].TargetPort.IntValue()).To(Equal(8080))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		})

		It("should update the PullRequest status with hostnames", func() {
			Eventually(func(g Gomega) {
				pr := &reviewapps.PullRequest{}
				g.Expect(k8sClient.Get(ctx, prNN, pr)).To(Succeed())
				g.Expect(pr.Status.Deployments).To(HaveKey("web"))
				g.Expect(pr.Status.Deployments["web"].Hostnames).To(ContainElement("feature-test-branch.test.example.com"))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		})

		It("should record the URL of the review app on the PullRequest status", func() {
			Eventually(func(g Gomega) {
				pr := &reviewapps.PullRequest{}
				g.Expect(k8sClient.Get(ctx, prNN, pr)).To(Succeed())
				g.Expect(pr.Status.URL).To(Equal("https://feature-test-branch.test.example.com"))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		})

		It("should set the container image from the PullRequest spec", func() {
			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
				found := false
				for _, c := range deploy.Spec.Template.Spec.Containers {
					if c.Name == "app" {
						g.Expect(c.Image).To(Equal("nginx:test"))
						found = true
					}
				}
				g.Expect(found).To(BeTrue(), "container 'app' not found in deployment")
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})

	Context("When reconciling a PullRequest that is not started on deploy", func() {
		const namespace = "default"

		var reviewApp *reviewapps.ReviewAppConfig
		var pr *reviewapps.PullRequest

		BeforeEach(func() {
			reviewApp = &reviewapps.ReviewAppConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "inactive", Namespace: namespace},
				Spec: reviewapps.ReviewAppConfigSpec{
					Deployments: []reviewapps.Deployments{{
						Name:                "web",
						TargetContainerName: "app",
						TargetContainerPort: 8080,
						Replicas:            2,
						HostTemplates:       []string{"{{.BranchName}}.inactive.example.com"},
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
							},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, reviewApp)).To(Succeed())

			pr = &reviewapps.PullRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "inactive-feature", Namespace: namespace},
				Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: "inactive", BranchName: "feature", ImageName: "nginx:test"},
			}
		})

		AfterEach(func() {
			// envtest runs no garbage collector to delete what the PullRequest owns
			objectMeta := metav1.ObjectMeta{Name: utils.GetDeploymentName(reviewApp, pr, "web"), Namespace: namespace}
			for _, obj := range []client.Object{pr, reviewApp, &appsv1.Deployment{ObjectMeta: objectMeta}, &corev1.Service{ObjectMeta: objectMeta}, &coordinationv1.Lease{ObjectMeta: objectMeta}} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		It("creates the deployment scaled down", func() {
			Expect(k8sClient.Create(ctx, pr)).To(Succeed())

			// A deployment created with replicas and scaled down right after would have a new generation
			deployNN := types.NamespacedName{Name: utils.GetDeploymentName(reviewApp, pr, "web"), Namespace: namespace}
			Eventually(func() error {
				return k8sClient.Get(ctx, deployNN, &appsv1.Deployment{})
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
			Consistently(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
				g.Expect(*deploy.Spec.Replicas).To(BeZero())
				g.Expect(deploy.Generation).To(BeEquivalentTo(1))
			}, 2*time.Second, 250*time.Millisecond).Should(Succeed())
		})

		It("creates the deployment with the template image when the PullRequest has no image", func() {
			pr.Spec.ImageName = ""
			Expect(k8sClient.Create(ctx, pr)).To(Succeed())

			deployNN := types.NamespacedName{Name: utils.GetDeploymentName(reviewApp, pr, "web"), Namespace: namespace}
			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
				g.Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:latest"))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})

	Context("When a host template has a random ID", func() {
		const namespace = "default"

		var reviewApp *reviewapps.ReviewAppConfig
		var pr *reviewapps.PullRequest

		BeforeEach(func() {
			reviewApp = &reviewapps.ReviewAppConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "random", Namespace: namespace},
				Spec: reviewapps.ReviewAppConfigSpec{
					Deployments: []reviewapps.Deployments{{
						Name:                "web",
						TargetContainerName: "app",
						TargetContainerPort: 8080,
						HostTemplates: []string{
							"{{.RandomID}}.random.example.com",
							"mobile-{{.BranchName}}-{{.RandomID}}.random.example.com",
						},
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
							},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, reviewApp)).To(Succeed())

			pr = &reviewapps.PullRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "random-feature", Namespace: namespace},
				Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: "random", BranchName: "feature/login", ImageName: "nginx:test"},
			}
		})

		AfterEach(func() {
			// envtest runs no garbage collector to delete what the PullRequest owns
			objectMeta := metav1.ObjectMeta{Name: utils.GetDeploymentName(reviewApp, pr, "web"), Namespace: namespace}
			for _, obj := range []client.Object{pr, reviewApp, &appsv1.Deployment{ObjectMeta: objectMeta}, &corev1.Service{ObjectMeta: objectMeta}, &coordinationv1.Lease{ObjectMeta: objectMeta}} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		It("records the hostnames with the random ID from the PullRequest UID", func() {
			Expect(k8sClient.Create(ctx, pr)).To(Succeed())

			// The first 12 hex digits of the UID the API server generated
			id := strings.ReplaceAll(string(pr.UID), "-", "")[:12]

			Eventually(func(g Gomega) {
				var got reviewapps.PullRequest
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pr), &got)).To(Succeed())
				g.Expect(got.Status.Deployments).To(HaveKey("web"))
				g.Expect(got.Status.Deployments["web"].Hostnames).To(ConsistOf(
					id+".random.example.com",
					"mobile-feature-login-"+id+".random.example.com",
				))

				condition := meta.FindStatusCondition(got.Status.Conditions, reviewapps.ConditionReconciled)
				g.Expect(condition).NotTo(BeNil())
				g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(condition.ObservedGeneration).To(Equal(got.Generation))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})

	Context("When a PullRequest can't be reconciled", func() {
		const namespace = "default"

		var reviewApp *reviewapps.ReviewAppConfig
		var pr *reviewapps.PullRequest

		newReviewApp := func(hostTemplate string) *reviewapps.ReviewAppConfig {
			return &reviewapps.ReviewAppConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "failing", Namespace: namespace},
				Spec: reviewapps.ReviewAppConfigSpec{
					Deployments: []reviewapps.Deployments{{
						Name:                "web",
						TargetContainerName: "app",
						TargetContainerPort: 8080,
						HostTemplates:       []string{hostTemplate},
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
							},
						},
					}},
				},
			}
		}

		BeforeEach(func() {
			pr = &reviewapps.PullRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "failing-feature", Namespace: namespace},
				Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: "failing", BranchName: "feature", ImageName: "nginx:test"},
			}
		})

		AfterEach(func() {
			for _, obj := range []client.Object{pr, &reviewapps.ReviewAppConfig{ObjectMeta: metav1.ObjectMeta{Name: "failing", Namespace: namespace}}} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		expectCondition := func(reason, message string) {
			Eventually(func(g Gomega) {
				var got reviewapps.PullRequest
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pr), &got)).To(Succeed())

				condition := meta.FindStatusCondition(got.Status.Conditions, reviewapps.ConditionReconciled)
				g.Expect(condition).NotTo(BeNil())
				g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(condition.Reason).To(Equal(reason))
				g.Expect(condition.Message).To(ContainSubstring(message))
				g.Expect(condition.ObservedGeneration).To(Equal(got.Generation))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		}

		It("records an invalid host template on the PullRequest", func() {
			// Valid for the CRD, but too long for a DNS label with the random ID
			reviewApp = newReviewApp(strings.Repeat("a", 60) + "-{{.RandomID}}.example.com")
			Expect(k8sClient.Create(ctx, reviewApp)).To(Succeed())
			Expect(k8sClient.Create(ctx, pr)).To(Succeed())

			expectCondition("InvalidHostTemplate", "too long for a DNS label")
		})

		It("records a missing ReviewAppConfig on the PullRequest", func() {
			Expect(k8sClient.Create(ctx, pr)).To(Succeed())

			expectCondition("ReviewAppConfigNotFound", "ReviewAppConfig not found: failing")
		})

		It("rejects host templates with unknown variables or without the branch name or random ID", func() {
			for _, template := range []string{
				"web-{{.RandomId}}.example.com",
				"web-{{.BranchName}}-{{.Unknown}}.example.com",
				"web.example.com",
				"Web-{{.BranchName}}.example.com",
			} {
				err := k8sClient.Create(ctx, newReviewApp(template))
				Expect(errors.IsInvalid(err)).To(BeTrue(), template)
			}
		})
	})

	Context("When the ReviewAppConfig of a review app changes", func() {
		const namespace = "default"

		var reviewApp *reviewapps.ReviewAppConfig
		var pr *reviewapps.PullRequest
		var deployNN types.NamespacedName

		BeforeEach(func() {
			reviewApp = &reviewapps.ReviewAppConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "changing", Namespace: namespace},
				Spec: reviewapps.ReviewAppConfigSpec{
					Deployments: []reviewapps.Deployments{{
						Name:                "web",
						TargetContainerName: "app",
						TargetContainerPort: 8080,
						HostTemplates:       []string{"{{.BranchName}}.changing.example.com"},
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
							},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, reviewApp)).To(Succeed())

			pr = &reviewapps.PullRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "changing-feature", Namespace: namespace},
				Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: "changing", BranchName: "feature", ImageName: "nginx:test"},
			}
			Expect(k8sClient.Create(ctx, pr)).To(Succeed())

			deployNN = types.NamespacedName{Name: utils.GetDeploymentName(reviewApp, pr, "web"), Namespace: namespace}
			Eventually(func() error {
				return k8sClient.Get(ctx, deployNN, &appsv1.Deployment{})
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		})

		AfterEach(func() {
			// envtest runs no garbage collector to delete what the PullRequest owns
			objectMeta := metav1.ObjectMeta{Name: deployNN.Name, Namespace: namespace}
			for _, obj := range []client.Object{pr, reviewApp, &appsv1.Deployment{ObjectMeta: objectMeta}, &corev1.Service{ObjectMeta: objectMeta}, &coordinationv1.Lease{ObjectMeta: objectMeta}} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		// updateReviewApp changes the ReviewAppConfig, retrying when the controller updated it first
		updateReviewApp := func(change func(*reviewapps.ReviewAppConfig)) {
			Eventually(func() error {
				var latest reviewapps.ReviewAppConfig
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(reviewApp), &latest); err != nil {
					return err
				}
				change(&latest)
				return k8sClient.Update(ctx, &latest)
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		}

		// pushImage deploys a new image to the review app like the webhook does on a push
		pushImage := func(image string) {
			Eventually(func() error {
				var latest reviewapps.PullRequest
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pr), &latest); err != nil {
					return err
				}
				latest.Spec.ImageName = image
				return k8sClient.Update(ctx, &latest)
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		}

		It("rolls out the change with the next image that is pushed", func() {
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
			generation := deploy.Generation

			updateReviewApp(func(reviewApp *reviewapps.ReviewAppConfig) {
				reviewApp.Spec.Deployments[0].Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "FEATURE_FLAG", Value: "on"}}
			})

			// Changing the ReviewAppConfig doesn't restart every review app at once
			Consistently(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
				g.Expect(deploy.Generation).To(Equal(generation))
			}, 2*time.Second, 250*time.Millisecond).Should(Succeed())

			pushImage("nginx:test2")

			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
				container := deploy.Spec.Template.Spec.Containers[0]
				g.Expect(container.Image).To(Equal("nginx:test2"))
				g.Expect(container.Env).To(ConsistOf(corev1.EnvVar{Name: "FEATURE_FLAG", Value: "on"}))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})

	Context("When a PullRequest overrides a deployment", func() {
		const namespace = "default"

		var reviewApp *reviewapps.ReviewAppConfig
		var pr *reviewapps.PullRequest
		var deployNN types.NamespacedName

		apiURL := corev1.EnvVar{Name: "API_URL", Value: "https://api.example.com"}
		logLevel := corev1.EnvVar{Name: "LOG_LEVEL", Value: "info"}

		// overrides returns overrides of the web deployment with environment variables
		overrides := func(env ...reviewapps.EnvVar) []reviewapps.DeploymentOverrides {
			return []reviewapps.DeploymentOverrides{{Name: "web", Env: env}}
		}

		BeforeEach(func() {
			reviewApp = &reviewapps.ReviewAppConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "overridden", Namespace: namespace},
				Spec: reviewapps.ReviewAppConfigSpec{
					Deployments: []reviewapps.Deployments{{
						Name:                "web",
						TargetContainerName: "app",
						TargetContainerPort: 8080,
						HostTemplates:       []string{"{{.BranchName}}.overridden.example.com"},
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "app", Image: "nginx:latest", Env: []corev1.EnvVar{apiURL, logLevel}},
									{Name: "sidecar", Image: "redis:8", Env: []corev1.EnvVar{apiURL}},
								},
							},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, reviewApp)).To(Succeed())

			pr = &reviewapps.PullRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "overridden-feature", Namespace: namespace},
				Spec: reviewapps.PullRequestSpec{
					ReviewAppConfigRef: "overridden",
					BranchName:         "feature",
					ImageName:          "nginx:test",
					Deployments: overrides(
						reviewapps.EnvVar{Name: "API_URL", Value: "https://api-feature.example.com"},
						reviewapps.EnvVar{Name: "FEATURE_FLAG", Value: "on"},
					),
				},
			}
			Expect(k8sClient.Create(ctx, pr)).To(Succeed())

			deployNN = types.NamespacedName{Name: utils.GetDeploymentName(reviewApp, pr, "web"), Namespace: namespace}
		})

		AfterEach(func() {
			// envtest runs no garbage collector to delete what the PullRequest owns
			objectMeta := metav1.ObjectMeta{Name: deployNN.Name, Namespace: namespace}
			for _, obj := range []client.Object{pr, reviewApp, &appsv1.Deployment{ObjectMeta: objectMeta}, &corev1.Service{ObjectMeta: objectMeta}, &coordinationv1.Lease{ObjectMeta: objectMeta}} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		// updatePullRequest changes the PullRequest, retrying when the controller updated it first
		updatePullRequest := func(change func(*reviewapps.PullRequest)) {
			Eventually(func() error {
				var latest reviewapps.PullRequest
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pr), &latest); err != nil {
					return err
				}
				change(&latest)
				return k8sClient.Update(ctx, &latest)
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		}

		// expectEnv waits for the target container of the deployment to have the environment variables
		expectEnv := func(env ...corev1.EnvVar) {
			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
				g.Expect(deploy.Spec.Template.Spec.Containers[0].Env).To(Equal(env))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		}

		It("sets the environment variables of the overrides in the target container", func() {
			expectEnv(
				corev1.EnvVar{Name: "API_URL", Value: "https://api-feature.example.com"},
				logLevel,
				corev1.EnvVar{Name: "FEATURE_FLAG", Value: "on"},
			)

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
			Expect(deploy.Spec.Template.Spec.Containers[1].Env).To(Equal([]corev1.EnvVar{apiURL}))
			Expect(deploy.Annotations).To(HaveKey("reviewapps.william.nu/overrides-hash"))
		})

		It("keeps the overrides when a new image is pushed", func() {
			expectEnv(
				corev1.EnvVar{Name: "API_URL", Value: "https://api-feature.example.com"},
				logLevel,
				corev1.EnvVar{Name: "FEATURE_FLAG", Value: "on"},
			)

			updatePullRequest(func(pr *reviewapps.PullRequest) { pr.Spec.ImageName = "nginx:test2" })

			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
				g.Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:test2"))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
			expectEnv(
				corev1.EnvVar{Name: "API_URL", Value: "https://api-feature.example.com"},
				logLevel,
				corev1.EnvVar{Name: "FEATURE_FLAG", Value: "on"},
			)
		})

		It("applies changes to the overrides right away, and reverts the removed ones", func() {
			expectEnv(
				corev1.EnvVar{Name: "API_URL", Value: "https://api-feature.example.com"},
				logLevel,
				corev1.EnvVar{Name: "FEATURE_FLAG", Value: "on"},
			)

			updatePullRequest(func(pr *reviewapps.PullRequest) {
				pr.Spec.Deployments = overrides(reviewapps.EnvVar{Name: "API_URL", Value: "https://api-other.example.com"})
			})
			expectEnv(corev1.EnvVar{Name: "API_URL", Value: "https://api-other.example.com"}, logLevel)

			updatePullRequest(func(pr *reviewapps.PullRequest) { pr.Spec.Deployments = nil })
			expectEnv(apiURL, logLevel)

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
			Expect(deploy.Annotations).NotTo(HaveKey("reviewapps.william.nu/overrides-hash"))
			Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:test"))
		})

		It("reports overrides of a deployment that the ReviewAppConfig doesn't have", func() {
			updatePullRequest(func(pr *reviewapps.PullRequest) {
				pr.Spec.Deployments = append(pr.Spec.Deployments, reviewapps.DeploymentOverrides{
					Name: "wbe",
					Env:  []reviewapps.EnvVar{{Name: "API_URL", Value: "https://api-other.example.com"}},
				})
			})

			Eventually(func(g Gomega) {
				var got reviewapps.PullRequest
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pr), &got)).To(Succeed())

				condition := meta.FindStatusCondition(got.Status.Conditions, reviewapps.ConditionReconciled)
				g.Expect(condition).NotTo(BeNil())
				g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(condition.Reason).To(Equal("UnknownDeployment"))
				g.Expect(condition.Message).To(ContainSubstring(`"wbe"`))
				g.Expect(condition.ObservedGeneration).To(Equal(got.Generation))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())

			// The deployments that the ReviewAppConfig has keep running with their overrides
			expectEnv(
				corev1.EnvVar{Name: "API_URL", Value: "https://api-feature.example.com"},
				logLevel,
				corev1.EnvVar{Name: "FEATURE_FLAG", Value: "on"},
			)
		})
	})

	Context("When two PullRequests have the same truncated deployment name", func() {
		const namespace = "default"

		// Both branches are truncated to the same deployment name
		const branchA = "dependabot/npm_and_yarn/typescript-eslint/eslint-plugin-8.1.0"
		const branchB = "dependabot/npm_and_yarn/typescript-eslint/eslint-plugin-8.2.0"

		var reviewApp *reviewapps.ReviewAppConfig
		var prA, prB *reviewapps.PullRequest

		newPullRequest := func(name, branchName, image string) *reviewapps.PullRequest {
			return &reviewapps.PullRequest{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: "trunc", BranchName: branchName, ImageName: image},
			}
		}

		BeforeEach(func() {
			reviewApp = &reviewapps.ReviewAppConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "trunc", Namespace: namespace},
				Spec: reviewapps.ReviewAppConfigSpec{
					Deployments: []reviewapps.Deployments{{
						Name:                "web",
						TargetContainerName: "app",
						TargetContainerPort: 8080,
						HostTemplates:       []string{"{{.BranchName}}.trunc.example.com"},
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
							},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, reviewApp)).To(Succeed())

			prA = newPullRequest("trunc-a", branchA, "nginx:a")
			prB = newPullRequest("trunc-b", branchB, "nginx:b")
		})

		AfterEach(func() {
			// envtest runs no garbage collector to delete what the PullRequests own
			objectMeta := metav1.ObjectMeta{Name: utils.GetDeploymentName(reviewApp, prA, "web"), Namespace: namespace}
			for _, obj := range []client.Object{prA, prB, reviewApp, &appsv1.Deployment{ObjectMeta: objectMeta}, &corev1.Service{ObjectMeta: objectMeta}, &coordinationv1.Lease{ObjectMeta: objectMeta}} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		It("does not take over the deployment of the other PullRequest", func() {
			deploymentName := utils.GetDeploymentName(reviewApp, prA, "web")
			Expect(utils.GetDeploymentName(reviewApp, prB, "web")).To(Equal(deploymentName))
			deployNN := types.NamespacedName{Name: deploymentName, Namespace: namespace}

			Expect(k8sClient.Create(ctx, prA)).To(Succeed())
			deploy := &appsv1.Deployment{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
				g.Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:a"))
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
			uid, generation := deploy.UID, deploy.Generation

			// A change by the other PullRequest is reverted right away, either by a new rollout
			// or by recreating the deployment, so check that the deployment is left untouched
			Expect(k8sClient.Create(ctx, prB)).To(Succeed())
			Consistently(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, deployNN, deploy)).To(Succeed())
				g.Expect(deploy.UID).To(Equal(uid))
				g.Expect(deploy.Generation).To(Equal(generation))
				g.Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:a"))
				g.Expect(metav1.GetControllerOf(deploy).Name).To(Equal("trunc-a"))
			}, 3*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})
})
