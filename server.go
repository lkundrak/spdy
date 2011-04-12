// spdy/server.go

package spdy

import (
	"bytes"
	"encoding/binary"
	"http"
	"net"
	"os"
	"strconv"
	"sync"
)

type Server struct {
	Addr    string
	Handler http.Handler
}

func (srv *Server) Serve(l net.Listener) os.Error {
	defer l.Close()
	handler := srv.Handler
	if handler == nil {
		handler = http.DefaultServeMux
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		s, err := newSession(c, handler)
		if err != nil {
			return err
		}
		go s.serve()
	}
	return nil
}

type session struct {
	c       net.Conn
	handler http.Handler
	in, out chan Frame
	streams map[uint32]*stream
}

func newSession(c net.Conn, h http.Handler) (s *session, err os.Error) {
	s = &session{
		c:       c,
		handler: h,
		in:      make(chan Frame),
		out:     make(chan Frame),
		streams: make(map[uint32]*stream),
	}
	return
}

func (sess *session) serve() {
	defer sess.c.Close()
	go sess.sendFrames()
	go sess.receiveFrames()

	for {
		select {
		case f := <-sess.in:
			switch frame := f.(type) {
			case ControlFrame:
				sess.handleControl(frame)
			case DataFrame:
				sess.handleData(frame)
			}
		}
	}
}

func (sess *session) handleControl(frame ControlFrame) {
	// TODO
}

func (sess *session) handleData(frame DataFrame) {
	st, found := sess.streams[frame.StreamID]
	if !found {
		// TODO: Error?
		return
	}
	if st.readChan != nil {
		st.readChan <- frame
		if frame.Flags&FlagFin != 0 {
			close(st.readChan)
		}
	}
}

func (sess *session) sendFrames() {
	for frame := range sess.out {
		// TODO: Check for errors
		frame.WriteTo(sess.c)
	}
}

func (sess *session) receiveFrames() {
	defer close(sess.in)
	for {
		frame, err := ReadFrame(sess.c)
		if err != nil {
			return
		}
		sess.in <- frame
	}
}

type stream struct {
	id      uint32
	session *session
	closed  bool

	requestHeaders  http.Header
	responseHeaders http.Header
	headerWriter    *HeaderWriter
	wroteHeader     bool

	readChan   chan DataFrame
	readBuffer *bytes.Buffer
	readLock   sync.Mutex

	lastWrite Frame
}

func (st *stream) bufferReads() {
	for frame := range st.readChan {
		st.readLock.Lock()
		st.readBuffer.Write(frame.Data)
		st.readLock.Unlock()
	}
	st.readChan = nil
}

func (st *stream) Read(p []byte) (n int, err os.Error) {
	st.readLock.Lock()
	defer st.readLock.Unlock()
	if st.readBuffer.Len() == 0 {
		if st.readChan == nil {
			return 0, os.EOF
		}
		// TODO: Block for more data
	}
	return st.readBuffer.Read(p)
}

// Header returns the current response headers.
func (st *stream) Header() http.Header { return st.responseHeaders }

func (st *stream) Write(p []byte) (n int, err os.Error) {
	if st.closed {
		err = os.NewError("Write on closed stream")
		return
	}
	if !st.wroteHeader {
		st.WriteHeader(http.StatusOK)
	}
	for len(p) > 0 {
		frame := DataFrame{
			StreamID: st.id,
		}
		if len(p) <= MaxDataLength {
			frame.Data = p
			p = p[:0]
		} else {
			frame.Data = p[:MaxDataLength]
			p = p[MaxDataLength:]
		}
		st.writeFrame(frame)
	}
	return
}

func (st *stream) WriteHeader(code int) {
	if st.wroteHeader {
		return
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, st.id&0x7fffffff)
	buf.Write([16]byte{}[:])
	st.responseHeaders.Set("status", strconv.Itoa(code)+" "+http.StatusText(code))
	st.responseHeaders.Set("version", "HTTP/1.1")
	st.headerWriter.WriteHeader(buf, st.responseHeaders)
	st.writeFrame(ControlFrame{
		Type: TypeSynReply,
		Data: buf.Bytes(),
	})
}

func (st *stream) writeFrame(frame Frame) {
	if st.lastWrite != nil {
		st.session.out <- st.lastWrite
	}
	st.lastWrite = frame
}

func (st *stream) Flush() os.Error {
	if st.lastWrite != nil {
		st.session.out <- st.lastWrite
		st.lastWrite = nil
	}
	return nil
}

func (st *stream) Close() (err os.Error) {
	if st.lastWrite != nil {
		switch frame := st.lastWrite.(type) {
		case DataFrame:
			frame.Flags |= FlagFin
			st.session.out <- frame
		case ControlFrame:
			if frame.Type == TypeSynStream || frame.Type == TypeSynReply {
				frame.Flags |= FlagFin
				st.session.out <- frame
			} else {
				st.session.out <- st.lastWrite
				st.session.out <- DataFrame{
					StreamID: st.id,
					Flags:    FlagFin,
					Data:     []byte{},
				}
			}
		default:
			// Any other frame must be sent first, then followed by a close.
			st.session.out <- st.lastWrite
			st.session.out <- DataFrame{
				StreamID: st.id,
				Flags:    FlagFin,
				Data:     []byte{},
			}
		}
		st.lastWrite = nil
	} else {
		// Already flushed
		st.session.out <- DataFrame{
			StreamID: st.id,
			Flags:    FlagFin,
			Data:     []byte{},
		}
	}
	st.closed = true
	return nil
}
