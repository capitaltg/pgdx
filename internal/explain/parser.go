package explain

import (
	"encoding/json"
	"fmt"
)

// Parse turns raw `EXPLAIN (FORMAT JSON)` output into a structured plan.
//
// The output is a JSON array with a single element. We tolerate extra fields and
// node types we don't model (D3: degrade, don't crash).
func Parse(raw []byte) (*ExplainOutput, error) {
	var outputs []ExplainOutput
	if err := json.Unmarshal(raw, &outputs); err != nil {
		return nil, fmt.Errorf("parse EXPLAIN JSON: %w", err)
	}
	if len(outputs) == 0 {
		return nil, fmt.Errorf("parse EXPLAIN JSON: empty output array")
	}
	return &outputs[0], nil
}
