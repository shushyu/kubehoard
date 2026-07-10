package collector

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// resolveTopOwner resolves a pod's top-level owner workload:
//
//	Pod -> ReplicaSet -> Deployment
//	Pod -> StatefulSet / DaemonSet (direct)
//	Pod -> Job / DeploymentConfig / ... (passed through unchanged;
//	       apply deliberately does not support these kinds)
//
// rsCache avoids repeated GETs for ReplicaSets of the same Deployment.
// A ("", "") return means: bare pod without a controller.
func (c *Collector) resolveTopOwner(ctx context.Context, pod *corev1.Pod, rsCache map[string]*appsv1.ReplicaSet) (kind, name string) {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return "", ""
	}
	if ref.Kind != "ReplicaSet" {
		return ref.Kind, ref.Name
	}

	key := pod.Namespace + "/" + ref.Name
	rs, ok := rsCache[key]
	if !ok {
		got, err := c.kube.AppsV1().ReplicaSets(pod.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			// Best effort: report the ReplicaSet itself as owner;
			// apply will then reject the kind in a controlled way.
			return "ReplicaSet", ref.Name
		}
		rsCache[key] = got
		rs = got
	}
	if oref := metav1.GetControllerOf(rs); oref != nil && oref.Kind == "Deployment" {
		return "Deployment", oref.Name
	}
	return "ReplicaSet", ref.Name
}
