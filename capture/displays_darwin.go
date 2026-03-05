//go:build darwin

package capture

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=12.3
#cgo LDFLAGS: -mmacosx-version-min=12.3 -framework Foundation -framework ScreenCaptureKit -framework CoreMedia -framework CoreVideo -framework CoreGraphics
#include "capture_darwin.h"
*/
import "C"

import "fmt"

func listDisplays() ([]Display, error) {
	count := int(C.GetMacDisplayCount())
	if count <= 0 {
		return []Display{}, nil
	}

	out := make([]Display, 0, count)
	for i := 0; i < count; i++ {
		var displayID C.uint32_t
		var width C.uint32_t
		var height C.uint32_t
		var primary C.bool
		if C.GetMacDisplayInfo(C.int(i), &displayID, &width, &height, &primary) == C.bool(false) {
			continue
		}

		id := fmt.Sprintf("%d", uint32(displayID))
		isPrimary := primary != C.bool(false)
		name := fmt.Sprintf("Display %s", id)
		if isPrimary {
			name += " (primary)"
		}

		out = append(out, Display{
			Index:   i,
			ID:      id,
			Name:    name,
			Width:   uint32(width),
			Height:  uint32(height),
			Primary: isPrimary,
		})
	}

	return out, nil
}
