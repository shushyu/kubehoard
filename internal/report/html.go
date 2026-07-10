package report

import (
	_ "embed"
	"html/template"
	"io"

	"github.com/shushyu/kubehoard/internal/model"
)

//go:embed report.html.tmpl
var htmlTemplate string

// htmlView is the template view model: bar widths are precomputed so the
// template needs no arithmetic.
type htmlView struct {
	Report     *model.Report
	Namespaces []nsView
}

type nsView struct {
	*model.NamespaceReport
	CPUReqBarPct   float64
	CPUUseBarPct   float64
	MemReqBarPct   float64
	MemUseBarPct   float64
	FactorLabel    string // larger of the two factors, for the ranking label
	SeverityString string
}

func newHTMLView(r *model.Report) htmlView {
	// Scale per resource: largest value (request or usage) across all
	// namespaces, so all bars are comparable.
	var maxCPU, maxMem int64 = 1, 1
	for i := range r.Namespaces {
		ns := &r.Namespaces[i]
		maxCPU = max64(maxCPU, ns.Stats.CPURequestMilli, ns.Stats.CPUUsageMilli)
		maxMem = max64(maxMem, ns.Stats.MemRequestBytes, ns.Stats.MemUsageBytes)
	}

	v := htmlView{Report: r}
	for i := range r.Namespaces {
		ns := &r.Namespaces[i]
		v.Namespaces = append(v.Namespaces, nsView{
			NamespaceReport: ns,
			CPUReqBarPct:    pct(ns.Stats.CPURequestMilli, maxCPU),
			CPUUseBarPct:    pct(ns.Stats.CPUUsageMilli, maxCPU),
			MemReqBarPct:    pct(ns.Stats.MemRequestBytes, maxMem),
			MemUseBarPct:    pct(ns.Stats.MemUsageBytes, maxMem),
			FactorLabel: FormatFactor(model.Factor{
				Value: ns.SortFactor,
				Valid: ns.SortFactor > 0,
				Capped: (ns.CPUFactor.Capped && ns.CPUFactor.Value == ns.SortFactor) ||
					(ns.MemFactor.Capped && ns.MemFactor.Value == ns.SortFactor),
			}),
			SeverityString: string(ns.HoardSeverity),
		})
	}
	return v
}

func pct(val, max int64) float64 {
	if max <= 0 {
		return 0
	}
	p := float64(val) / float64(max) * 100.0
	// Minimum visible width for values > 0, so the bar does not vanish.
	if val > 0 && p < 0.5 {
		p = 0.5
	}
	return p
}

func max64(vals ...int64) int64 {
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

// WriteHTML renders the report as a self-contained HTML page.
func WriteHTML(w io.Writer, r *model.Report) error {
	th := r.Thresholds
	funcs := template.FuncMap{
		"fmtCPU":    FormatCPU,
		"fmtMem":    FormatMem,
		"fmtCPULim": FormatCPUOrDash,
		"fmtMemLim": FormatMemOrDash,
		"fmtFactor": FormatFactor,
		"fmtPct":    FormatPct,
		"add1":      func(i int) int { return i + 1 },
		"factorHot": func(f model.Factor) bool { return f.Valid && f.Value >= th.WarnFactor },
		"podHot":    func(s model.Severity) bool { return s == model.SeverityWarning || s == model.SeverityCritical },
	}
	tmpl, err := template.New("report").Funcs(funcs).Parse(htmlTemplate)
	if err != nil {
		return err
	}
	return tmpl.Execute(w, newHTMLView(r))
}
