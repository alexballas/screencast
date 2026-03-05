//go:build windows

package capture

/*
#include "capture_windows.h"
*/
import "C"

import "fmt"

func listDisplays() ([]Display, error) {
	count := int(C.GetWinMonitorCount())
	if count <= 0 {
		return []Display{}, nil
	}

	out := make([]Display, 0, count)
	for i := 0; i < count; i++ {
		var width C.uint32_t
		var height C.uint32_t
		var primary C.bool
		if C.GetWinMonitorInfo(C.int(i), &width, &height, &primary) == C.bool(false) {
			continue
		}

		isPrimary := primary != C.bool(false)
		name := fmt.Sprintf("Monitor %d", i)
		if isPrimary {
			name += " (primary)"
		}

		out = append(out, Display{
			Index:   i,
			ID:      fmt.Sprintf("monitor-%d", i),
			Name:    name,
			Width:   uint32(width),
			Height:  uint32(height),
			Primary: isPrimary,
		})
	}

	return out, nil
}
