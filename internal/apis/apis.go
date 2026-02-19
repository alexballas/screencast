package apis

import (
	"github.com/godbus/dbus/v5"
)

const (
	ObjectName        = "org.freedesktop.portal.Desktop"
	ObjectPath        = "/org/freedesktop/portal/desktop"
	CallBaseName      = "org.freedesktop.portal"
	PropertiesGetName = "org.freedesktop.DBus.Properties.Get"
)

func Call(callName string, args ...any) (any, error) {
	call, err := callOnObject(ObjectPath, callName, args...)
	if err != nil {
		return nil, err
	}

	var result any
	err = call.Store(&result)
	return result, err
}

func CallOnObject(path dbus.ObjectPath, callName string, args ...any) error {
	_, err := callOnObject(path, callName, args...)
	return err
}

func callOnObject(path dbus.ObjectPath, callName string, args ...any) (*dbus.Call, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, err
	}

	obj := conn.Object(ObjectName, path)
	call := obj.Call(callName, 0, args...)
	return call, call.Err
}

func GetProperty(interfaceName, property string) (any, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, err
	}

	obj := conn.Object(ObjectName, ObjectPath)
	call := obj.Call(PropertiesGetName, 0, interfaceName, property)
	if call.Err != nil {
		return nil, call.Err
	}

	var value any
	err = call.Store(&value)
	return value, err
}

func ListenOnSignal(path dbus.ObjectPath, iface, signalName string) (chan *dbus.Signal, error) {
	_, signal, err := ListenOnSignalWithConn(path, iface, signalName)
	return signal, err
}

func ListenOnSignalWithConn(path dbus.ObjectPath, iface, signalName string) (*dbus.Conn, chan *dbus.Signal, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, nil, err
	}
	if path == "" {
		path = ObjectPath
	}

	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(path),
		dbus.WithMatchInterface(iface),
		dbus.WithMatchMember(signalName),
	); err != nil {
		return nil, nil, err
	}

	signal := make(chan *dbus.Signal)
	conn.Signal(signal)
	return conn, signal, nil
}
