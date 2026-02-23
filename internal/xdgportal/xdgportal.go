package xdgportal

import (
	"fmt"
	"io"
	"os"

	"github.com/godbus/dbus/v5"

	"go2tv.app/screencast/internal/apis"
	"go2tv.app/screencast/internal/convert"
	"go2tv.app/screencast/internal/request"
	"go2tv.app/screencast/internal/session"
)

const (
	interfaceName      = apis.CallBaseName + ".ScreenCast"
	createSessionName  = interfaceName + ".CreateSession"
	selectSourcesName  = interfaceName + ".SelectSources"
	startName          = interfaceName + ".Start"
	openPipeWireRemote = interfaceName + ".OpenPipeWireRemote"
)

const (
	SourceTypeMonitor uint32 = 1
	SourceTypeWindow  uint32 = 2
	SourceTypeVirtual uint32 = 4
)

const (
	CursorModeHidden   uint32 = 1
	CursorModeEmbedded uint32 = 2
	CursorModeMetadata uint32 = 4
)

const (
	PersistModeNone       uint32 = 0
	PersistModeRunning    uint32 = 1
	PersistModePersistent uint32 = 2
)

func getUint32Property(property string) (uint32, error) {
	value, err := apis.GetProperty(interfaceName, property)
	if err != nil {
		return 0, err
	}

	result, ok := value.(uint32)
	if !ok {
		return 0, fmt.Errorf("property %s returned unexpected type %T", property, value)
	}
	return result, nil
}

func GetAvailableSourceTypes() (uint32, error) {
	return getUint32Property("AvailableSourceTypes")
}

func GetAvailableCursorModes() (uint32, error) {
	return getUint32Property("AvailableCursorModes")
}

func GetVersion() (uint32, error) {
	return getUint32Property("version")
}

type Stream struct {
	NodeID     uint32
	Position   [2]int32
	Size       [2]int32
	SourceType uint32
	MappingID  string
	ID         string
}

type Session struct {
	Path         dbus.ObjectPath
	sessionToken string
}

type Options struct {
	HandleToken        string
	SessionHandleToken string
}

type SelectSourcesOptions struct {
	HandleToken  string
	Types        uint32
	Multiple     bool
	CursorMode   uint32
	RestoreToken string
	PersistMode  uint32
}

type StartOptions struct {
	HandleToken string
}

type OpenPipeWireRemoteOptions struct {
}

func CreateSession(options *Options) (*Session, error) {
	data := map[string]dbus.Variant{
		"session_handle_token": session.GenerateToken(),
	}
	if options != nil {
		if options.HandleToken != "" {
			data["handle_token"] = convert.FromString(options.HandleToken)
		}
		if options.SessionHandleToken != "" {
			data["session_handle_token"] = convert.FromString(options.SessionHandleToken)
		}
	}

	result, err := apis.Call(createSessionName, data)
	if err != nil {
		return nil, err
	}

	requestPath, ok := result.(dbus.ObjectPath)
	if !ok {
		return nil, fmt.Errorf("CreateSession returned unexpected type %T", result)
	}

	status, results, err := request.OnSignalResponse(requestPath)
	if err != nil {
		return nil, err
	} else if status >= request.Cancelled {
		return nil, nil
	}

	sessionHandle, ok := results["session_handle"]
	if !ok {
		return nil, fmt.Errorf("CreateSession response missing session_handle")
	}
	sessionPath, ok := sessionHandle.Value().(string)
	if !ok {
		return nil, fmt.Errorf("CreateSession session_handle has unexpected type %T", sessionHandle.Value())
	}
	token := ""
	if options != nil {
		token = options.HandleToken
	}
	return &Session{Path: dbus.ObjectPath(sessionPath), sessionToken: token}, nil
}

func (s *Session) SelectSources(options *SelectSourcesOptions) error {
	data := map[string]dbus.Variant{}
	if options != nil && options.HandleToken == "" && s.sessionToken != "" {
		data["handle_token"] = convert.FromString(s.sessionToken)
	}
	if options != nil {
		if options.HandleToken != "" {
			data["handle_token"] = convert.FromString(options.HandleToken)
		}
		if options.Types != 0 {
			data["types"] = convert.FromUint32(options.Types)
		}
		if options.Multiple {
			data["multiple"] = convert.FromBool(options.Multiple)
		}
		if options.CursorMode != 0 {
			data["cursor_mode"] = convert.FromUint32(options.CursorMode)
		}
		if options.RestoreToken != "" {
			data["restore_token"] = convert.FromString(options.RestoreToken)
		}
		if options.PersistMode != 0 {
			data["persist_mode"] = convert.FromUint32(options.PersistMode)
		}
	}

	result, err := apis.Call(selectSourcesName, s.Path, data)
	if err != nil {
		return err
	}

	requestPath, ok := result.(dbus.ObjectPath)
	if !ok {
		return fmt.Errorf("SelectSources returned unexpected type %T", result)
	}

	status, _, err := request.OnSignalResponse(requestPath)
	if err != nil {
		return err
	} else if status >= request.Cancelled {
		return nil
	}

	return nil
}

func (s *Session) Start(parentWindow string, options *StartOptions) ([]Stream, error) {
	data := map[string]dbus.Variant{}
	if options != nil && options.HandleToken == "" && s.sessionToken != "" {
		data["handle_token"] = convert.FromString(s.sessionToken)
	}
	if options != nil && options.HandleToken != "" {
		data["handle_token"] = convert.FromString(options.HandleToken)
	}

	result, err := apis.Call(startName, s.Path, parentWindow, data)
	if err != nil {
		return nil, err
	}

	requestPath, ok := result.(dbus.ObjectPath)
	if !ok {
		return nil, fmt.Errorf("Start returned unexpected type %T", result)
	}

	status, results, err := request.OnSignalResponse(requestPath)
	if err != nil {
		return nil, err
	} else if status >= request.Cancelled {
		return nil, nil
	}

	streams := []Stream{}

	streamVariant, ok := results["streams"]
	if !ok {
		return nil, nil
	}

	var rawStreams [][]any
	if rs, ok := streamVariant.Value().([][]any); ok {
		rawStreams = rs
	} else if rs, ok := streamVariant.Value().([]any); ok {
		rawStreams = make([][]any, len(rs))
		for i, r := range rs {
			if s, ok := r.([]any); ok {
				rawStreams[i] = s
			}
		}
	} else {
		return nil, nil
	}

	for _, streamSlice := range rawStreams {
		if len(streamSlice) < 2 {
			continue
		}

		stream := Stream{}

		nodeID, ok := streamSlice[0].(uint32)
		if ok {
			stream.NodeID = nodeID
		}

		props, ok := streamSlice[1].(map[string]dbus.Variant)
		if ok {
			if pos, ok := props["position"]; ok {
				if position, ok := parseInt32Pair(pos.Value()); ok {
					stream.Position = position
				}
			}
			if size, ok := props["size"]; ok {
				if parsedSize, ok := parseInt32Pair(size.Value()); ok {
					stream.Size = parsedSize
				}
			}
			if sourceType, ok := props["source_type"]; ok {
				if parsedType, ok := sourceType.Value().(uint32); ok {
					stream.SourceType = parsedType
				}
			}
			if mappingID, ok := props["mapping_id"]; ok {
				if parsedID, ok := mappingID.Value().(string); ok {
					stream.MappingID = parsedID
				}
			}
			if id, ok := props["id"]; ok {
				if parsedID, ok := id.Value().(string); ok {
					stream.ID = parsedID
				}
			}
		}

		streams = append(streams, stream)
	}

	return streams, nil
}

func (s *Session) OpenPipeWireRemote(options *OpenPipeWireRemoteOptions) (int, error) {
	_ = options
	data := map[string]dbus.Variant{}

	conn, err := dbus.SessionBus()
	if err != nil {
		return -1, err
	}

	obj := conn.Object(apis.ObjectName, apis.ObjectPath)
	call := obj.Call(openPipeWireRemote, 0, s.Path, data)
	if call.Err != nil {
		return -1, call.Err
	}

	var fd int
	err = call.Store(&fd)
	return fd, err
}

func parseInt32Pair(value any) ([2]int32, bool) {
	values, ok := value.([]any)
	if !ok || len(values) < 2 {
		return [2]int32{}, false
	}

	left, ok := values[0].(int32)
	if !ok {
		return [2]int32{}, false
	}
	right, ok := values[1].(int32)
	if !ok {
		return [2]int32{}, false
	}

	return [2]int32{left, right}, true
}

func (s *Session) Close() error {
	return session.Close(s.Path)
}

func (s *Session) OpenPipeWireRemoteReader() (io.Reader, error) {
	fd, err := s.OpenPipeWireRemote(nil)
	if err != nil {
		return nil, err
	}

	file := os.NewFile(uintptr(fd), "pipewire")
	return file, nil
}
