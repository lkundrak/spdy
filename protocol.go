// spdy/protocol.go

package spdy

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"http"
	"io"
	"os"
	"strings"
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
	io.WriterTo
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

const MaxDataLength = 1<<24 - 1

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
		err = readBinary(headBuffer, &df.StreamID, &df.Flags)
		if err != nil {
			return
		}
		df.Data, err = readData(r)
		f = df
	} else {
		// Control
		cf := ControlFrame{}
		headBuffer.ReadByte()
		headBuffer.ReadByte() // skip version word
		err = readBinary(headBuffer, &cf.Type, &cf.Flags)
		if err != nil {
			return
		}
		cf.Data, err = readData(r)
		f = cf
	}
	return
}

func readBinary(r io.Reader, args ...interface{}) (err os.Error) {
	for _, a := range args {
		err = binary.Read(r, binary.BigEndian, a)
		if err == nil {
			return
		}
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

const headerDictionary = `optionsgetheadpostputdeletetraceacceptaccept-charsetaccept-encodingaccept-languageauthorizationexpectfromhostif-modified-sinceif-matchif-none-matchif-rangeif-unmodifiedsincemax-forwardsproxy-authorizationrangerefererteuser-agent100101200201202203204205206300301302303304305306307400401402403404405406407408409410411412413414415416417500501502503504505accept-rangesageetaglocationproxy-authenticatepublicretry-afterservervarywarningwww-authenticateallowcontent-basecontent-encodingcache-controlconnectiondatetrailertransfer-encodingupgradeviawarningcontent-languagecontent-lengthcontent-locationcontent-md5content-rangecontent-typeetagexpireslast-modifiedset-cookieMondayTuesdayWednesdayThursdayFridaySaturdaySundayJanFebMarAprMayJunJulAugSepOctNovDecchunkedtext/htmlimage/pngimage/jpgimage/gifapplication/xmlapplication/xhtmltext/plainpublicmax-agecharset=iso-8859-1utf-8gzipdeflateHTTP/1.1statusversionurl`

type HeaderReader struct {
	inflater io.Reader
	buffer   *bytes.Buffer
}

func NewHeaderReader() (hr *HeaderReader) {
	hr = &HeaderReader{buffer: new(bytes.Buffer)}
	hr.inflater = flate.NewReader(hr.buffer)
	// TODO: dictionary
	return
}

func (hr *HeaderReader) Decode(data []byte) (h http.Header, err os.Error) {
	hr.buffer.Write(data)
	h, err = hr.read()
	hr.buffer.Reset()
	return
}

func (hr *HeaderReader) read() (h http.Header, err os.Error) {
	var count uint16
	err = binary.Read(hr.inflater, binary.BigEndian, &count)
	if err != nil {
		return
	}
	h = make(http.Header, int(count))
	for i := 0; i < int(count); i++ {
		var name, value string
		name, err = readHeaderString(hr.inflater)
		if err != nil {
			return
		}
		value, err = readHeaderString(hr.inflater)
		if err != nil {
			return
		}
		valueList := strings.Split(string(value), "\x00", -1)
		h[name] = valueList
	}
	return
}

func readHeaderString(r io.Reader) (s string, err os.Error) {
	var length uint16
	err = binary.Read(r, binary.BigEndian, &length)
	if err != nil {
		return
	}
	data := make([]byte, int(length))
	_, err = io.ReadFull(r, data)
	if err != nil {
		return
	}
	return string(data), nil
}

type HeaderWriter struct {
	deflater *flate.Writer
	buffer   *bytes.Buffer
}

func NewHeaderWriter(level int) (hw *HeaderWriter) {
	hw = &HeaderWriter{buffer: new(bytes.Buffer)}
	hw.deflater = flate.NewWriter(hw.buffer, level)
	// TODO: dictionary
	return
}

func (hw *HeaderWriter) WriteHeader(w io.Writer, h http.Header) (err os.Error) {
	hw.write(h)
	_, err = io.Copy(w, hw.buffer)
	hw.buffer.Reset()
	return
}

func (hw *HeaderWriter) Encode(h http.Header) (data []byte) {
	hw.write(h)
	data = make([]byte, hw.buffer.Len())
	hw.buffer.Read(data)
	return
}

func (hw *HeaderWriter) write(h http.Header) {
	var count uint16
	binary.Write(hw.deflater, binary.BigEndian, count)
	for k, vals := range h {
		binary.Write(hw.deflater, binary.BigEndian, uint16(len(k)))
		binary.Write(hw.deflater, binary.BigEndian, []byte(k))
		v := strings.Join(vals, "\x00")
		binary.Write(hw.deflater, binary.BigEndian, uint16(len(v)))
		binary.Write(hw.deflater, binary.BigEndian, []byte(v))
	}
	// XXX: We may need to do this after we've copied data.
	hw.deflater.Flush()
}
