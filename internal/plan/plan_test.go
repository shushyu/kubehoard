package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shushyu/kubehoard/internal/model"
)

func TestSuggestCPURequestMilli(t *testing.T) {
	cases := []struct {
		usage    int64
		headroom float64
		want     int64
	}{
		{0, 2.0, 10},     // floor
		{3, 2.0, 10},     // 6m -> floor 10m
		{40, 2.0, 80},    // 80m, already on a 5m step
		{41, 2.0, 85},    // 82m -> rounded up to 5m
		{60, 2.0, 125},   // 120m -> step 25m
		{600, 2.0, 1200}, // 1200m -> step 100m
		{100, 1.5, 150},
	}
	for _, c := range cases {
		if got := SuggestCPURequestMilli(c.usage, c.headroom); got != c.want {
			t.Errorf("SuggestCPURequestMilli(%d, %.1f) = %d, want %d", c.usage, c.headroom, got, c.want)
		}
	}
}

func TestSuggestMemRequestBytes(t *testing.T) {
	cases := []struct {
		usageMi  int64
		headroom float64
		wantMi   int64
	}{
		{0, 2.0, 32},     // floor 32Mi
		{10, 2.0, 32},    // 20Mi -> floor
		{50, 2.0, 112},   // 100Mi -> step 16Mi
		{200, 2.0, 448},  // 400Mi -> step 64Mi
		{700, 2.0, 1536}, // 1400Mi -> step 256Mi
	}
	for _, c := range cases {
		got := SuggestMemRequestBytes(c.usageMi*mib, c.headroom)
		if got != c.wantMi*mib {
			t.Errorf("SuggestMemRequestBytes(%dMi, %.1f) = %dMi, want %dMi",
				c.usageMi, c.headroom, got/mib, c.wantMi)
		}
	}
}

func TestQuantityStrings(t *testing.T) {
	if got := CPUQuantityString(250); got != "250m" {
		t.Errorf("CPUQuantityString(250) = %q", got)
	}
	if got := CPUQuantityString(2000); got != "2" {
		t.Errorf("CPUQuantityString(2000) = %q", got)
	}
	if got := MemQuantityString(2 * gib); got != "2Gi" {
		t.Errorf("MemQuantityString(2Gi) = %q", got)
	}
	if got := MemQuantityString(448 * mib); got != "448Mi" {
		t.Errorf("MemQuantityString(448Mi) = %q", got)
	}
}

func f(v float64) model.Factor { return model.Factor{Value: v, Valid: true} }

func testReport() *model.Report {
	return &model.Report{
		Namespaces: []model.NamespaceReport{{
			Name: "dev",
			Pods: []model.PodReport{
				{
					Name: "web-abc", HasMetrics: true,
					OwnerKind: "Deployment", OwnerName: "web",
					Containers: []model.ContainerReport{
						{
							Name: "app", HasMetrics: true,
							Stats:     model.ResourceStats{CPURequestMilli: 2000, CPUUsageMilli: 40, MemRequestBytes: 4 * gib, MemUsageBytes: 100 * mib},
							CPUFactor: f(50), MemFactor: f(40.96),
						},
						{ // unremarkable sidecar
							Name: "proxy", HasMetrics: true,
							Stats:     model.ResourceStats{CPURequestMilli: 100, CPUUsageMilli: 50},
							CPUFactor: f(2),
						},
					},
				},
				{ // second replica with higher usage
					Name: "web-def", HasMetrics: true,
					OwnerKind: "Deployment", OwnerName: "web",
					Containers: []model.ContainerReport{{
						Name: "app", HasMetrics: true,
						Stats:     model.ResourceStats{CPURequestMilli: 2000, CPUUsageMilli: 60, MemRequestBytes: 4 * gib, MemUsageBytes: 150 * mib},
						CPUFactor: f(33.3), MemFactor: f(27.3),
					}},
				},
				{ // Job -> must end up in Skipped
					Name: "batch-xyz", HasMetrics: true,
					OwnerKind: "Job", OwnerName: "batch",
					Containers: []model.ContainerReport{{
						Name: "runner", HasMetrics: true,
						Stats:     model.ResourceStats{CPURequestMilli: 1000, CPUUsageMilli: 10},
						CPUFactor: f(100),
					}},
				},
			},
		}},
	}
}

func TestBuildPlan(t *testing.T) {
	p := BuildPlan(testReport(), 2.0, 10)

	if len(p.Targets) != 1 {
		t.Fatalf("want 1 target (replicas grouped, sidecar/job excluded), got %d: %+v", len(p.Targets), p.Targets)
	}
	tg := p.Targets[0]
	if tg.Kind != "Deployment" || tg.Workload != "web" || tg.Container != "app" {
		t.Errorf("wrong target: %+v", tg)
	}
	// max usage across replicas: 60m CPU, 150Mi mem, headroom 2.0
	if tg.CPURequest != CPUQuantityString(SuggestCPURequestMilli(60, 2.0)) {
		t.Errorf("CPURequest = %q", tg.CPURequest)
	}
	if tg.MemRequest != MemQuantityString(SuggestMemRequestBytes(150*mib, 2.0)) {
		t.Errorf("MemRequest = %q", tg.MemRequest)
	}
	if tg.CPULimit != "" || tg.MemLimit != "" {
		t.Errorf("limits must never be suggested automatically: %+v", tg)
	}
	if tg.Observed == nil || tg.Observed.Pods != 2 {
		t.Errorf("Observed.Pods = %+v, want 2", tg.Observed)
	}
	if len(p.Skipped) != 1 || !strings.Contains(p.Skipped[0], "Job") {
		t.Errorf("the Job must show up in Skipped: %+v", p.Skipped)
	}
}

func TestBuildStrategicMergePatch(t *testing.T) {
	tg := ContainerTarget{
		Namespace: "dev", Kind: "Deployment", Workload: "web", Container: "app",
		CPURequest: "150m", MemRequest: "384Mi",
	}
	data, err := BuildStrategicMergePatch(tg)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	containers := got["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	c := containers[0].(map[string]any)
	if c["name"] != "app" {
		t.Errorf("container name (merge key) missing/wrong: %v", c)
	}
	res := c["resources"].(map[string]any)
	req := res["requests"].(map[string]any)
	if req["cpu"] != "150m" || req["memory"] != "384Mi" {
		t.Errorf("wrong requests: %v", req)
	}
	if _, hasLimits := res["limits"]; hasLimits {
		t.Errorf("limits must not appear in the patch when unset: %v", res)
	}
}

func TestValidate(t *testing.T) {
	base := ContainerTarget{Namespace: "dev", Kind: "Deployment", Workload: "web", Container: "app", CPURequest: "100m"}

	if err := Validate(base); err != nil {
		t.Errorf("valid target rejected: %v", err)
	}

	bad := base
	bad.CPURequest = "100 millicores"
	if err := Validate(bad); err == nil {
		t.Error("invalid quantity must be rejected")
	}

	bad = base
	bad.Kind = "Job"
	if err := Validate(bad); err == nil {
		t.Error("unsupported kind must be rejected")
	}

	bad = base
	bad.CPURequest = ""
	if err := Validate(bad); err == nil {
		t.Error("a target without any change must be rejected")
	}
}
