package session

import (
	"crypto/rand"
	"math/big"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
	"go2tv.app/screencast/internal/apis"
	"go2tv.app/screencast/internal/convert"
)

const (
	interfaceName = "org.freedesktop.portal.Session"
	closedMember  = "Closed"
	closeCallName = interfaceName + ".Close"
)

func Close(path dbus.ObjectPath) error {
	return apis.CallOnObject(path, closeCallName)
}

func GenerateToken() dbus.Variant {
	str := strings.Builder{}
	str.WriteString("screencast")
	a, _ := rand.Int(rand.Reader, big.NewInt(1<<16))
	str.WriteString(strconv.FormatUint(a.Uint64(), 16))
	return convert.FromString(str.String())
}
