// Package damengnames owns the deterministic target-column naming policy.
// Source identifiers must never be replaced with these names in source queries.
package damengnames

import (
	"fmt"
	"strings"
)

var replacements = map[string]string{
	"ROWID": "ROW_ID", "TRXID": "TRX_ID",
	"VERSIONS_STARTTIME": "VERSIONS_START_TIME", "VERSIONS_ENDTIME": "VERSIONS_END_TIME",
	"VERSIONS_STARTTRXID": "VERSIONS_START_TRX_ID", "VERSIONS_ENDTRXID": "VERSIONS_END_TRX_ID",
	"VERSIONS_OPERATION": "VERSIONS__OPERATION",
}

func Forbidden(name string) bool { return replacements[strings.ToUpper(name)] != "" }

// Plan reserves all original names before allocating replacements. The result
// has the same order as names and does not depend on map iteration order.
func Plan(names []string) ([]string, error) {
	used := make(map[string]bool, len(names))
	for _, name := range names {
		key := strings.ToUpper(name)
		if used[key] {
			return nil, fmt.Errorf("目标列名冲突: %s", key)
		}
		used[key] = true
	}
	out := make([]string, len(names))
	for i, name := range names {
		key := strings.ToUpper(name)
		if base := replacements[key]; base != "" {
			key = base
			for suffix := 2; used[key]; suffix++ {
				key = fmt.Sprintf("%s_%d", base, suffix)
			}
			used[key] = true
		}
		out[i] = key
	}
	return out, nil
}
