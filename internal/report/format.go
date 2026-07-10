// Package report renders a model.Report as a terminal table, JSON or HTML.
package report

import (
	"fmt"

	"github.com/shushyu/kubehoard/internal/model"
)

// FormatCPU renders millicores human-readable ("250m" or "1.50 cores").
func FormatCPU(milli int64) string {
	if milli >= 1000 {
		return fmt.Sprintf("%.2f cores", float64(milli)/1000.0)
	}
	return fmt.Sprintf("%dm", milli)
}

// FormatMem renders bytes human-readable (binary units).
func FormatMem(bytes int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
		tib = 1 << 40
	)
	switch {
	case bytes >= tib:
		return fmt.Sprintf("%.2f TiB", float64(bytes)/float64(tib))
	case bytes >= gib:
		return fmt.Sprintf("%.2f GiB", float64(bytes)/float64(gib))
	case bytes >= mib:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/float64(mib))
	case bytes >= kib:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/float64(kib))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// FormatCPUOrDash is FormatCPU, but renders "–" for unset values (0).
func FormatCPUOrDash(milli int64) string {
	if milli == 0 {
		return "–"
	}
	return FormatCPU(milli)
}

// FormatMemOrDash is FormatMem, but renders "–" for unset values (0).
func FormatMemOrDash(bytes int64) string {
	if bytes == 0 {
		return "–"
	}
	return FormatMem(bytes)
}

// FormatFactor renders an overprovisioning factor ("12.3x", "≥1000x", "N/A").
func FormatFactor(f model.Factor) string {
	if !f.Valid {
		return "N/A"
	}
	if f.Capped {
		return fmt.Sprintf("≥%.0fx", f.Value)
	}
	return fmt.Sprintf("%.1fx", f.Value)
}

// FormatPct renders a limit utilization ("87%", "N/A").
func FormatPct(f model.Factor) string {
	if !f.Valid {
		return "N/A"
	}
	return fmt.Sprintf("%.0f%%", f.Value)
}

// SeverityLabel returns a short terminal label.
func SeverityLabel(s model.Severity) string {
	switch s {
	case model.SeverityCritical:
		return "CRITICAL"
	case model.SeverityWarning:
		return "WARNING"
	default:
		return "ok"
	}
}
