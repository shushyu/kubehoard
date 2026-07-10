// Package plan implements the suggest/apply workflow: a scan is turned into
// request recommendations (suggest), stored as an editable YAML plan, and
// then applied to the owner workloads via strategic merge patch (apply).
// Patching the pod template triggers the rolling restart automatically.
package plan

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/shushyu/kubehoard/internal/model"
	"github.com/shushyu/kubehoard/internal/report"
)

// Observed documents the measured values a suggestion is based on —
// purely informational, ignored by apply.
type Observed struct {
	CPURequest string `json:"cpuRequest,omitempty"`
	MemRequest string `json:"memRequest,omitempty"`
	CPUUsage   string `json:"cpuUsage,omitempty"`
	MemUsage   string `json:"memUsage,omitempty"`
	Pods       int    `json:"pods,omitempty"`
}

// ContainerTarget is one planned change to exactly one container of a
// workload. Empty fields mean: leave unchanged.
type ContainerTarget struct {
	Namespace string `json:"namespace"`
	// Kind: Deployment | StatefulSet | DaemonSet
	Kind      string `json:"kind"`
	Workload  string `json:"workload"`
	Container string `json:"container"`

	CPURequest string `json:"cpuRequest,omitempty"`
	MemRequest string `json:"memRequest,omitempty"`
	CPULimit   string `json:"cpuLimit,omitempty"`
	MemLimit   string `json:"memLimit,omitempty"`

	Observed *Observed `json:"observed,omitempty"`
}

func (t ContainerTarget) String() string {
	return fmt.Sprintf("%s/%s %s container=%s", t.Namespace, t.Workload, t.Kind, t.Container)
}

// Plan is the serialized result of suggest and the input for apply.
type Plan struct {
	GeneratedAt time.Time         `json:"generatedAt"`
	Headroom    float64           `json:"headroom"`
	MinFactor   float64           `json:"minFactor"`
	Targets     []ContainerTarget `json:"targets"`
	// Skipped: containers that were flagged but cannot be patched
	// automatically (bare pods, jobs, ReplicaSets without a Deployment, ...).
	Skipped []string `json:"skipped,omitempty"`
}

// ---- Suggestion logic (pure, unit-tested) ----

const (
	minCPURequestMilli = 10
	mib                = int64(1) << 20
	gib                = int64(1) << 30
	minMemRequestBytes = 32 * mib
)

// SuggestCPURequestMilli: measured usage * headroom, floor 10m, rounded up
// to "round" steps (5m / 25m / 100m depending on magnitude).
func SuggestCPURequestMilli(usageMilli int64, headroom float64) int64 {
	v := int64(math.Ceil(float64(usageMilli) * headroom))
	if v < minCPURequestMilli {
		v = minCPURequestMilli
	}
	return roundUpTo(v, cpuStep(v))
}

func cpuStep(v int64) int64 {
	switch {
	case v <= 100:
		return 5
	case v <= 1000:
		return 25
	default:
		return 100
	}
}

// SuggestMemRequestBytes: same for memory, floor 32Mi,
// steps 16Mi / 64Mi / 256Mi.
func SuggestMemRequestBytes(usageBytes int64, headroom float64) int64 {
	v := int64(math.Ceil(float64(usageBytes) * headroom))
	if v < minMemRequestBytes {
		v = minMemRequestBytes
	}
	return roundUpTo(v, memStep(v))
}

func memStep(v int64) int64 {
	switch {
	case v <= 256*mib:
		return 16 * mib
	case v <= gib:
		return 64 * mib
	default:
		return 256 * mib
	}
}

func roundUpTo(v, step int64) int64 {
	return ((v + step - 1) / step) * step
}

// CPUQuantityString renders millicores as a Kubernetes quantity ("250m", "2").
func CPUQuantityString(milli int64) string {
	if milli%1000 == 0 {
		return fmt.Sprintf("%d", milli/1000)
	}
	return fmt.Sprintf("%dm", milli)
}

// MemQuantityString renders bytes as a Kubernetes quantity ("512Mi", "2Gi").
func MemQuantityString(bytes int64) string {
	if bytes%gib == 0 {
		return fmt.Sprintf("%dGi", bytes/gib)
	}
	// Round up to whole Mi so the plan stays readable.
	return fmt.Sprintf("%dMi", (bytes+mib-1)/mib)
}

// ---- Building a plan from a scan report ----

// Kinds apply can patch automatically.
var supportedKinds = map[string]bool{
	"Deployment":  true,
	"StatefulSet": true,
	"DaemonSet":   true,
}

type groupKey struct {
	ns, kind, workload, container string
}

type groupAgg struct {
	maxCPUUsage, maxMemUsage     int64
	maxCPURequest, maxMemRequest int64
	cpuHot, memHot               bool
	pods                         int
}

// BuildPlan groups flagged containers (factor >= minFactor) by their owner
// workload and derives suggestions from the highest measured usage across
// all replicas. Requires a report created with ResolveOwners. Limits are
// deliberately never touched (fields stay empty and can be set by hand in
// the plan).
func BuildPlan(rep *model.Report, headroom, minFactor float64) *Plan {
	groups := map[groupKey]*groupAgg{}
	skippedSet := map[string]bool{}

	hot := func(f model.Factor) bool { return f.Valid && f.Value >= minFactor }

	for ni := range rep.Namespaces {
		ns := &rep.Namespaces[ni]
		for pi := range ns.Pods {
			pod := &ns.Pods[pi]
			if !pod.HasMetrics {
				continue
			}
			for ci := range pod.Containers {
				ctr := &pod.Containers[ci]
				if ctr.Init || !ctr.HasMetrics {
					continue
				}
				if !hot(ctr.CPUFactor) && !hot(ctr.MemFactor) {
					continue
				}
				if pod.OwnerKind == "" {
					skippedSet[fmt.Sprintf("%s/%s (bare pod without a controller) container=%s", ns.Name, pod.Name, ctr.Name)] = true
					continue
				}
				if !supportedKinds[pod.OwnerKind] {
					skippedSet[fmt.Sprintf("%s/%s (%s not supported by apply) container=%s", ns.Name, pod.OwnerName, pod.OwnerKind, ctr.Name)] = true
					continue
				}

				k := groupKey{ns.Name, pod.OwnerKind, pod.OwnerName, ctr.Name}
				g := groups[k]
				if g == nil {
					g = &groupAgg{}
					groups[k] = g
				}
				g.pods++
				g.maxCPUUsage = maxI(g.maxCPUUsage, ctr.Stats.CPUUsageMilli)
				g.maxMemUsage = maxI(g.maxMemUsage, ctr.Stats.MemUsageBytes)
				g.maxCPURequest = maxI(g.maxCPURequest, ctr.Stats.CPURequestMilli)
				g.maxMemRequest = maxI(g.maxMemRequest, ctr.Stats.MemRequestBytes)
				g.cpuHot = g.cpuHot || hot(ctr.CPUFactor)
				g.memHot = g.memHot || hot(ctr.MemFactor)
			}
		}
	}

	p := &Plan{GeneratedAt: time.Now(), Headroom: headroom, MinFactor: minFactor}
	for k, g := range groups {
		t := ContainerTarget{
			Namespace: k.ns,
			Kind:      k.kind,
			Workload:  k.workload,
			Container: k.container,
			Observed: &Observed{
				CPURequest: report.FormatCPU(g.maxCPURequest),
				MemRequest: report.FormatMem(g.maxMemRequest),
				CPUUsage:   report.FormatCPU(g.maxCPUUsage),
				MemUsage:   report.FormatMem(g.maxMemUsage),
				Pods:       g.pods,
			},
		}
		// Only suggest the resource(s) that are actually overprovisioned —
		// the other one stays unchanged.
		if g.cpuHot {
			t.CPURequest = CPUQuantityString(SuggestCPURequestMilli(g.maxCPUUsage, headroom))
		}
		if g.memHot {
			t.MemRequest = MemQuantityString(SuggestMemRequestBytes(g.maxMemUsage, headroom))
		}
		p.Targets = append(p.Targets, t)
	}

	sort.Slice(p.Targets, func(i, j int) bool {
		a, b := p.Targets[i], p.Targets[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Workload != b.Workload {
			return a.Workload < b.Workload
		}
		return a.Container < b.Container
	})
	for s := range skippedSet {
		p.Skipped = append(p.Skipped, s)
	}
	sort.Strings(p.Skipped)
	return p
}

func maxI(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
