package request

import (
	"errors"

	"go2tv.app/screencast/internal/apis"
	"github.com/godbus/dbus/v5"
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
	signal, err := apis.ListenOnSignal(interfaceName, responseMember)
	if err != nil {
		return Ended, nil, err
	}

	response := <-signal
	if len(response.Body) != 2 {
		return Ended, nil, ErrUnexpectedResponse
	}

	status := response.Body[0].(ResponseStatus)
	results := response.Body[1].(map[string]dbus.Variant)
	return status, results, nil
}
