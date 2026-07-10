// Package collector gathers pod specs (Kube API) and actual usage
// (metrics API, metrics.k8s.io) and combines both into a report.
package collector

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/aleks/kubehoard/internal/model"
)

const metricsGroupVersion = "metrics.k8s.io/v1beta1"

// Options control namespace selection.
type Options struct {
	// LabelSelector filters namespaces server-side (e.g. "team=platform").
	LabelSelector string
	// IncludeRegex: only namespaces whose name matches (nil = all).
	IncludeRegex *regexp.Regexp
	// ExcludeRegex: namespaces whose name matches are skipped (nil = none).
	// Applied after IncludeRegex.
	ExcludeRegex *regexp.Regexp
	// Names: exact namespace names (from -n/--namespace); empty = all.
	// Applied before the regex filters.
	Names []string
	// ResolveOwners: resolve the owner workload per pod (extra API calls
	// for ReplicaSets, required for the suggest/apply workflow).
	ResolveOwners bool
}

// Collector wraps the two API clients.
type Collector struct {
	kube    kubernetes.Interface
	metrics metricsclient.Interface
}

// New creates a Collector.
func New(kube kubernetes.Interface, metrics metricsclient.Interface) *Collector {
	return &Collector{kube: kube, metrics: metrics}
}

// containerUsage: container name -> usage ResourceList of one pod.
type containerUsage map[string]corev1.ResourceList

// Collect runs the scan. Namespace-level failures do not abort the scan but
// end up as warnings in the report; hard failures (metrics API missing,
// namespaces not listable) return an error.
func (c *Collector) Collect(ctx context.Context, opts Options, th model.Thresholds, server string) (*model.Report, error) {
	// Early, explicit check instead of a cryptic error on the first list call.
	if _, err := c.kube.Discovery().ServerResourcesForGroupVersion(metricsGroupVersion); err != nil {
		return nil, fmt.Errorf(
			"metrics API (%s) is not available on this cluster — is metrics-server (or the Prometheus adapter on OpenShift) running? Underlying error: %w",
			metricsGroupVersion, err)
	}

	nsList, err := c.kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: opts.LabelSelector})
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}

	report := &model.Report{
		GeneratedAt: time.Now(),
		Server:      server,
		Thresholds:  th,
	}

	wantNames := map[string]bool{}
	for _, n := range opts.Names {
		wantNames[n] = true
	}
	seenNames := map[string]bool{}

	for _, ns := range nsList.Items {
		name := ns.Name
		if len(wantNames) > 0 {
			if !wantNames[name] {
				continue
			}
			seenNames[name] = true
		}
		if opts.IncludeRegex != nil && !opts.IncludeRegex.MatchString(name) {
			continue
		}
		if opts.ExcludeRegex != nil && opts.ExcludeRegex.MatchString(name) {
			continue
		}

		nsReport, warns := c.collectNamespace(ctx, name, th, opts.ResolveOwners)
		report.Warnings = append(report.Warnings, warns...)
		if nsReport == nil {
			continue
		}
		report.Namespaces = append(report.Namespaces, *nsReport)
	}

	for _, n := range opts.Names {
		if !seenNames[n] {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("namespace %s not found (or filtered out by the label selector)", n))
		}
	}

	// Ranking: descending by largest overprovisioning factor, alphabetical
	// on ties for stable output.
	sort.Slice(report.Namespaces, func(i, j int) bool {
		a, b := report.Namespaces[i], report.Namespaces[j]
		if a.SortFactor != b.SortFactor {
			return a.SortFactor > b.SortFactor
		}
		return a.Name < b.Name
	})

	summarize(report)
	return report, nil
}

// collectNamespace evaluates a single namespace.
// A nil return means: namespace skipped (error or no pods).
func (c *Collector) collectNamespace(ctx context.Context, ns string, th model.Thresholds, resolveOwners bool) (*model.NamespaceReport, []string) {
	var warnings []string

	// Running pods only: Succeeded/Failed no longer consume anything and
	// their requests do not count for capacity; Pending has no metrics.
	pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return nil, []string{fmt.Sprintf("namespace %s skipped: failed to list pods: %v", ns, err)}
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}

	podMetrics, err := c.metrics.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		// Not a hard abort: a report without usage would be misleading,
		// so skip the namespace with a warning.
		return nil, []string{fmt.Sprintf("namespace %s skipped: failed to fetch metrics: %v", ns, err)}
	}

	// Matching strictly by namespace+pod name (the namespace dimension is
	// fixed by this loop): pod name -> container name -> usage. No
	// position-based joining — the ordering of the two APIs is not
	// guaranteed to be identical.
	usageByPod := make(map[string]containerUsage, len(podMetrics.Items))
	for _, pm := range podMetrics.Items {
		cu := make(containerUsage, len(pm.Containers))
		for _, cm := range pm.Containers {
			cu[cm.Name] = cm.Usage
		}
		usageByPod[pm.Name] = cu
	}

	nsReport := &model.NamespaceReport{Name: ns}

	rsCache := map[string]*appsv1.ReplicaSet{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		pr := buildPodReport(pod, usageByPod[pod.Name], th)
		if resolveOwners {
			pr.OwnerKind, pr.OwnerName = c.resolveTopOwner(ctx, pod, rsCache)
		}
		nsReport.Pods = append(nsReport.Pods, pr)

		nsReport.PodsTotal++
		nsReport.Stats.Add(pr.Stats)
		if pr.HasMetrics {
			nsReport.MeasuredStats.Add(pr.Stats)
		} else {
			nsReport.PodsWithoutMetrics++
		}

		// Track the worst pod-level limit utilization in the namespace.
		for _, f := range []model.Factor{pr.CPULimitUtilPct, pr.MemLimitUtilPct} {
			if f.Valid && (!nsReport.MaxPodLimitUtilPct.Valid || f.Value > nsReport.MaxPodLimitUtilPct.Value) {
				nsReport.MaxPodLimitUtilPct = f
			}
		}
	}

	m := nsReport.MeasuredStats
	nsReport.CPUFactor = model.ComputeFactor(m.CPURequestMilli, m.CPUUsageMilli, th.CapFactor)
	nsReport.MemFactor = model.ComputeFactor(m.MemRequestBytes, m.MemUsageBytes, th.CapFactor)
	nsReport.SortFactor = model.MaxFactor(nsReport.CPUFactor, nsReport.MemFactor)
	nsReport.CPULimitUtilPct = model.ComputeLimitUtilization(m.CPUUsageMilli, m.CPULimitMilli)
	nsReport.MemLimitUtilPct = model.ComputeLimitUtilization(m.MemUsageBytes, m.MemLimitBytes)
	nsReport.HoardSeverity = model.ClassifyHoarder(nsReport.CPUFactor, nsReport.MemFactor, th)
	// Namespace-level limit risk if the sum OR any single pod crosses the
	// threshold.
	nsReport.LimitRisk = model.IsLimitRisk(nsReport.CPULimitUtilPct, nsReport.MemLimitUtilPct, th) ||
		model.IsLimitRisk(nsReport.MaxPodLimitUtilPct, model.Factor{}, th)

	if nsReport.PodsWithoutMetrics > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"namespace %s: %d/%d pods without metrics (recently started?) — factors are based on measured pods only",
			ns, nsReport.PodsWithoutMetrics, nsReport.PodsTotal))
	}

	return nsReport, warnings
}

// buildPodReport combines spec and usage of one pod.
// usage may be nil (pod without metrics).
func buildPodReport(pod *corev1.Pod, usage containerUsage, th model.Thresholds) model.PodReport {
	pr := model.PodReport{
		Name:       pod.Name,
		Namespace:  pod.Namespace,
		HasMetrics: usage != nil,
	}

	// Regular containers: sum of requests/limits/usage.
	for i := range pod.Spec.Containers {
		ctr := &pod.Spec.Containers[i]
		var u corev1.ResourceList
		if usage != nil {
			u = usage[ctr.Name]
		}
		cr := buildContainerReport(ctr, u, false, th)
		pr.Containers = append(pr.Containers, cr)
		pr.Stats.Add(cr.Stats)
	}

	// Init containers: reported separately. Effectively the per-resource
	// maximum counts (the way the scheduler computes it), not the sum.
	// No usage — init containers have finished at runtime.
	for i := range pod.Spec.InitContainers {
		ctr := &pod.Spec.InitContainers[i]
		cr := buildContainerReport(ctr, nil, true, th)
		pr.Containers = append(pr.Containers, cr)
		maxStats(&pr.InitStats, cr.Stats)
	}

	pr.CPUFactor = model.ComputeFactor(pr.Stats.CPURequestMilli, pr.Stats.CPUUsageMilli, th.CapFactor)
	pr.MemFactor = model.ComputeFactor(pr.Stats.MemRequestBytes, pr.Stats.MemUsageBytes, th.CapFactor)
	pr.CPULimitUtilPct = model.ComputeLimitUtilization(pr.Stats.CPUUsageMilli, pr.Stats.CPULimitMilli)
	pr.MemLimitUtilPct = model.ComputeLimitUtilization(pr.Stats.MemUsageBytes, pr.Stats.MemLimitBytes)
	pr.LimitRisk = model.IsLimitRisk(pr.CPULimitUtilPct, pr.MemLimitUtilPct, th)

	if !pr.HasMetrics {
		// Without metrics the factors are meaningless -> force N/A.
		pr.CPUFactor = model.Factor{}
		pr.MemFactor = model.Factor{}
		pr.CPULimitUtilPct = model.Factor{}
		pr.MemLimitUtilPct = model.Factor{}
		pr.LimitRisk = false
	}

	pr.HoardSeverity = model.ClassifyHoarder(pr.CPUFactor, pr.MemFactor, th)

	return pr
}

func buildContainerReport(ctr *corev1.Container, usage corev1.ResourceList, init bool, th model.Thresholds) model.ContainerReport {
	cr := model.ContainerReport{
		Name:       ctr.Name,
		Init:       init,
		HasMetrics: usage != nil,
		Stats:      model.StatsFromResources(ctr.Resources.Requests, ctr.Resources.Limits, usage),
	}
	if init || !cr.HasMetrics {
		return cr
	}
	cr.CPUFactor = model.ComputeFactor(cr.Stats.CPURequestMilli, cr.Stats.CPUUsageMilli, th.CapFactor)
	cr.MemFactor = model.ComputeFactor(cr.Stats.MemRequestBytes, cr.Stats.MemUsageBytes, th.CapFactor)
	cr.CPULimitUtilPct = model.ComputeLimitUtilization(cr.Stats.CPUUsageMilli, cr.Stats.CPULimitMilli)
	cr.MemLimitUtilPct = model.ComputeLimitUtilization(cr.Stats.MemUsageBytes, cr.Stats.MemLimitBytes)
	return cr
}

// maxStats sets each field in dst to the maximum of dst and src (for the
// effective init container resources).
func maxStats(dst *model.ResourceStats, src model.ResourceStats) {
	if src.CPURequestMilli > dst.CPURequestMilli {
		dst.CPURequestMilli = src.CPURequestMilli
	}
	if src.CPULimitMilli > dst.CPULimitMilli {
		dst.CPULimitMilli = src.CPULimitMilli
	}
	if src.MemRequestBytes > dst.MemRequestBytes {
		dst.MemRequestBytes = src.MemRequestBytes
	}
	if src.MemLimitBytes > dst.MemLimitBytes {
		dst.MemLimitBytes = src.MemLimitBytes
	}
}

func summarize(r *model.Report) {
	s := model.Summary{NamespacesScanned: len(r.Namespaces)}
	for i := range r.Namespaces {
		ns := &r.Namespaces[i]
		s.PodsScanned += ns.PodsTotal
		s.Totals.Add(ns.Stats)
		switch ns.HoardSeverity {
		case model.SeverityWarning:
			s.HoardersWarning++
		case model.SeverityCritical:
			s.HoardersCritical++
		}
		if ns.LimitRisk {
			s.LimitRiskCount++
		}
	}
	r.Summary = s
}
