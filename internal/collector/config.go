package collector

import (
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// BuildRESTConfig resolves the kubeconfig like kubectl does:
//  1. explicit --kubeconfig flag
//  2. $KUBECONFIG (supports colon-separated lists of files, merged)
//  3. ~/.kube/config
//  4. in-cluster config (service account) as a fallback
func BuildRESTConfig(kubeconfig string) (*rest.Config, error) {
	// NewDefaultClientConfigLoadingRules honors $KUBECONFIG including
	// colon-separated multi-file lists and falls back to ~/.kube/config —
	// the same merging semantics as kubectl.
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err == nil {
		return cfg, nil
	}

	// No usable kubeconfig — maybe we are running inside a cluster.
	if inCluster, icErr := rest.InClusterConfig(); icErr == nil {
		return inCluster, nil
	}

	return nil, fmt.Errorf("no valid kubeconfig found (tried: --kubeconfig, $KUBECONFIG, ~/.kube/config, in-cluster): %w", err)
}
