package model

import (
	"encoding/json"
	"testing"
)

func TestFactorJSONRoundtrip(t *testing.T) {
	cases := []struct {
		name string
		in   Factor
		want string
	}{
		{"N/A -> null", Factor{}, "null"},
		{"normal value", Factor{Value: 12.5, Valid: true}, `{"value":12.5}`},
		{"capped", Factor{Value: 1000, Valid: true, Capped: true}, `{"value":1000,"capped":true}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, err := json.Marshal(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != c.want {
				t.Fatalf("Marshal = %s, want %s", data, c.want)
			}
			var back Factor
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatal(err)
			}
			if back != c.in {
				t.Errorf("roundtrip: got %+v, want %+v", back, c.in)
			}
		})
	}
}

func TestReportJSONRoundtripKeepsFactorsAndOwners(t *testing.T) {
	in := NamespaceReport{
		Name:      "dev",
		CPUFactor: Factor{Value: 42, Valid: true},
		Pods: []PodReport{{
			Name: "web-abc", HasMetrics: true,
			OwnerKind: "Deployment", OwnerName: "web",
			Containers: []ContainerReport{{
				Name: "app", HasMetrics: true,
				Stats:     ResourceStats{CPURequestMilli: 2000, CPUUsageMilli: 40},
				CPUFactor: Factor{Value: 50, Valid: true},
				MemFactor: Factor{}, // N/A must stay N/A
			}},
		}},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out NamespaceReport
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	ctr := out.Pods[0].Containers[0]
	if !ctr.CPUFactor.Valid || ctr.CPUFactor.Value != 50 {
		t.Errorf("CPUFactor lost: %+v", ctr.CPUFactor)
	}
	if ctr.MemFactor.Valid {
		t.Errorf("N/A factor must not become valid after roundtrip: %+v", ctr.MemFactor)
	}
	if out.Pods[0].OwnerKind != "Deployment" || out.Pods[0].OwnerName != "web" {
		t.Errorf("owner info lost: %+v", out.Pods[0])
	}
	if ctr.Stats.CPURequestMilli != 2000 {
		t.Errorf("stats lost: %+v", ctr.Stats)
	}
}
