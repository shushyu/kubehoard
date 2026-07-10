package plan

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Validate checks a target client-side: supported kind, required fields set,
// all provided quantities parseable, at least one change.
func Validate(t ContainerTarget) error {
	if t.Namespace == "" || t.Workload == "" || t.Container == "" {
		return fmt.Errorf("%s: namespace, workload and container are required", t)
	}
	if !supportedKinds[t.Kind] {
		return fmt.Errorf("%s: kind %q is not supported (allowed: Deployment, StatefulSet, DaemonSet)", t, t.Kind)
	}
	changes := 0
	for field, val := range map[string]string{
		"cpuRequest": t.CPURequest, "memRequest": t.MemRequest,
		"cpuLimit": t.CPULimit, "memLimit": t.MemLimit,
	} {
		if val == "" {
			continue
		}
		if _, err := resource.ParseQuantity(val); err != nil {
			return fmt.Errorf("%s: %s=%q is not a valid quantity: %w", t, field, val, err)
		}
		changes++
	}
	if changes == 0 {
		return fmt.Errorf("%s: no change specified (all fields empty)", t)
	}
	return nil
}

// BuildStrategicMergePatch builds the patch against the workload's pod
// template. Strategic merge uses "name" as the merge key of the container
// list: only the named container is changed, and only the specified
// resource keys are replaced — everything else stays untouched.
func BuildStrategicMergePatch(t ContainerTarget) ([]byte, error) {
	if err := Validate(t); err != nil {
		return nil, err
	}
	requests := map[string]string{}
	limits := map[string]string{}
	if t.CPURequest != "" {
		requests["cpu"] = t.CPURequest
	}
	if t.MemRequest != "" {
		requests["memory"] = t.MemRequest
	}
	if t.CPULimit != "" {
		limits["cpu"] = t.CPULimit
	}
	if t.MemLimit != "" {
		limits["memory"] = t.MemLimit
	}
	resources := map[string]any{}
	if len(requests) > 0 {
		resources["requests"] = requests
	}
	if len(limits) > 0 {
		resources["limits"] = limits
	}
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name":      t.Container,
							"resources": resources,
						},
					},
				},
			},
		},
	}
	return json.Marshal(patch)
}
