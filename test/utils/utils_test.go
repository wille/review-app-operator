package utils

import (
	"os/exec"
	"strings"
	"testing"
)

func kubeconfigFor(context string, servers ...string) []byte {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\ncurrent-context: " + context + "\nclusters:\n")
	for i, server := range servers {
		name := context
		if i > 0 {
			name = context + "-" + string(rune('a'+i))
		}
		b.WriteString("- name: " + name + "\n  cluster:\n    server: " + server + "\n")
	}
	b.WriteString("contexts:\n")
	for i := range servers {
		name := context
		if i > 0 {
			name = context + "-" + string(rune('a'+i))
		}
		b.WriteString("- name: " + name + "\n  context:\n    cluster: " + name + "\n    user: " + name + "\n")
	}
	b.WriteString("users:\n- name: " + context + "\n  user: {}\n")
	return []byte(b.String())
}

func TestCheckLocalCluster(t *testing.T) {
	const context = "kind-review-app-operator-test-e2e"

	tests := []struct {
		name       string
		kubeconfig []byte
		allowed    bool
	}{
		{"kind on IPv4 loopback", kubeconfigFor(context, "https://127.0.0.1:41231"), true},
		{"kind on IPv6 loopback", kubeconfigFor(context, "https://[::1]:41231"), true},
		{"localhost", kubeconfigFor(context, "https://localhost:6443"), true},
		{"remote cluster", kubeconfigFor(context, "https://api.k8s.example.com"), false},
		{"remote cluster by IP", kubeconfigFor(context, "https://10.0.0.1:6443"), false},
		{"another current context", kubeconfigFor("production", "https://127.0.0.1:6443"), false},
		{"more clusters than Kind's", kubeconfigFor(context, "https://127.0.0.1:6443", "https://api.k8s.example.com"), false},
		{"not a kubeconfig", []byte("{"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkLocalCluster(tt.kubeconfig, context)
			if tt.allowed && err != nil {
				t.Fatalf("expected the cluster to be allowed, got %v", err)
			}
			if !tt.allowed && err == nil {
				t.Fatal("expected the cluster to be refused")
			}
		})
	}
}

func TestRunRefusesCommandsBeforeTheKindClusterIsPinned(t *testing.T) {
	kubeconfig = ""

	if _, err := Run(exec.Command("true")); err == nil {
		t.Fatal("expected Run to refuse commands before UseKindCluster")
	}
}
