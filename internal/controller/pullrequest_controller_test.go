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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
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

			Eventually(func(g Gomega) {
				ra := &reviewapps.ReviewAppConfig{}
				g.Expect(k8sClient.Get(ctx, reviewAppNN, ra)).To(Succeed())
				g.Expect(controllerutil.ContainsFinalizer(ra, "reviewapp.william.nu/finalizer")).To(BeTrue())
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())

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
})
