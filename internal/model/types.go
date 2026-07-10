// Package model contains kubehoard's data structures and pure calculation
// logic (factors, classification) — deliberately free of Kube API
// dependencies so the logic stays testable in isolation.
package model

import (
	"encoding/json"
	"time"
)

// ResourceStats holds CPU in millicores and memory in bytes.
// All values are sums over the respective scope (container, pod, namespace).
type ResourceStats struct {
	CPURequestMilli int64 `json:"cpuRequestMilli"`
	CPULimitMilli   int64 `json:"cpuLimitMilli"`
	CPUUsageMilli   int64 `json:"cpuUsageMilli"`
	MemRequestBytes int64 `json:"memRequestBytes"`
	MemLimitBytes   int64 `json:"memLimitBytes"`
	MemUsageBytes   int64 `json:"memUsageBytes"`
}

// Add adds o's values onto s.
func (s *ResourceStats) Add(o ResourceStats) {
	s.CPURequestMilli += o.CPURequestMilli
	s.CPULimitMilli += o.CPULimitMilli
	s.CPUUsageMilli += o.CPUUsageMilli
	s.MemRequestBytes += o.MemRequestBytes
	s.MemLimitBytes += o.MemLimitBytes
	s.MemUsageBytes += o.MemUsageBytes
}

// Factor represents a computed ratio (overprovisioning factor or limit
// utilization in percent). Valid=false means "N/A" (e.g. no request or no
// limit set). Capped=true means usage was (near) zero and the value was
// clamped to the configured maximum instead of reporting infinity.
type Factor struct {
	Value  float64 `json:"-"`
	Valid  bool    `json:"-"`
	Capped bool    `json:"-"`
}

// MarshalJSON renders null for N/A, otherwise an object with value/capped.
func (f Factor) MarshalJSON() ([]byte, error) {
	if !f.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(struct {
		Value  float64 `json:"value"`
		Capped bool    `json:"capped,omitempty"`
	}{f.Value, f.Capped})
}

// UnmarshalJSON is the inverse, so a report written with --format=json can
// be read back (suggest --from).
func (f *Factor) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*f = Factor{}
		return nil
	}
	var v struct {
		Value  float64 `json:"value"`
		Capped bool    `json:"capped"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*f = Factor{Value: v.Value, Valid: true, Capped: v.Capped}
	return nil
}

// Severity classifies a namespace or pod.
type Severity string

const (
	SeverityOK       Severity = "ok"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Thresholds are the configurable classification thresholds.
type Thresholds struct {
	// Overprovisioning factor (request / usage).
	WarnFactor float64 `json:"warnFactor"` // e.g. 10
	CritFactor float64 `json:"critFactor"` // e.g. 50
	// Clamp for the factor when usage is near zero (instead of infinity).
	CapFactor float64 `json:"capFactor"` // e.g. 1000
	// Limit utilization in percent (usage / limit * 100).
	LimitWarnPct float64 `json:"limitWarnPct"` // e.g. 90
}

// ContainerReport is the evaluation of a single container.
type ContainerReport struct {
	Name       string        `json:"name"`
	Init       bool          `json:"init"`
	Stats      ResourceStats `json:"stats"`
	HasMetrics bool          `json:"hasMetrics"`

	CPUFactor Factor `json:"cpuFactor"`
	MemFactor Factor `json:"memFactor"`

	CPULimitUtilPct Factor `json:"cpuLimitUtilPct"`
	MemLimitUtilPct Factor `json:"memLimitUtilPct"`
}

// PodReport is the evaluation of a pod. Stats sums regular containers only.
// Init containers are reported separately (InitStats holds the per-resource
// maximum — the same way the scheduler computes effective requests) and are
// NOT part of the overprovisioning factors, since they consume nothing at
// runtime.
type PodReport struct {
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace"`
	Containers []ContainerReport `json:"containers"`

	Stats     ResourceStats `json:"stats"`
	InitStats ResourceStats `json:"initStats"`

	HasMetrics bool `json:"hasMetrics"`

	CPUFactor Factor `json:"cpuFactor"`
	MemFactor Factor `json:"memFactor"`

	CPULimitUtilPct Factor `json:"cpuLimitUtilPct"`
	MemLimitUtilPct Factor `json:"memLimitUtilPct"`

	LimitRisk     bool     `json:"limitRisk"`
	HoardSeverity Severity `json:"hoardSeverity"`

	// Owner workload (resolved via owner references, ReplicaSet ->
	// Deployment). Only populated when Options.ResolveOwners is set
	// (suggest workflow).
	OwnerKind string `json:"ownerKind,omitempty"`
	OwnerName string `json:"ownerName,omitempty"`
}

// NamespaceReport is the namespace-level aggregation.
type NamespaceReport struct {
	Name string      `json:"name"`
	Pods []PodReport `json:"pods"`

	// Stats: sum over all scanned (Running) pods.
	Stats ResourceStats `json:"stats"`
	// MeasuredStats: sum over pods that had metrics only. Factors are
	// computed from these, so freshly started pods without metrics do not
	// skew the result.
	MeasuredStats ResourceStats `json:"measuredStats"`

	PodsTotal          int `json:"podsTotal"`
	PodsWithoutMetrics int `json:"podsWithoutMetrics"`

	CPUFactor Factor `json:"cpuFactor"`
	MemFactor Factor `json:"memFactor"`
	// SortFactor = max(CPUFactor, MemFactor); basis for the ranking.
	SortFactor float64 `json:"sortFactor"`

	CPULimitUtilPct Factor `json:"cpuLimitUtilPct"`
	MemLimitUtilPct Factor `json:"memLimitUtilPct"`
	// MaxPodLimitUtilPct: highest limit utilization of any single pod in
	// the namespace (CPU or memory) — namespace sums can hide outliers.
	MaxPodLimitUtilPct Factor `json:"maxPodLimitUtilPct"`

	HoardSeverity Severity `json:"hoardSeverity"`
	LimitRisk     bool     `json:"limitRisk"`
}

// Summary holds the headline numbers for the report.
type Summary struct {
	NamespacesScanned int           `json:"namespacesScanned"`
	PodsScanned       int           `json:"podsScanned"`
	Totals            ResourceStats `json:"totals"`
	HoardersWarning   int           `json:"hoardersWarning"`
	HoardersCritical  int           `json:"hoardersCritical"`
	LimitRiskCount    int           `json:"limitRiskCount"`
}

// Report is the complete result of one scan.
type Report struct {
	GeneratedAt time.Time         `json:"generatedAt"`
	Server      string            `json:"server"`
	Thresholds  Thresholds        `json:"thresholds"`
	Summary     Summary           `json:"summary"`
	Namespaces  []NamespaceReport `json:"namespaces"`
	Warnings    []string          `json:"warnings,omitempty"`
}
