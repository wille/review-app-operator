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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
)

var _ = Describe("ReviewAppConfig Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-reviewapp"
		const namespace = "default"

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: namespace,
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ReviewAppConfig")
			reviewApp := &reviewapps.ReviewAppConfig{}
			err := k8sClient.Get(ctx, typeNamespacedName, reviewApp)
			if err != nil && errors.IsNotFound(err) {
				resource := &reviewapps.ReviewAppConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: namespace,
					},
					Spec: reviewapps.ReviewAppConfigSpec{
						Deployments: []reviewapps.Deployments{
							{
								Name:                "web",
								TargetContainerName: "app",
								TargetContainerPort: 8080,
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
		})

		AfterEach(func() {
			resource := &reviewapps.ReviewAppConfig{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				By("Cleanup the specific resource instance ReviewAppConfig")
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
				Eventually(func() bool {
					return errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &reviewapps.ReviewAppConfig{}))
				}, 10*time.Second, 250*time.Millisecond).Should(BeTrue())
			}
		})

		It("should add a finalizer to the ReviewAppConfig", func() {
			Eventually(func(g Gomega) {
				reviewApp := &reviewapps.ReviewAppConfig{}
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, reviewApp)).To(Succeed())
				g.Expect(controllerutil.ContainsFinalizer(reviewApp, "reviewapp.william.nu/finalizer")).To(BeTrue())
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		})
	})
})
