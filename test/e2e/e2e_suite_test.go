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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wille/review-app-operator/test/utils"
)

// Run e2e tests using the Ginkgo runner.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting review-app-operator suite\n")
	RunSpecs(t, "e2e suite")
}

// The e2e tests delete what they deploy, so before anything runs they are pinned to the Kind
// cluster and can never act on the clusters of the current kube context
var _ = BeforeSuite(func() {
	By("pinning the tests to the Kind cluster " + utils.KindCluster())
	Expect(utils.UseKindCluster(GinkgoT().TempDir())).To(Succeed())
})
