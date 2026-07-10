package report

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/shushyu/kubehoard/internal/model"
)

// DetailMode controls whether a container breakdown is printed below the
// namespace table.
type DetailMode int

const (
	DetailNone     DetailMode = iota // namespace table only
	DetailHoarders                   // container detail for hoarding pods (>= warning)
	DetailAll                        // container detail for all pods
)

// ParseDetailMode parses the --detail flag value.
func ParseDetailMode(s string) (DetailMode, error) {
	switch s {
	case "none":
		return DetailNone, nil
	case "hoarders":
		return DetailHoarders, nil
	case "all":
		return DetailAll, nil
	default:
		return DetailNone, fmt.Errorf("unknown --detail mode %q (allowed: none, hoarders, all)", s)
	}
}

// WriteTerminal renders the report as a plain table for CLI use.
func WriteTerminal(w io.Writer, r *model.Report, detail DetailMode) error {
	fmt.Fprintf(w, "kubehoard – %s\n", r.GeneratedAt.Format("2006-01-02 15:04:05"))
	if r.Server != "" {
		fmt.Fprintf(w, "Cluster: %s\n", r.Server)
	}
	s := r.Summary
	fmt.Fprintf(w, "Namespaces: %d | Pods: %d | Hoarders: %d warning / %d critical | Limit risk: %d\n",
		s.NamespacesScanned, s.PodsScanned, s.HoardersWarning, s.HoardersCritical, s.LimitRiskCount)
	fmt.Fprintf(w, "CPU total: requests %s, usage %s\n",
		FormatCPU(s.Totals.CPURequestMilli), FormatCPU(s.Totals.CPUUsageMilli))
	fmt.Fprintf(w, "Mem total: requests %s, usage %s\n\n",
		FormatMem(s.Totals.MemRequestBytes), FormatMem(s.Totals.MemUsageBytes))

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tPODS\tCPU REQ\tCPU USE\tCPU FACTOR\tMEM REQ\tMEM USE\tMEM FACTOR\tLIMIT USE (CPU/MEM)\tSTATUS")
	for i := range r.Namespaces {
		ns := &r.Namespaces[i]
		status := SeverityLabel(ns.HoardSeverity)
		if ns.LimitRisk {
			status += " +LIMIT"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s/%s\t%s\n",
			ns.Name,
			ns.PodsTotal,
			FormatCPU(ns.Stats.CPURequestMilli),
			FormatCPU(ns.Stats.CPUUsageMilli),
			FormatFactor(ns.CPUFactor),
			FormatMem(ns.Stats.MemRequestBytes),
			FormatMem(ns.Stats.MemUsageBytes),
			FormatFactor(ns.MemFactor),
			FormatPct(ns.CPULimitUtilPct),
			FormatPct(ns.MemLimitUtilPct),
			status,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if detail != DetailNone {
		if err := writeContainerDetail(w, r, detail); err != nil {
			return err
		}
	}

	if len(r.Warnings) > 0 {
		fmt.Fprintln(w, "\nNotes:")
		for _, warn := range r.Warnings {
			fmt.Fprintf(w, "  - %s\n", warn)
		}
	}
	return nil
}

// writeContainerDetail lists pods with their containers (requests, limits,
// usage, factors). In DetailHoarders mode only pods classified as hoarders
// (>= warning) are listed.
func writeContainerDetail(w io.Writer, r *model.Report, mode DetailMode) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	headerWritten := false

	for i := range r.Namespaces {
		ns := &r.Namespaces[i]
		for j := range ns.Pods {
			pod := &ns.Pods[j]
			hot := pod.HoardSeverity == model.SeverityWarning || pod.HoardSeverity == model.SeverityCritical
			if mode == DetailHoarders && !hot {
				continue
			}
			if !headerWritten {
				title := "Container detail (hoarding pods)"
				if mode == DetailAll {
					title = "Container detail (all pods)"
				}
				fmt.Fprintf(w, "\n%s:\n", title)
				fmt.Fprintln(tw, "NAMESPACE\tPOD\tCONTAINER\tCPU REQ\tCPU LIM\tCPU USE\tCPU FACTOR\tMEM REQ\tMEM LIM\tMEM USE\tMEM FACTOR")
				headerWritten = true
			}
			for k := range pod.Containers {
				ctr := &pod.Containers[k]
				name := ctr.Name
				if ctr.Init {
					name += " (init)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					ns.Name, pod.Name, name,
					FormatCPU(ctr.Stats.CPURequestMilli),
					FormatCPUOrDash(ctr.Stats.CPULimitMilli),
					FormatCPU(ctr.Stats.CPUUsageMilli),
					FormatFactor(ctr.CPUFactor),
					FormatMem(ctr.Stats.MemRequestBytes),
					FormatMemOrDash(ctr.Stats.MemLimitBytes),
					FormatMem(ctr.Stats.MemUsageBytes),
					FormatFactor(ctr.MemFactor),
				)
			}
		}
	}
	if !headerWritten {
		fmt.Fprintln(w, "\nContainer detail: no hoarding pods above the threshold.")
		return nil
	}
	return tw.Flush()
}
