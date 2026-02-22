package hls

import (
	"os"
	"strings"
)

func envDebugEnabled() bool {
	return strings.TrimSpace(os.Getenv("SCREENCAST_DEBUG")) == "1"
}
