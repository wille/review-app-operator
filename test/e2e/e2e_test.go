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
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	return deploymentNameOf(branchName)
}

// deploymentNameOf is the name of the Deployment of the review app of a branch
func deploymentNameOf(branch string) string {
	reviewApp := &reviewapps.ReviewAppConfig{ObjectMeta: metav1.ObjectMeta{Name: reviewAppName}}
	pr := &reviewapps.PullRequest{
		ObjectMeta: metav1.ObjectMeta{Name: utils.GetResourceName(reviewAppName, branch)},
		Spec:       reviewapps.PullRequestSpec{ReviewAppConfigRef: reviewAppName, BranchName: branch},
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
			"review-app-operator-controller-manager": "2",
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

	It("keeps starting review apps when the leading controller stops", func() {
		// The controller replica that holds the leader election Lease, whose holder is
		// <pod name>_<random ID>
		leader := func() (string, error) {
			holder, err := kubectl("get", "lease", "80807133.reviewapps.william.nu", "-n", namespace,
				"-o", "jsonpath={.spec.holderIdentity}")
			pod, _, _ := strings.Cut(holder, "_")
			return pod, err
		}

		stopped, err := leader()
		Expect(err).NotTo(HaveOccurred())
		Expect(stopped).To(HavePrefix("review-app-operator-controller-manager-"))

		_, err = kubectl("delete", "pod", stopped, "-n", namespace, "--wait=false")
		Expect(err).NotTo(HaveOccurred())
		start := time.Now()

		// The stopping leader hands over right away, instead of the other replica waiting
		// 15 seconds for its Lease to expire
		Eventually(leader, 30*time.Second, 500*time.Millisecond).Should(And(
			HavePrefix("review-app-operator-controller-manager-"), Not(Equal(stopped))))
		_, _ = fmt.Fprintf(GinkgoWriter, "the other controller replica took over after %s\n", time.Since(start))
		Expect(time.Since(start)).To(BeNumerically("<", 10*time.Second))

		// The new leader starts the review app that was scaled down
		Eventually(visit, 5*time.Minute, 5*time.Second).Should(Equal("200"))
		Expect(replicas()).To(Equal("1"))
	})

	Context("when the Review App Action deploys a pull request through the webhook", Ordered, func() {
		// The pull request that the action deploys, which isn't visited, so its review app is
		// scaled down and pulls no image
		const deployBranch = "webhook-deploy"
		pullRequestName := utils.GetResourceName(reviewAppName, deployBranch)
		deployment := deploymentNameOf(deployBranch)

		var webhookURL, webhookSecret string

		BeforeAll(func() {
			By("reading the webhook secret that the chart created")
			secret, err := kubectl("get", "secret", "webhook-secret", "-n", namespace, "-o", "jsonpath={.data.webhook-secret}")
			Expect(err).NotTo(HaveOccurred())
			decoded, err := base64.StdEncoding.DecodeString(secret)
			Expect(err).NotTo(HaveOccurred())
			webhookSecret = string(decoded)

			By("forwarding a local port to the webhook, like the ingress does")
			portForward := exec.Command("kubectl", "port-forward", "-n", namespace, "svc/review-app-operator-webhook", ":8080")
			stdout, err := portForward.StdoutPipe()
			Expect(err).NotTo(HaveOccurred())
			Expect(testutils.Start(portForward)).To(Succeed())
			DeferCleanup(func() {
				_ = portForward.Process.Kill()
				_ = portForward.Wait()
			})

			// kubectl prints "Forwarding from 127.0.0.1:<port> -> 8080" once it listens
			address := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					if local, ok := strings.CutPrefix(scanner.Text(), "Forwarding from 127.0.0.1:"); ok {
						port, _, _ := strings.Cut(local, " ")
						address <- "http://127.0.0.1:" + port
						return
					}
				}
			}()
			Eventually(address, 30*time.Second).Should(Receive(&webhookURL))
		})

		// runAction runs the action script for a pull request event like GitHub runs it, and
		// returns its outputs
		runAction := func(action, image string) (map[string]string, error) {
			dir := GinkgoT().TempDir()

			event := filepath.Join(dir, "event.json")
			Expect(os.WriteFile(event, []byte(`{
				"action": "`+action+`",
				"number": 7,
				"repository": {"html_url": "https://github.com/octo-org/smoke", "full_name": "octo-org/smoke"},
				"pull_request": {
					"head": {"ref": "`+deployBranch+`", "sha": "0123456789abcdef"},
					"html_url": "https://github.com/octo-org/smoke/pull/7",
					"merged": false
				},
				"sender": {"html_url": "https://github.com/octocat"}
			}`), 0o600)).To(Succeed())

			output := filepath.Join(dir, "github_output")
			Expect(os.WriteFile(output, nil, 0o600)).To(Succeed())

			projectDir, err := testutils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())

			// The action only reaches the webhook, so it doesn't run with the kubeconfig of the
			// Kind cluster, nor any other
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", filepath.Join(projectDir, "action-entrypoint.sh"))
			for _, env := range os.Environ() {
				if name, _, _ := strings.Cut(env, "="); name != "KUBECONFIG" {
					cmd.Env = append(cmd.Env, env)
				}
			}
			cmd.Env = append(cmd.Env,
				"GITHUB_EVENT_NAME=pull_request",
				"GITHUB_EVENT_PATH="+event,
				"GITHUB_OUTPUT="+output,
				"RUNNER_TEMP="+dir,
				"INPUT_REVIEW_APP_NAME="+reviewAppName,
				"INPUT_REVIEW_APP_NAMESPACE="+reviewAppsNamespace,
				"INPUT_WEBHOOK_URL="+webhookURL,
				"INPUT_WEBHOOK_SECRET="+webhookSecret,
				"INPUT_IMAGE="+image,
				"INPUT_CHECK=false",
			)
			log, err := cmd.CombinedOutput()
			_, _ = fmt.Fprintf(GinkgoWriter, "$ action %s %s\n%s\n", action, image, log)

			outputs := map[string]string{}
			written, readErr := os.ReadFile(output)
			Expect(readErr).NotTo(HaveOccurred())
			for _, line := range strings.Split(strings.TrimSpace(string(written)), "\n") {
				if name, value, ok := strings.Cut(line, "="); ok {
					outputs[name] = value
				}
			}

			return outputs, err
		}

		// image returns the image of the target container of the deployment
		image := func() (string, error) {
			return kubectl("get", "deployment", deployment, "-n", reviewAppsNamespace,
				"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="web")].image}`)
		}

		It("deploys the pull request and reports its URLs", func() {
			outputs, err := runAction("opened", appImage)
			Expect(err).NotTo(HaveOccurred())

			Expect(outputs).To(HaveKeyWithValue("review_app_url", "https://webhook-deploy.smoke.example.com"))
			var urls map[string][]string
			Expect(json.Unmarshal([]byte(outputs["review_app_urls"]), &urls)).To(Succeed())
			Expect(urls).To(Equal(map[string][]string{"web": {"https://webhook-deploy.smoke.example.com"}}))

			// The controller updates the status right after the deployment, which the action waits for
			Eventually(func() (string, error) {
				return kubectl("get", "pullrequests", pullRequestName, "-n", reviewAppsNamespace,
					"-o", `jsonpath={.status.url} {.status.conditions[?(@.type=="Reconciled")].status}`)
			}, 30*time.Second, time.Second).Should(Equal("https://webhook-deploy.smoke.example.com True"))
			Expect(image()).To(Equal(appImage))
		})

		It("keeps the environment overrides of the pull request when a new image is pushed", func() {
			apiURL := func() (string, error) {
				return kubectl("get", "deployment", deployment, "-n", reviewAppsNamespace,
					"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="web")].env[?(@.name=="API_URL")].value}`)
			}

			_, err := kubectl("patch", "pullrequests", pullRequestName, "-n", reviewAppsNamespace, "--type", "merge",
				"-p", `{"spec":{"deployments":[{"name":"web","env":[{"name":"API_URL","value":"https://api.example.com"}]}]}}`)
			Expect(err).NotTo(HaveOccurred())
			Eventually(apiURL, time.Minute, 2*time.Second).Should(Equal("https://api.example.com"))

			// Scaled down, the review app doesn't pull the new image
			_, err = runAction("synchronize", "nginx:1.28-alpine")
			Expect(err).NotTo(HaveOccurred())

			Expect(image()).To(Equal("nginx:1.28-alpine"))
			Expect(apiURL()).To(Equal("https://api.example.com"))
		})

		It("deletes the review app when the pull request is closed", func() {
			_, err := runAction("closed", "")
			Expect(err).NotTo(HaveOccurred())

			_, err = kubectl("get", "pullrequests", pullRequestName, "-n", reviewAppsNamespace)
			Expect(err).To(MatchError(ContainSubstring("NotFound")))

			// The garbage collector deletes what the PullRequest owned
			Eventually(func() (string, error) {
				return kubectl("get", "deployments", "-n", reviewAppsNamespace, "-o", "name")
			}, time.Minute, 2*time.Second).ShouldNot(ContainSubstring(deployment))
		})
	})
})
