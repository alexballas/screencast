package convert

import (
	"reflect"

	"github.com/godbus/dbus/v5"
)

var (
	boolSignature   = dbus.SignatureOfType(reflect.TypeOf(false))
	stringSignature = dbus.SignatureOfType(reflect.TypeOf(""))
	uint32Signature = dbus.SignatureOfType(reflect.TypeOf(uint32(0)))
)

func FromBool(input bool) dbus.Variant {
	return dbus.MakeVariantWithSignature(input, boolSignature)
}

func FromString(input string) dbus.Variant {
	return dbus.MakeVariantWithSignature(input, stringSignature)
}

func FromUint32(input uint32) dbus.Variant {
	return dbus.MakeVariantWithSignature(input, uint32Signature)
}
