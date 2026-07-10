package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/shushyu/kubehoard/internal/model"
)

// WriteJSON serializes the complete report (including pod/container level)
// for further processing, e.g. with jq or in pipelines.
func WriteJSON(w io.Writer, r *model.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// LoadJSON reads back a report written with --format=json (counterpart to
// WriteJSON, basis for 'suggest --from').
func LoadJSON(path string) (*model.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read report %q: %w", path, err)
	}
	var r model.Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("report %q is not a valid kubehoard JSON report: %w", path, err)
	}
	if r.GeneratedAt.IsZero() && len(r.Namespaces) == 0 {
		return nil, fmt.Errorf("report %q does not look like a kubehoard report (neither generatedAt nor namespaces)", path)
	}
	return &r, nil
}
