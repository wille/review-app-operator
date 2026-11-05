//go:build e2e

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

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/utils"
	testutils "github.com/wille/review-app-operator/test/utils"
)

// namespace is not the one of real installs, in case the tests ever reach a real cluster
const namespace = "review-app-operator-e2e"

// reviewAppsNamespace is where the review apps run
const reviewAppsNamespace = "default"

// The image of the operator that is built and loaded into the Kind cluster
const image, tag = "example.com/review-app-operator", "v0.0.1"

// The review app that the smoke test deploys and visits
const (
	reviewAppName = "smoke"
	branchName    = "feature"
	host          = "feature.smoke.example.com"
	appImage      = "nginx:1.27-alpine"
)

// The forwarder Service that the ingress sends the review app hosts to
const forwarderURL = "http://review-app-operator-forwarder." + namespace + ".svc:6969/"

var reviewAppManifest = `
apiVersion: reviewapps.william.nu/v1alpha1
kind: ReviewAppConfig
metadata:
  name: ` + reviewAppName + `
  namespace: ` + reviewAppsNamespace + `
spec:
  scaleDownAfter: 30s
  deployments:
    - name: web
      targetContainerName: web
      targetContainerPort: 80
      hostTemplates:
        - "{{.BranchName}}.smoke.example.com"
      template:
        spec:
          containers:
            - name: web
              image: ` + appImage + `
              ports:
                - containerPort: 80
---
apiVersion: reviewapps.william.nu/v1alpha1
kind: PullRequest
metadata:
  name: ` + reviewAppName + `-` + branchName + `
  namespace: ` + reviewAppsNamespace + `
spec:
  reviewAppRef: ` + reviewAppName + `
  branchName: ` + branchName + `
  imageName: ` + appImage + `
`

// kubectl runs kubectl in the Kind cluster and returns its output
func kubectl(args ...string) (string, error) {
	output, err := testutils.Run(exec.Command("kubectl", args...))
	return strings.TrimSpace(string(output)), err
}

// deploymentName is the name of the Deployment, Service and Lease of the review app
func deploymentName() string {
	reviewApp := &reviewapps.ReviewAppConfig{ObjectMeta: metav1.ObjectMeta{Name: reviewAppName}}
	pr := &reviewapps.PullRequest{
		ObjectMeta: metav1.ObjectMeta{Name: reviewAppName + "-" + branchName},
		Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: reviewAppName, BranchName: branchName},
	}
	return utils.GetDeploymentName(reviewApp, pr, "web")
}

// replicas returns the replicas that the controller scaled the review app to
func replicas() (string, error) {
	return kubectl("get", "deployment", deploymentName(), "-n", reviewAppsNamespace, "-o", "jsonpath={.spec.replicas}")
}

// visit requests the review app through the forwarder, from a pod in the cluster, and returns the status code
func visit() (string, error) {
	return kubectl("exec", "curl", "-n", reviewAppsNamespace, "--",
		"curl", "--silent", "--max-time", "30", "--output", "/dev/null", "--write-out", "%{http_code}",
		"--header", "Host: "+host, forwarderURL)
}

// dumpDiagnostics writes the state of the operator and the review app, to find out why a spec failed
func dumpDiagnostics() {
	for _, args := range [][]string{
		{"get", "deployments,pods,services", "-n", namespace, "-o", "wide"},
		{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp"},
		{"get", "reviewappconfigs,pullrequests,deployments,pods,leases", "-n", reviewAppsNamespace, "-o", "wide"},
		{"get", "pullrequests", "-n", reviewAppsNamespace, "-o", "yaml"},
		{"get", "events", "-n", reviewAppsNamespace, "--sort-by=.lastTimestamp"},
		{"logs", "-n", namespace, "-l", "control-plane=controller-manager", "--tail=200", "--prefix"},
		{"logs", "-n", namespace, "-l", "control-plane=forwarder", "--tail=200", "--prefix"},
		{"logs", "-n", namespace, "-l", "control-plane=webhook", "--tail=200", "--prefix"},
	} {
		output, err := kubectl(args...)
		_, _ = fmt.Fprintf(GinkgoWriter, "\n$ kubectl %s\n%s\n", strings.Join(args, " "), output)
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", err)
		}
	}
}

var _ = Describe("Review App Operator", Ordered, func() {
	BeforeAll(func() {
		By("creating a namespace that enforces the restricted Pod Security Standard")
		_, err := kubectl("create", "namespace", namespace)
		Expect(err).NotTo(HaveOccurred())
		_, err = kubectl("label", "namespace", namespace,
			"pod-security.kubernetes.io/enforce=restricted", "pod-security.kubernetes.io/enforce-version=latest")
		Expect(err).NotTo(HaveOccurred())

		By("building the operator image")
		_, err = testutils.Run(exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s:%s", image, tag)))
		Expect(err).NotTo(HaveOccurred())

		By("loading the operator image into Kind")
		Expect(testutils.LoadImageToKindClusterWithName(image + ":" + tag)).To(Succeed())

		By("installing the Helm chart")
		_, err = testutils.Run(exec.Command("helm", "install", "review-app-operator", "./chart",
			"--namespace", namespace,
			"--set", "image="+image,
			"--set", "tag="+tag,
			"--set", "namespaces={"+reviewAppsNamespace+"}",
			"--set", "webhook.ingress.host=webhook.example.com",
			"--set", "forwarder.ingress.hosts[0]=*.example.com",
		))
		Expect(err).NotTo(HaveOccurred())

		By("starting a pod to visit the review app from")
		_, err = kubectl("run", "curl", "-n", reviewAppsNamespace, "--image=curlimages/curl:8.11.1",
			"--restart=Never", "--command", "--", "sleep", "3600")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDiagnostics()
		}
	})

	AfterAll(func() {
		By("removing the review app, the operator and its namespace")
		for _, args := range [][]string{
			{"delete", "pullrequests,reviewappconfigs", "--all", "-n", reviewAppsNamespace, "--ignore-not-found"},
			{"delete", "pod", "curl", "-n", reviewAppsNamespace, "--ignore-not-found"},
		} {
			_, _ = kubectl(args...)
		}
		_, _ = testutils.Run(exec.Command("helm", "uninstall", "review-app-operator", "--namespace", namespace))
		_, _ = kubectl("delete", "namespace", namespace, "--ignore-not-found")
	})

	It("runs the controller, forwarder and webhook", func() {
		// With the replicas, RBAC, readiness probes and security settings of the chart
		_, err := kubectl("wait", "deployment", "--all", "-n", namespace,
			"--for=condition=Available", "--timeout=5m")
		Expect(err).NotTo(HaveOccurred())

		for deployment, replicas := range map[string]string{
			"review-app-operator-controller-manager": "1",
			"review-app-operator-forwarder":          "2",
			"review-app-operator-webhook":            "2",
		} {
			Expect(kubectl("get", "deployment", deployment, "-n", namespace, "-o", "jsonpath={.status.readyReplicas}")).
				To(Equal(replicas), deployment)
		}
	})

	It("creates a review app that is scaled down until it is visited", func() {
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(reviewAppManifest)
		_, err := testutils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		Eventually(replicas, 2*time.Minute, 2*time.Second).Should(Equal("0"))
		Expect(kubectl("get", "service", deploymentName(), "-n", reviewAppsNamespace, "-o", "name")).
			To(Equal("service/" + deploymentName()))
		Expect(kubectl("get", "lease", deploymentName(), "-n", reviewAppsNamespace,
			"-o", "jsonpath={.spec.leaseDurationSeconds}")).To(Equal("30"))
	})

	It("starts the review app when it is visited through the forwarder", func() {
		_, err := kubectl("wait", "pod", "curl", "-n", reviewAppsNamespace, "--for=condition=Ready", "--timeout=3m")
		Expect(err).NotTo(HaveOccurred())

		// The forwarder answers 503 while the review app starts and pulls its image
		Eventually(visit, 5*time.Minute, 5*time.Second).Should(Equal("200"))

		Expect(replicas()).To(Equal("1"))
		Expect(kubectl("get", "lease", deploymentName(), "-n", reviewAppsNamespace, "-o", "jsonpath={.spec.holderIdentity}")).
			To(HavePrefix("review-app-operator-forwarder-"))
	})

	It("scales the review app down when it is no longer visited", func() {
		// After scaleDownAfter has passed since the last visit
		Eventually(replicas, 3*time.Minute, 5*time.Second).Should(Equal("0"))
	})
})
