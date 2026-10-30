package request

import (
	"errors"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestAwaitResponse(t *testing.T) {
	const reqPath = dbus.ObjectPath("/org/freedesktop/portal/desktop/request/1_1/go2tv")
	response := &dbus.Signal{
		Path: reqPath,
		Name: interfaceName + "." + responseMember,
		Body: []any{uint32(Success), map[string]dbus.Variant{"session_handle": dbus.MakeVariant("s")}},
	}

	tt := []struct {
		name    string
		signals []*dbus.Signal
		status  ResponseStatus
		wantErr error
	}{
		{
			name:    "response only",
			signals: []*dbus.Signal{response},
			status:  Success,
		},
		{
			// Fyne listens for theme changes on the shared session bus; KDE
			// emits these while the share dialog is open.
			name: "skips portal SettingChanged on shared bus",
			signals: []*dbus.Signal{
				{
					Path: "/org/freedesktop/portal/desktop",
					Name: "org.freedesktop.portal.Settings.SettingChanged",
					Body: []any{"org.freedesktop.appearance", "color-scheme", dbus.MakeVariant(uint32(1))},
				},
				response,
			},
			status: Success,
		},
		{
			name: "skips response of another request",
			signals: []*dbus.Signal{
				{Path: reqPath + "_other", Name: response.Name, Body: response.Body},
				response,
			},
			status: Success,
		},
		{
			name:    "channel closed without response",
			signals: nil,
			status:  Ended,
			wantErr: ErrUnexpectedResponse,
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan *dbus.Signal, len(tc.signals))
			for _, s := range tc.signals {
				ch <- s
			}
			close(ch)

			status, _, err := awaitResponse(ch, reqPath)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
		})
	}
}
