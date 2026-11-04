package utils

import (
	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import is idiomatic for Ginkgo specs
	. "github.com/onsi/gomega"    //nolint:staticcheck // dot-import is idiomatic for Gomega matchers
)

var _ = Describe("Image repositories", func() {
	const digest = "@sha256:4b8e1b5ecb5b1a9a0a3cbc6c0e7f1b2d3c4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9"

	DescribeTable("IsSameImageRepo",
		func(i1, i2 string, same bool) {
			Expect(IsSameImageRepo(i1, i2)).To(Equal(same))
		},
		Entry("different tags", "nginx:1", "nginx:2", true),
		Entry("untagged and tagged", "nginx", "nginx:latest", true),
		Entry("tag and digest", "ghcr.io/o/app:v1", "ghcr.io/o/app"+digest, true),
		Entry("tag and tag with digest", "ghcr.io/o/app:v1", "ghcr.io/o/app:sha-1"+digest, true),
		Entry("registry with port", "registry.example.com:5000/app:v1", "registry.example.com:5000/app:v2", true),
		Entry("untagged registry with port", "registry.example.com:5000/app", "registry.example.com:5000/app:v2", true),

		Entry("another repository", "ghcr.io/o/app:v1", "ghcr.io/o/other:v1", false),
		Entry("a nested repository", "ghcr.io/o/app:v1", "ghcr.io/o/app/other:v1", false),
		Entry("another repository on a registry with port", "registry.example.com:5000/app:v1", "registry.example.com:5000/other:v1", false),
		Entry("another registry port", "registry.example.com:5000/app:v1", "registry.example.com:5001/app:v1", false),
		Entry("a path after the tag", "ghcr.io/o/app:v1", "ghcr.io/o/app:v1/../other", false),
		Entry("no image", "nginx:1", "", false),
	)
})
