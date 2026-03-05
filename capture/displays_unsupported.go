//go:build !windows && !darwin

package capture

func listDisplays() ([]Display, error) {
	return nil, ErrNotImplemented
}
