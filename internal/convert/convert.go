package convert

import (
	"reflect"

	"github.com/godbus/dbus/v5"
)

var (
	boolSignature   = dbus.SignatureOfType(reflect.TypeFor[bool]())
	stringSignature = dbus.SignatureOfType(reflect.TypeFor[string]())
	uint32Signature = dbus.SignatureOfType(reflect.TypeFor[uint32]())
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
