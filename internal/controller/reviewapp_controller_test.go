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
	"sigs.k8s.io/controller-runtime/pkg/client"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
)

var _ = Describe("ReviewAppConfig Controller", func() {
	const namespace = "default"

	reviewAppNamed := func(name string, finalizers ...string) *reviewapps.ReviewAppConfig {
		return &reviewapps.ReviewAppConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
				Namespace:  namespace,
				Finalizers: finalizers,
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
	}

	// create creates the ReviewAppConfig and deletes it after the spec
	create := func(reviewApp *reviewapps.ReviewAppConfig) types.NamespacedName {
		Expect(k8sClient.Create(ctx, reviewApp)).To(Succeed())

		key := client.ObjectKeyFromObject(reviewApp)
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &reviewapps.ReviewAppConfig{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			}))).To(Succeed())
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, key, &reviewapps.ReviewAppConfig{}))
			}, 10*time.Second, 250*time.Millisecond).Should(BeTrue())
		})

		return key
	}

	It("does not add a finalizer, which would block deleting it while the operator is not running", func() {
		key := create(reviewAppNamed("without-finalizer"))

		// The client reads from the cache of the manager, which sees the ReviewAppConfig shortly after
		Eventually(func() error {
			return k8sClient.Get(ctx, key, &reviewapps.ReviewAppConfig{})
		}, 10*time.Second, 250*time.Millisecond).Should(Succeed())

		Consistently(func(g Gomega) {
			reviewApp := &reviewapps.ReviewAppConfig{}
			g.Expect(k8sClient.Get(ctx, key, reviewApp)).To(Succeed())
			g.Expect(reviewApp.Finalizers).To(BeEmpty())
		}, 2*time.Second, 250*time.Millisecond).Should(Succeed())
	})

	It("removes the finalizer of earlier versions", func() {
		key := create(reviewAppNamed("with-finalizer", legacyFinalizer))

		Eventually(func(g Gomega) {
			reviewApp := &reviewapps.ReviewAppConfig{}
			g.Expect(k8sClient.Get(ctx, key, reviewApp)).To(Succeed())
			g.Expect(reviewApp.Finalizers).To(BeEmpty())
		}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
	})

	It("keeps the finalizers of others", func() {
		key := create(reviewAppNamed("other-finalizers", legacyFinalizer, "example.com/other"))

		Eventually(func(g Gomega) {
			reviewApp := &reviewapps.ReviewAppConfig{}
			g.Expect(k8sClient.Get(ctx, key, reviewApp)).To(Succeed())
			g.Expect(reviewApp.Finalizers).To(ConsistOf("example.com/other"))
		}, 10*time.Second, 250*time.Millisecond).Should(Succeed())

		// Let the ReviewAppConfig be deleted after the spec
		Eventually(func() error {
			reviewApp := &reviewapps.ReviewAppConfig{}
			if err := k8sClient.Get(ctx, key, reviewApp); err != nil {
				return err
			}
			reviewApp.Finalizers = nil
			return k8sClient.Update(ctx, reviewApp)
		}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
	})
})
