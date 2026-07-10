package plan

import (
	"context"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// Load reads a plan from a YAML file (JSON, being a YAML subset, works too).
func Load(path string) (*Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read plan %q: %w", path, err)
	}
	var p Plan
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("plan %q is not valid YAML: %w", path, err)
	}
	if len(p.Targets) == 0 {
		return nil, fmt.Errorf("plan %q contains no targets", path)
	}
	return &p, nil
}

// Marshal serializes a plan as YAML.
func Marshal(p *Plan) ([]byte, error) {
	return yaml.Marshal(p)
}

// Applier executes plan targets against the cluster.
type Applier struct {
	kube kubernetes.Interface
}

func NewApplier(kube kubernetes.Interface) *Applier {
	return &Applier{kube: kube}
}

// Apply patches the owner workload of a target. With dryRun=true the patch
// is validated server-side (LimitRanges, quotas, admission webhooks)
// without changing anything. The real patch triggers the workload's rolling
// restart automatically.
func (a *Applier) Apply(ctx context.Context, t ContainerTarget, dryRun bool) error {
	data, err := BuildStrategicMergePatch(t)
	if err != nil {
		return err
	}
	po := metav1.PatchOptions{FieldManager: "kubehoard"}
	if dryRun {
		po.DryRun = []string{metav1.DryRunAll}
	}
	switch t.Kind {
	case "Deployment":
		_, err = a.kube.AppsV1().Deployments(t.Namespace).Patch(ctx, t.Workload, types.StrategicMergePatchType, data, po)
	case "StatefulSet":
		_, err = a.kube.AppsV1().StatefulSets(t.Namespace).Patch(ctx, t.Workload, types.StrategicMergePatchType, data, po)
	case "DaemonSet":
		_, err = a.kube.AppsV1().DaemonSets(t.Namespace).Patch(ctx, t.Workload, types.StrategicMergePatchType, data, po)
	default:
		return fmt.Errorf("%s: kind %q is not supported", t, t.Kind)
	}
	if err != nil {
		mode := "patch"
		if dryRun {
			mode = "dry-run"
		}
		return fmt.Errorf("%s: %s failed: %w", t, mode, err)
	}
	return nil
}
