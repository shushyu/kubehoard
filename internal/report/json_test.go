package report

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aleks/kubehoard/internal/model"
)

func TestWriteAndLoadJSONRoundtrip(t *testing.T) {
	in := &model.Report{
		GeneratedAt: time.Now().Truncate(time.Second),
		Server:      "https://api.example:6443",
		Thresholds:  model.Thresholds{WarnFactor: 10, CritFactor: 50, CapFactor: 1000, LimitWarnPct: 90},
		Namespaces: []model.NamespaceReport{{
			Name:      "dev",
			CPUFactor: model.Factor{Value: 42, Valid: true},
			MemFactor: model.Factor{Value: 1000, Valid: true, Capped: true},
			Pods: []model.PodReport{{
				Name: "web-abc", HasMetrics: true,
				OwnerKind: "Deployment", OwnerName: "web",
				Containers: []model.ContainerReport{{
					Name: "app", HasMetrics: true,
					Stats:     model.ResourceStats{CPURequestMilli: 2000, CPUUsageMilli: 40},
					CPUFactor: model.Factor{Value: 50, Valid: true},
				}},
			}},
		}},
	}

	path := filepath.Join(t.TempDir(), "report.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(f, in); err != nil {
		t.Fatal(err)
	}
	f.Close()

	out, err := LoadJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	ns := out.Namespaces[0]
	if !ns.CPUFactor.Valid || ns.CPUFactor.Value != 42 {
		t.Errorf("namespace CPUFactor: %+v", ns.CPUFactor)
	}
	if !ns.MemFactor.Capped {
		t.Errorf("capped flag lost: %+v", ns.MemFactor)
	}
	pod := ns.Pods[0]
	if pod.OwnerKind != "Deployment" || pod.OwnerName != "web" || !pod.HasMetrics {
		t.Errorf("pod fields lost: %+v", pod)
	}
	ctr := pod.Containers[0]
	if ctr.Stats.CPURequestMilli != 2000 || !ctr.CPUFactor.Valid {
		t.Errorf("container fields lost: %+v", ctr)
	}
	if out.Thresholds.WarnFactor != 10 {
		t.Errorf("thresholds lost: %+v", out.Thresholds)
	}
}

func TestLoadJSONRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.json")
	if err := os.WriteFile(path, []byte(`{"foo": "bar"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJSON(path); err == nil {
		t.Error("arbitrary JSON must not be accepted as a report")
	}
	if _, err := LoadJSON(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing file must return an error")
	}
}
