package collector

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// BuildRESTConfig resolves the kubeconfig in the usual kubectl order:
//  1. explicit --kubeconfig flag
//  2. $KUBECONFIG
//  3. in-cluster config (service account)
//  4. ~/.kube/config
func BuildRESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig %q: %w", kubeconfig, err)
		}
		return cfg, nil
	}

	if env := os.Getenv("KUBECONFIG"); env != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", env)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig from $KUBECONFIG (%q): %w", env, err)
		}
		return cfg, nil
	}

	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("no --kubeconfig, $KUBECONFIG or in-cluster config available and home directory unknown: %w", err)
	}
	def := filepath.Join(home, ".kube", "config")
	cfg, err := clientcmd.BuildConfigFromFlags("", def)
	if err != nil {
		return nil, fmt.Errorf("no valid kubeconfig found (tried: --kubeconfig, $KUBECONFIG, in-cluster, %s): %w", def, err)
	}
	return cfg, nil
}
