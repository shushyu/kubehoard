package model

import (
	"math"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestComputeFactor(t *testing.T) {
	tests := []struct {
		name       string
		request    int64
		usage      int64
		cap        float64
		wantValid  bool
		wantCapped bool
		wantValue  float64
	}{
		{"normal factor", 1000, 100, 1000, true, false, 10.0},
		{"factor exactly 1", 250, 250, 1000, true, false, 1.0},
		{"underprovisioning < 1", 100, 400, 1000, true, false, 0.25},
		{"no request -> N/A", 0, 100, 1000, false, false, 0},
		{"negative request -> N/A", -5, 100, 1000, false, false, 0},
		{"zero usage -> cap instead of infinity", 500, 0, 1000, true, true, 1000},
		{"negative usage -> cap", 500, -1, 1000, true, true, 1000},
		{"factor above cap -> clamped", 100000, 1, 1000, true, true, 1000},
		{"factor just below cap", 999, 1, 1000, true, false, 999},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeFactor(tt.request, tt.usage, tt.cap)
			if got.Valid != tt.wantValid {
				t.Fatalf("Valid = %v, want %v", got.Valid, tt.wantValid)
			}
			if !tt.wantValid {
				return
			}
			if got.Capped != tt.wantCapped {
				t.Errorf("Capped = %v, want %v", got.Capped, tt.wantCapped)
			}
			if !almostEqual(got.Value, tt.wantValue) {
				t.Errorf("Value = %v, want %v", got.Value, tt.wantValue)
			}
		})
	}
}

func TestComputeLimitUtilization(t *testing.T) {
	got := ComputeLimitUtilization(900, 1000)
	if !got.Valid || !almostEqual(got.Value, 90.0) {
		t.Errorf("900/1000 = %+v, want 90%% valid", got)
	}

	got = ComputeLimitUtilization(1200, 1000)
	if !got.Valid || !almostEqual(got.Value, 120.0) {
		t.Errorf("overshoot: got %+v, want 120%%", got)
	}

	got = ComputeLimitUtilization(500, 0)
	if got.Valid {
		t.Errorf("without a limit the result must be N/A, got %+v", got)
	}
}

func TestClassifyHoarder(t *testing.T) {
	th := Thresholds{WarnFactor: 10, CritFactor: 50, CapFactor: 1000, LimitWarnPct: 90}

	cases := []struct {
		name string
		cpu  Factor
		mem  Factor
		want Severity
	}{
		{"both unremarkable", Factor{Value: 2, Valid: true}, Factor{Value: 3, Valid: true}, SeverityOK},
		{"cpu above warn", Factor{Value: 12, Valid: true}, Factor{Value: 1, Valid: true}, SeverityWarning},
		{"memory above crit", Factor{Value: 1, Valid: true}, Factor{Value: 80, Valid: true}, SeverityCritical},
		{"exactly on warn threshold", Factor{Value: 10, Valid: true}, Factor{}, SeverityWarning},
		{"both N/A", Factor{}, Factor{}, SeverityOK},
		{"capped factor counts", Factor{Value: 1000, Valid: true, Capped: true}, Factor{}, SeverityCritical},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyHoarder(c.cpu, c.mem, th); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestIsLimitRisk(t *testing.T) {
	th := Thresholds{LimitWarnPct: 90}
	if !IsLimitRisk(Factor{Value: 95, Valid: true}, Factor{}, th) {
		t.Error("95%% CPU limit utilization must be a risk")
	}
	if IsLimitRisk(Factor{Value: 50, Valid: true}, Factor{Value: 89.9, Valid: true}, th) {
		t.Error("below threshold must not be a risk")
	}
	if IsLimitRisk(Factor{}, Factor{}, th) {
		t.Error("without limits (N/A) no risk must be reported")
	}
}

// Quantity handling: we deliberately rely on resource.Quantity from
// apimachinery instead of hand-rolled string parsing. These tests document
// the expected conversion semantics.
func TestQuantityConversion(t *testing.T) {
	rl := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("50m"),
		corev1.ResourceMemory: resource.MustParse("1Gi"),
	}
	if got := CPUMilli(rl); got != 50 {
		t.Errorf("50m -> %d millicores, want 50", got)
	}
	if got := MemBytes(rl); got != 1073741824 {
		t.Errorf("1Gi -> %d bytes, want 1073741824", got)
	}

	// Whole cores.
	rl[corev1.ResourceCPU] = resource.MustParse("2")
	if got := CPUMilli(rl); got != 2000 {
		t.Errorf("2 cores -> %d millicores, want 2000", got)
	}

	// Nanocores as returned by the metrics API: MilliValue rounds up.
	rl[corev1.ResourceCPU] = resource.MustParse("12345678n")
	if got := CPUMilli(rl); got != 13 {
		t.Errorf("12345678n -> %d millicores, want 13 (ceil)", got)
	}

	// Decimal suffix M (not Mi).
	rl[corev1.ResourceMemory] = resource.MustParse("100M")
	if got := MemBytes(rl); got != 100_000_000 {
		t.Errorf("100M -> %d bytes, want 100000000", got)
	}

	// Missing keys -> 0.
	empty := corev1.ResourceList{}
	if CPUMilli(empty) != 0 || MemBytes(empty) != 0 {
		t.Error("missing resources must yield 0")
	}
}

func TestStatsFromResources(t *testing.T) {
	req := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("500m"),
		corev1.ResourceMemory: resource.MustParse("512Mi"),
	}
	lim := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1"),
		corev1.ResourceMemory: resource.MustParse("1Gi"),
	}
	use := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("25m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	}
	s := StatsFromResources(req, lim, use)
	want := ResourceStats{
		CPURequestMilli: 500,
		CPULimitMilli:   1000,
		CPUUsageMilli:   25,
		MemRequestBytes: 512 * 1024 * 1024,
		MemLimitBytes:   1024 * 1024 * 1024,
		MemUsageBytes:   128 * 1024 * 1024,
	}
	if s != want {
		t.Errorf("StatsFromResources = %+v, want %+v", s, want)
	}
}

func TestResourceStatsAdd(t *testing.T) {
	a := ResourceStats{CPURequestMilli: 100, MemRequestBytes: 1000, CPUUsageMilli: 10}
	a.Add(ResourceStats{CPURequestMilli: 50, MemRequestBytes: 500, CPUUsageMilli: 5, MemLimitBytes: 42})
	want := ResourceStats{CPURequestMilli: 150, MemRequestBytes: 1500, CPUUsageMilli: 15, MemLimitBytes: 42}
	if a != want {
		t.Errorf("Add = %+v, want %+v", a, want)
	}
}
