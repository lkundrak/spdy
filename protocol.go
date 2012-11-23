// spdy/protocol.go

package spdy

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

type ControlFrameType uint16

// Control frame type constants
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

// Frame flag constants
const (
	FlagFin                              = 0x01
	FlagUnidirectional                   = 0x02
	FlagClearPreviouslyPersistedSettings = 0x01
)

// A Frame is the low-level construct passed over a SPDY connection.
type Frame interface {
	io.WriterTo // WriteTo method writes the packet in SPDY format.
	GetFlags() FrameFlags
	GetData() []byte
}

// ControlFrame holds a generic control frame.
type ControlFrame struct {
	Type  ControlFrameType
	Flags FrameFlags
	Data  []byte
}

func (f ControlFrame) GetFlags() FrameFlags { return f.Flags }
func (f ControlFrame) GetData() []byte      { return f.Data }

func (f ControlFrame) WriteTo(w io.Writer) (n int64, err error) {
	nn, err := writeFrame(w, []interface{}{uint16(0x8002), f.Type, f.Flags}, f.Data)
	return int64(nn), err
}

// MaxDataLength is the maximum number of bytes that can be stored in one frame.
const MaxDataLength = 1<<24 - 1

// A DataFrame stores a data frame and its associated metadata.
type DataFrame struct {
	StreamID uint32
	Flags    FrameFlags
	Data     []byte
}

func (f DataFrame) GetFlags() FrameFlags { return f.Flags }
func (f DataFrame) GetData() []byte      { return f.Data }

func (f DataFrame) WriteTo(w io.Writer) (n int64, err error) {
	nn, err := writeFrame(w, []interface{}{f.StreamID & 0x7fffffff, f.Flags}, f.Data)
	return int64(nn), err
}

const frameHeadSize = 5 // in bytes, excluding length field

// ReadFrame reads an entire frame into memory.
func ReadFrame(r io.Reader) (f Frame, err error) {
	headBuffer := new(bytes.Buffer)
	_, err = io.CopyN(headBuffer, r, frameHeadSize)
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

func readBinary(r io.Reader, args ...interface{}) (err error) {
	for _, a := range args {
		err = binary.Read(r, binary.BigEndian, a)
		if err != nil {
			return
		}
	}
	return
}

func readData(r io.Reader) (data []byte, err error) {
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

func writeFrame(w io.Writer, head []interface{}, data []byte) (n int, err error) {
	var nn int
	// Header (40 bits)
	err = writeBinary(w, head...)
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
	n += nn
	if err != nil {
		return
	}
	// Data
	if length > 0 {
		nn, err = w.Write(data)
		n += nn
	}
	return
}

func writeBinary(r io.Writer, args ...interface{}) (err error) {
	for _, a := range args {
		err = binary.Write(r, binary.BigEndian, a)
		if err != nil {
			return
		}
	}
	return
}

const headerDictionary = `optionsgetheadpostputdeletetraceacceptaccept-charsetaccept-encodingaccept-languageauthorizationexpectfromhostif-modified-sinceif-matchif-none-matchif-rangeif-unmodifiedsincemax-forwardsproxy-authorizationrangerefererteuser-agent100101200201202203204205206300301302303304305306307400401402403404405406407408409410411412413414415416417500501502503504505accept-rangesageetaglocationproxy-authenticatepublicretry-afterservervarywarningwww-authenticateallowcontent-basecontent-encodingcache-controlconnectiondatetrailertransfer-encodingupgradeviawarningcontent-languagecontent-lengthcontent-locationcontent-md5content-rangecontent-typeetagexpireslast-modifiedset-cookieMondayTuesdayWednesdayThursdayFridaySaturdaySundayJanFebMarAprMayJunJulAugSepOctNovDecchunkedtext/htmlimage/pngimage/jpgimage/gifapplication/xmlapplication/xhtmltext/plainpublicmax-agecharset=iso-8859-1utf-8gzipdeflateHTTP/1.1statusversionurl` + "\x00"

type hrSource struct {
	r io.Reader
	m sync.RWMutex
	c *sync.Cond
}

func (src *hrSource) Read(p []byte) (n int, err error) {
	src.m.RLock()
	for src.r == nil {
		src.c.Wait()
	}
	n, err = src.r.Read(p)
	src.m.RUnlock()
	if err == io.EOF {
		src.change(nil)
		err = nil
	}
	return
}

func (src *hrSource) change(r io.Reader) {
	src.m.Lock()
	defer src.m.Unlock()
	src.r = r
	src.c.Broadcast()
}

// A HeaderReader reads zlib-compressed headers from discontiguous sources.
type HeaderReader struct {
	source       hrSource
	decompressor io.ReadCloser
}

// NewHeaderReader creates a HeaderReader with the initial dictionary.
func NewHeaderReader() (hr *HeaderReader) {
	hr = new(HeaderReader)
	hr.source.c = sync.NewCond(hr.source.m.RLocker())
	return
}

// ReadHeader reads a set of headers from a reader.
func (hr *HeaderReader) ReadHeader(r io.Reader) (h http.Header, err error) {
	hr.source.change(r)
	h, err = hr.read()
	return
}

// Decode reads a set of headers from a block of bytes.
func (hr *HeaderReader) Decode(data []byte) (h http.Header, err error) {
	hr.source.change(bytes.NewBuffer(data))
	h, err = hr.read()
	return
}

func (hr *HeaderReader) read() (h http.Header, err error) {
	var count uint16
	if hr.decompressor == nil {
		hr.decompressor, _ = zlib.NewReaderDict(&hr.source, []byte(headerDictionary))
		if err != nil {
			return
		}
	}
	err = binary.Read(hr.decompressor, binary.BigEndian, &count)
	if err != nil {
		return
	}
	h = make(http.Header, int(count))
	for i := 0; i < int(count); i++ {
		var name, value string
		name, err = readHeaderString(hr.decompressor)
		if err != nil {
			return
		}
		value, err = readHeaderString(hr.decompressor)
		if err != nil {
			return
		}
		valueList := strings.Split(string(value), "\x00")
		for _, v := range valueList {
			h.Add(name, v)
		}
	}
	return
}

func readHeaderString(r io.Reader) (s string, err error) {
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

// HeaderWriter will write zlib-compressed headers on different streams.
type HeaderWriter struct {
	compressor *zlib.Writer
	buffer     *bytes.Buffer
}

// NewHeaderWriter creates a HeaderWriter ready to compress headers.
func NewHeaderWriter(level int) (hw *HeaderWriter) {
	hw = &HeaderWriter{buffer: new(bytes.Buffer)}
	hw.compressor, _ = zlib.NewWriterLevelDict(hw.buffer, level, []byte(headerDictionary))
	return
}

// WriteHeader writes a header block directly to an output.
func (hw *HeaderWriter) WriteHeader(w io.Writer, h http.Header) (err error) {
	hw.write(h)
	_, err = io.Copy(w, hw.buffer)
	hw.buffer.Reset()
	return
}

// Encode returns a compressed header block.
func (hw *HeaderWriter) Encode(h http.Header) (data []byte) {
	hw.write(h)
	data = make([]byte, hw.buffer.Len())
	hw.buffer.Read(data)
	return
}

func (hw *HeaderWriter) write(h http.Header) {
	binary.Write(hw.compressor, binary.BigEndian, uint16(len(h)))
	for k, vals := range h {
		k = strings.ToLower(k)
		binary.Write(hw.compressor, binary.BigEndian, uint16(len(k)))
		binary.Write(hw.compressor, binary.BigEndian, []byte(k))
		v := strings.Join(vals, "\x00")
		binary.Write(hw.compressor, binary.BigEndian, uint16(len(v)))
		binary.Write(hw.compressor, binary.BigEndian, []byte(v))
	}
	hw.compressor.Flush()
}
