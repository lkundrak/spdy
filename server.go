// spdy/server.go

package spdy

import (
	"bytes"
	"encoding/binary"
	"http"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
)

// ListenAndServe creates a new Server that serves on the given address.  If
// the handler is nil, then http.DefaultServeMux is used.
func ListenAndServe(addr string, handler http.Handler) os.Error {
	srv := &Server{addr, handler}
	return srv.ListenAndServe()
}

// A Server handles incoming SPDY connections with HTTP handlers.
type Server struct {
	Addr    string
	Handler http.Handler
}

func (srv *Server) ListenAndServe() os.Error {
	addr := srv.Addr
	if addr == "" {
		addr = ":http"
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return srv.Serve(l)
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

// A session manages a single TCP connection to a client.
type session struct {
	c           net.Conn
	handler     http.Handler
	in, out     chan Frame
	streams     map[uint32]*serverStream
	streamsLock sync.RWMutex
}

func newSession(c net.Conn, h http.Handler) (s *session, err os.Error) {
	s = &session{
		c:       c,
		handler: h,
		in:      make(chan Frame),
		out:     make(chan Frame),
		streams: make(map[uint32]*serverStream),
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
	switch frame.Type {
	case TypeSynStream:
		sess.streamsLock.Lock()
		defer sess.streamsLock.Unlock()
		if stream, err := newServerStream(sess, frame); err == nil {
			if _, exists := sess.streams[stream.id]; !exists {
				sess.streams[stream.id] = stream
				go stream.bufferReads()
				go sess.handler.ServeHTTP(stream, stream.Request())
			}
		}
	}
}

func (sess *session) handleData(frame DataFrame) {
	sess.streamsLock.RLock()
	defer sess.streamsLock.RUnlock()

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

// A serverStream is a logical data stream inside a session.  A serverStream
// services a single request.
type serverStream struct {
	id      uint32
	session *session
	closed  bool

	headerReader   *HeaderReader
	requestHeaders http.Header

	headerWriter    *HeaderWriter
	responseHeaders http.Header
	wroteHeader     bool

	readChan   chan DataFrame
	readBuffer *bytes.Buffer
	readLock   sync.Mutex

	lastWrite Frame
}

func newServerStream(sess *session, frame ControlFrame) (st *serverStream, err os.Error) {
	if frame.Type != TypeSynStream {
		err = os.NewError("Server stream must be created from a SynStream frame")
		return
	}
	st = &serverStream{
		session:         sess,
		headerReader:    NewHeaderReader(),
		headerWriter:    NewHeaderWriter(-1),
		responseHeaders: make(http.Header),
	}
	if frame.Flags&FlagFin == 0 {
		// Request body will follow
		st.readChan = make(chan DataFrame)
		st.readBuffer = new(bytes.Buffer)
	}
	// Read frame data
	data := bytes.NewBuffer(frame.Data)
	err = binary.Read(data, binary.BigEndian, &st.id)
	if err != nil {
		return
	}
	_, err = io.ReadFull(data, make([]byte, 6)) // skip associated stream ID and priority
	if err != nil {
		return
	}
	st.requestHeaders, err = st.headerReader.Decode(data.Bytes())
	return
}

func (st *serverStream) bufferReads() {
	for frame := range st.readChan {
		st.readLock.Lock()
		st.readBuffer.Write(frame.Data)
		st.readLock.Unlock()
	}
	st.readChan = nil
}

// Request returns the request data associated with the serverStream.
func (st *serverStream) Request() (req *http.Request) {
	// TODO
	req = &http.Request{
		Method:     st.requestHeaders.Get("method"),
		RawURL:     st.requestHeaders.Get("url"),
		Proto:      st.requestHeaders.Get("version"),
		Header:     st.requestHeaders,
		Body:       st,
		RemoteAddr: st.session.c.RemoteAddr().String(),
	}
	return
}

func (st *serverStream) Read(p []byte) (n int, err os.Error) {
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
func (st *serverStream) Header() http.Header { return st.responseHeaders }

func (st *serverStream) Write(p []byte) (n int, err os.Error) {
	if st.closed {
		err = os.NewError("Write on closed serverStream")
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

func (st *serverStream) WriteHeader(code int) {
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

func (st *serverStream) writeFrame(frame Frame) {
	if st.lastWrite != nil {
		st.session.out <- st.lastWrite
	}
	st.lastWrite = frame
}

func (st *serverStream) Flush() os.Error {
	if st.lastWrite != nil {
		st.session.out <- st.lastWrite
		st.lastWrite = nil
	}
	return nil
}

func (st *serverStream) Close() (err os.Error) {
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
