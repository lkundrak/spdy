// spdy/protocol.go

package spdy

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const (
	TypeSynStream    = 0x0001
	TypeSynReply     = 0x0002
	TypeRstStream    = 0x0003
	TypeSettings     = 0x0004
	TypeNoop         = 0x0005
	TypePing         = 0x0006
	TypeGoaway       = 0x0007
	TypeHeaders      = 0x0008
	TypeWindowUpdate = 0x0009
)

type ControlFrameType uint16

func (t ControlFrameType) String() string {
	switch t {
	case TypeSynStream:
		return "SYN_STREAM"
	case TypeSynReply:
		return "SYN_REPLY"
	case TypeRstStream:
		return "RST_STREAM"
	case TypeSettings:
		return "SETTINGS"
	case TypeNoop:
		return "NOOP"
	case TypePing:
		return "PING"
	case TypeGoaway:
		return "GOAWAY"
	case TypeHeaders:
		return "HEADERS"
	case TypeWindowUpdate:
		return "WINDOW_UPDATE"
	}
	return fmt.Sprintf("Type(%#04x)", uint16(t))
}

type FrameFlags uint8

const (
	FlagFin                              = 0x01
	FlagUnidirectional                   = 0x02
	FlagClearPreviouslyPersistedSettings = 0x01
)

type Frame interface {
	GetFlags() FrameFlags
	GetData() []byte
}

type ControlFrame struct {
	Type  ControlFrameType
	Flags FrameFlags
	Data  []byte
}

func (f ControlFrame) GetFlags() FrameFlags { return f.Flags }
func (f ControlFrame) GetData() []byte      { return f.Data }

func (f ControlFrame) WriteTo(w io.Writer) (n int64, err os.Error) {
	nn, err := writeFrame(w, []interface{}{0x8001, f.Type, f.Flags}, f.Data)
	return int64(nn), err
}

type DataFrame struct {
	StreamID uint32
	Flags    FrameFlags
	Data     []byte
}

func (f DataFrame) GetFlags() FrameFlags { return f.Flags }
func (f DataFrame) GetData() []byte      { return f.Data }

func (f DataFrame) WriteTo(w io.Writer) (n int64, err os.Error) {
	nn, err := writeFrame(w, []interface{}{f.StreamID & 0x7fffffff, f.Flags}, f.Data)
	return int64(nn), err
}

const frameHeadSize = 5 // in bytes, excluding length field

// ReadFrame reads an entire frame into memory.
func ReadFrame(r io.Reader) (f Frame, err os.Error) {
	headBuffer := new(bytes.Buffer)
	_, err = io.Copyn(headBuffer, r, frameHeadSize)
	if err != nil {
		return
	}
	if headBuffer.Bytes()[0]&0x80 == 0 {
		// Data
		df := DataFrame{}
		err = binary.Read(headBuffer, binary.BigEndian, []interface{}{&df.StreamID, &df.Flags})
		if err != nil {
			return
		}
		df.Data, err = readData(r)
		f = df
	} else {
		// Control
		cf := ControlFrame{}
		headBuffer.ReadByte() // skip version byte
		err = binary.Read(headBuffer, binary.BigEndian, []interface{}{&cf.Type, &cf.Flags})
		if err != nil {
			return
		}
		cf.Data, err = readData(r)
		f = cf
	}
	return
}

func readData(r io.Reader) (data []byte, err os.Error) {
	lengthField := make([]byte, 3)
	_, err = io.ReadFull(r, lengthField)
	if err != nil {
		return
	}
	var length uint32
	length |= uint32(lengthField[0]) << 16
	length |= uint32(lengthField[1]) << 8
	length |= uint32(lengthField[2])

	if length > 0 {
		data = make([]byte, int(length))
		_, err = io.ReadFull(r, data)
	} else {
		data = []byte{}
	}
	return
}

func writeFrame(w io.Writer, head []interface{}, data []byte) (n int, err os.Error) {
	var nn int
	// Header (40 bits)
	err = binary.Write(w, binary.BigEndian, head)
	if err != nil {
		return
	}
	n += frameHeadSize
	// Length (24 bits)
	length := len(data)
	nn, err = w.Write([]byte{
		byte(length & 0x00ff0000 >> 16),
		byte(length & 0x0000ff00 >> 8),
		byte(length & 0x000000ff),
	})
	if err != nil {
		return
	}
	n += nn
	// Data
	if length > 0 {
		nn, err = w.Write(data)
		if err == nil {
			n += nn
		}
	}
	return
}
