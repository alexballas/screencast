package hls

import (
	"os"
	"strconv"
	"strings"
)

func BoolEnv(name string, defaultValue bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if v == "" {
		return defaultValue
	}

	switch v {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	default:
		return defaultValue
	}
}

func IntEnvClamped(name string, defaultValue, minValue, maxValue int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return defaultValue
	}

	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultValue
	}

	if minValue <= maxValue {
		if n < minValue {
			n = minValue
		}
		if n > maxValue {
			n = maxValue
		}
	}

	return n
}
