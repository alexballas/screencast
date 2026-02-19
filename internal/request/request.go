package request

import (
	"errors"
	"fmt"

	"github.com/godbus/dbus/v5"
	"go2tv.app/screencast/internal/apis"
)

var ErrUnexpectedResponse = errors.New("unexpected response from dbus")

const (
	interfaceName  = "org.freedesktop.portal.Request"
	responseMember = "Response"
	closeCallName  = interfaceName + ".Close"
)

type ResponseStatus = uint32

const (
	Success   ResponseStatus = 0
	Cancelled ResponseStatus = 1
	Ended     ResponseStatus = 2
)

func Close(path dbus.ObjectPath) error {
	return apis.CallOnObject(path, closeCallName)
}

func OnSignalResponse(path dbus.ObjectPath) (ResponseStatus, map[string]dbus.Variant, error) {
	conn, signal, err := apis.ListenOnSignalWithConn(path, interfaceName, responseMember)
	if err != nil {
		return Ended, nil, err
	}
	defer conn.RemoveSignal(signal)
	defer func() {
		_ = conn.RemoveMatchSignal(
			dbus.WithMatchObjectPath(path),
			dbus.WithMatchInterface(interfaceName),
			dbus.WithMatchMember(responseMember),
		)
	}()

	response := <-signal
	if response == nil {
		return Ended, nil, ErrUnexpectedResponse
	}
	if response.Path != path {
		return Ended, nil, fmt.Errorf("%w: unexpected signal path %s", ErrUnexpectedResponse, response.Path)
	}
	if len(response.Body) != 2 {
		return Ended, nil, ErrUnexpectedResponse
	}

	status, ok := response.Body[0].(uint32)
	if !ok {
		return Ended, nil, ErrUnexpectedResponse
	}
	results, ok := response.Body[1].(map[string]dbus.Variant)
	if !ok {
		return Ended, nil, ErrUnexpectedResponse
	}

	return ResponseStatus(status), results, nil
}
