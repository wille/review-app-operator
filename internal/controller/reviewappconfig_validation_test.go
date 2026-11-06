package controller

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
)

// The API server rejects ReviewAppConfigs that the operator can't work with
var _ = Describe("ReviewAppConfig validation", func() {
	const namespace = "default"

	deployment := func(name string) reviewapps.Deployments {
		return reviewapps.Deployments{
			Name:                name,
			TargetContainerName: "app",
			TargetContainerPort: 8080,
			HostTemplates:       []string{"{{.BranchName}}-" + name + ".validation.example.com"},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
				},
			},
		}
	}

	// reviewApp returns a valid ReviewAppConfig, changed by change
	reviewApp := func(change func(*reviewapps.ReviewAppConfig)) *reviewapps.ReviewAppConfig {
		reviewApp := &reviewapps.ReviewAppConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "validation", Namespace: namespace},
			Spec: reviewapps.ReviewAppConfigSpec{
				Deployments: []reviewapps.Deployments{deployment("web"), deployment("api")},
			},
		}
		change(reviewApp)
		return reviewApp
	}

	// withDuration returns a valid ReviewAppConfig with a duration written like in YAML, which the
	// typed client can't send when it isn't a valid duration. The deployment field is in the
	// first deployment
	withDuration := func(field, value string) *unstructured.Unstructured {
		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(reviewApp(func(*reviewapps.ReviewAppConfig) {}))
		Expect(err).NotTo(HaveOccurred())
		obj := &unstructured.Unstructured{Object: content}
		obj.SetAPIVersion(reviewapps.GroupVersion.String())
		obj.SetKind("ReviewAppConfig")

		if deploymentField, ok := strings.CutPrefix(field, "spec.deployments[0]."); ok {
			deployments, _, err := unstructured.NestedSlice(obj.Object, "spec", "deployments")
			Expect(err).NotTo(HaveOccurred())
			deployments[0].(map[string]any)[deploymentField] = value
			Expect(unstructured.SetNestedSlice(obj.Object, deployments, "spec", "deployments")).To(Succeed())
		} else {
			Expect(unstructured.SetNestedField(obj.Object, value, strings.Split(field, ".")...)).To(Succeed())
		}

		return obj
	}

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &reviewapps.ReviewAppConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "validation", Namespace: namespace},
		}))).To(Succeed())
	})

	DescribeTable("rejects deployments",
		func(change func(*reviewapps.ReviewAppConfig), message string) {
			err := k8sClient.Create(ctx, reviewApp(change))

			Expect(errors.IsInvalid(err)).To(BeTrue(), "%v", err)
			Expect(err).To(MatchError(ContainSubstring(message)))
		},
		Entry("with the same names", func(r *reviewapps.ReviewAppConfig) {
			r.Spec.Deployments[1].Name = "web"
		}, "deployments must have different names"),
		Entry("without names", func(r *reviewapps.ReviewAppConfig) {
			r.Spec.Deployments[0].Name = ""
			r.Spec.Deployments[1].Name = ""
		}, "deployments must have different names"),
		Entry("with a name longer than 63 characters", func(r *reviewapps.ReviewAppConfig) {
			r.Spec.Deployments[0].Name = strings.Repeat("a", 64)
		}, "spec.deployments[0].name"),
		Entry("with port 0", func(r *reviewapps.ReviewAppConfig) {
			r.Spec.Deployments[0].TargetContainerPort = 0
		}, "spec.deployments[0].targetContainerPort"),
		Entry("with a port above 65535", func(r *reviewapps.ReviewAppConfig) {
			r.Spec.Deployments[0].TargetContainerPort = 65536
		}, "spec.deployments[0].targetContainerPort"),
		Entry("with negative replicas", func(r *reviewapps.ReviewAppConfig) {
			r.Spec.Deployments[0].Replicas = -1
		}, "spec.deployments[0].replicas"),
	)

	It("rejects repositories that aren't like octo-org/octo-repo", func() {
		err := k8sClient.Create(ctx, reviewApp(func(r *reviewapps.ReviewAppConfig) {
			r.Spec.Repositories = []string{"octo-org/app", "https://github.com/octo-org/app"}
		}))

		Expect(errors.IsInvalid(err)).To(BeTrue(), "%v", err)
		Expect(err).To(MatchError(ContainSubstring("spec.repositories[1]")))
	})

	It("accepts a single deployment without a name", func() {
		Expect(k8sClient.Create(ctx, reviewApp(func(r *reviewapps.ReviewAppConfig) {
			r.Spec.Deployments = r.Spec.Deployments[:1]
			r.Spec.Deployments[0].Name = ""
		}))).To(Succeed())
	})

	DescribeTable("rejects durations that the operator can't read",
		func(field, value string) {
			err := k8sClient.Create(ctx, withDuration(field, value))

			Expect(errors.IsInvalid(err)).To(BeTrue(), "%v", err)
			Expect(err).To(MatchError(ContainSubstring(field)))
		},
		Entry("in scaleDownAfter", "spec.scaleDownAfter", "2 hours"),
		Entry("in deleteAfter", "spec.deleteAfter", "7d"),
		Entry("in connectionTimeout", "spec.connectionTimeout", "10"),
		Entry("in the scaleDownAfter of a deployment", "spec.deployments[0].scaleDownAfter", "1 hour"),
		Entry("that are negative", "spec.scaleDownAfter", "-1h"),
	)

	DescribeTable("accepts durations",
		func(value string) {
			Expect(k8sClient.Create(ctx, withDuration("spec.scaleDownAfter", value))).To(Succeed())

			// The operator can read it
			var got reviewapps.ReviewAppConfig
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "validation"}, &got)
			}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
		},
		Entry("in hours and minutes", "1h30m"),
		Entry("with a fraction", "1.5h"),
		Entry("in milliseconds", "500ms"),
		Entry("of zero", "0"),
		Entry("the way the operator writes them", (90*time.Minute).String()),
	)
})
