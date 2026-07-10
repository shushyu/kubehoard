package model

import (
	corev1 "k8s.io/api/core/v1"
)

// ComputeFactor computes the overprovisioning factor request/usage.
//
// Special cases:
//   - request == 0 -> N/A (no request set, the factor is meaningless)
//   - usage  <= 0  -> factor clamped to cap (Capped=true) so no infinity
//     or absurd number ends up in the report
//   - factor > cap -> clamped as well
func ComputeFactor(request, usage int64, cap float64) Factor {
	if request <= 0 {
		return Factor{}
	}
	if usage <= 0 {
		return Factor{Value: cap, Valid: true, Capped: true}
	}
	f := float64(request) / float64(usage)
	if f > cap {
		return Factor{Value: cap, Valid: true, Capped: true}
	}
	return Factor{Value: f, Valid: true}
}

// ComputeLimitUtilization computes usage/limit in percent.
// Without a limit set -> N/A.
func ComputeLimitUtilization(usage, limit int64) Factor {
	if limit <= 0 {
		return Factor{}
	}
	return Factor{Value: float64(usage) / float64(limit) * 100.0, Valid: true}
}

// MaxFactor returns the larger of the two valid factors (0 if neither is
// valid).
func MaxFactor(a, b Factor) float64 {
	m := 0.0
	if a.Valid && a.Value > m {
		m = a.Value
	}
	if b.Valid && b.Value > m {
		m = b.Value
	}
	return m
}

// ClassifyHoarder classifies based on the larger of the two factors.
func ClassifyHoarder(cpu, mem Factor, th Thresholds) Severity {
	m := MaxFactor(cpu, mem)
	switch {
	case m >= th.CritFactor:
		return SeverityCritical
	case m >= th.WarnFactor:
		return SeverityWarning
	default:
		return SeverityOK
	}
}

// IsLimitRisk reports whether either limit utilization crosses the threshold.
func IsLimitRisk(cpuPct, memPct Factor, th Thresholds) bool {
	return (cpuPct.Valid && cpuPct.Value >= th.LimitWarnPct) ||
		(memPct.Valid && memPct.Value >= th.LimitWarnPct)
}

// CPUMilli extracts CPU from a ResourceList as millicores.
// resource.Quantity handles the parsing of "50m", "1", "2500m" etc. —
// MilliValue rounds up (ceil), which is the desired behavior for nanocore
// values from the metrics API ("12345678n").
func CPUMilli(rl corev1.ResourceList) int64 {
	q, ok := rl[corev1.ResourceCPU]
	if !ok {
		return 0
	}
	return q.MilliValue()
}

// MemBytes extracts memory from a ResourceList as bytes ("1Gi", "512Mi",
// "100M" etc. are resolved by resource.Quantity).
func MemBytes(rl corev1.ResourceList) int64 {
	q, ok := rl[corev1.ResourceMemory]
	if !ok {
		return 0
	}
	return q.Value()
}

// StatsFromResources builds ResourceStats from requests/limits/usage.
func StatsFromResources(requests, limits, usage corev1.ResourceList) ResourceStats {
	return ResourceStats{
		CPURequestMilli: CPUMilli(requests),
		CPULimitMilli:   CPUMilli(limits),
		CPUUsageMilli:   CPUMilli(usage),
		MemRequestBytes: MemBytes(requests),
		MemLimitBytes:   MemBytes(limits),
		MemUsageBytes:   MemBytes(usage),
	}
}
