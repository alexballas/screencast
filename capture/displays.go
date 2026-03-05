package capture

// Display describes a capturable monitor/display on platforms that support enumeration.
type Display struct {
	Index   int
	ID      string
	Name    string
	Width   uint32
	Height  uint32
	Primary bool
}

// ListDisplays returns available displays for selection.
// On unsupported platforms, it returns ErrNotImplemented.
func ListDisplays() ([]Display, error) {
	return listDisplays()
}
