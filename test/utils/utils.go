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

package utils

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is idiomatic for Ginkgo specs
	"k8s.io/client-go/tools/clientcmd"
)

// DefaultKindCluster is the Kind cluster the e2e tests run in, unless KIND_CLUSTER names another
const DefaultKindCluster = "review-app-operator-test-e2e"

// kubeconfig is the kubeconfig of the Kind cluster that every command runs with, set by
// UseKindCluster. It only holds the Kind cluster, so that the e2e tests can never act on the
// clusters of the current kube context
var kubeconfig string

// helmClusterEnv are the environment variables that point helm at a cluster outside the kubeconfig
var helmClusterEnv = []string{
	"HELM_KUBECONTEXT",
	"HELM_KUBEAPISERVER",
	"HELM_KUBETOKEN",
	"HELM_KUBECAFILE",
	"HELM_KUBEASUSER",
	"HELM_KUBEASGROUPS",
	"HELM_KUBETLS_SERVER_NAME",
	"HELM_KUBEINSECURE_SKIP_TLS_VERIFY",
}

// KindCluster returns the name of the Kind cluster the e2e tests run in
func KindCluster() string {
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok && v != "" {
		return v
	}
	return DefaultKindCluster
}

// UseKindCluster writes the kubeconfig of the Kind cluster to dir and makes every command
// run by Run use it, after making sure that it only reaches a cluster on this machine
func UseKindCluster(dir string) error {
	cluster := KindCluster()

	config, err := exec.Command("kind", "get", "kubeconfig", "--name", cluster).Output()
	if err != nil {
		return fmt.Errorf("the e2e tests only run in the Kind cluster %q, create it with make test-e2e: %w", cluster, err)
	}

	if err := checkLocalCluster(config, "kind-"+cluster); err != nil {
		return err
	}

	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, config, 0o600); err != nil {
		return err
	}

	kubeconfig = path
	return nil
}

// checkLocalCluster checks that a kubeconfig only has the context of the Kind cluster, and that
// its API server is on this machine like Kind's
func checkLocalCluster(kubeconfig []byte, context string) error {
	config, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return err
	}

	if config.CurrentContext != context || len(config.Contexts) != 1 || len(config.Clusters) != 1 {
		return fmt.Errorf("refusing to run the e2e tests: the kubeconfig must only have the context %q", context)
	}

	for _, cluster := range config.Clusters {
		server, err := url.Parse(cluster.Server)
		if err != nil {
			return err
		}

		host := server.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("refusing to run the e2e tests against %s, which is not a Kind cluster on this machine",
				cluster.Server)
		}
	}

	return nil
}

// pin makes a command run in the project directory with the kubeconfig of the Kind cluster, and
// refuses commands before UseKindCluster has pinned it
func pin(cmd *exec.Cmd) error {
	if kubeconfig == "" {
		return errors.New("refusing to run commands before UseKindCluster pins the e2e tests to the Kind cluster")
	}

	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %s\n", err)
	}

	cmd.Env = []string{"GO111MODULE=on", "KUBECONFIG=" + kubeconfig}
	for _, env := range os.Environ() {
		name, _, _ := strings.Cut(env, "=")
		if name == "KUBECONFIG" || slices.Contains(helmClusterEnv, name) {
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}

	return nil
}

// Start starts a command that keeps running, like kubectl port-forward, pinned to the Kind
// cluster like Run. The caller stops it
func Start(cmd *exec.Cmd) error {
	if err := pin(cmd); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(GinkgoWriter, "starting: %s\n", strings.Join(cmd.Args, " "))
	return cmd.Start()
}

// Run executes the provided command within this context. Commands only run once UseKindCluster
// has pinned the Kind cluster, and always with its kubeconfig
func Run(cmd *exec.Cmd) ([]byte, error) {
	if err := pin(cmd); err != nil {
		return nil, err
	}

	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %s\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s failed with error: (%v) %s", command, err, string(output))
	}

	return output, nil
}

// LoadImageToKindClusterWithName loads a local docker image to the Kind cluster
func LoadImageToKindClusterWithName(name string) error {
	cmd := exec.Command("kind", "load", "docker-image", name, "--name", KindCluster())
	_, err := Run(cmd)
	return err
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}
