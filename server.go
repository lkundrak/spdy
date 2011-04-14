// spdy/server.go

package spdy

import (
	"bytes"
	"encoding/binary"
	"http"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
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
	c       net.Conn
	handler http.Handler
	in, out chan Frame
	streams map[uint32]*serverStream // all access is done synchronously

	headerReader *HeaderReader
	headerWriter *HeaderWriter
}

func newSession(c net.Conn, h http.Handler) (s *session, err os.Error) {
	s = &session{
		c:            c,
		handler:      h,
		headerReader: NewHeaderReader(),
		headerWriter: NewHeaderWriter(-1),
		in:           make(chan Frame),
		out:          make(chan Frame),
		streams:      make(map[uint32]*serverStream),
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
	log.Printf("CONTROL <-\n")
	log.Printf("  Type:  %v\n", frame.Type)
	log.Printf("  Flags: %#04x\n", frame.Flags)

	switch frame.Type {
	case TypeSynStream:
		if stream, err := newServerStream(sess, frame); err == nil {
			if _, exists := sess.streams[stream.id]; !exists {
				sess.streams[stream.id] = stream
				go stream.bufferReads()
				go func() {
					sess.handler.ServeHTTP(stream, stream.Request())
					stream.finish()
				}()
			}
		} else {
			log.Println("Stream error:", err)
		}
	case TypeRstStream:
		d := bytes.NewBuffer(frame.Data)
		var streamId, statusCode uint32
		readBinary(d, &streamId, &statusCode)
		log.Println("  Reset")
		log.Println("    ID:", streamId)
		log.Println("    Code:", statusCode)
	}
}

func (sess *session) handleData(frame DataFrame) {
	log.Printf("DATA <-\n")
	log.Printf("  Flags: %#04x\n", frame.Flags)

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
		switch f := frame.(type) {
		case DataFrame:
			log.Println("DATA ->")
			log.Printf("  Flags: %#04x", f.Flags)
		case ControlFrame:
			log.Println("CONTROL ->")
			log.Printf("  Type:  %v", f.Type)
			log.Printf("  Flags: %#04x\n", f.Flags)
		}
		_, err := frame.WriteTo(sess.c)
		if err != nil {
			log.Println("Error", err)
		}
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

	requestHeaders  http.Header
	responseHeaders http.Header
	wroteHeader     bool

	readChan   chan DataFrame
	readBuffer *bytes.Buffer
	readLock   sync.Mutex
}

func newServerStream(sess *session, frame ControlFrame) (st *serverStream, err os.Error) {
	if frame.Type != TypeSynStream {
		err = os.NewError("Server stream must be created from a SynStream frame")
		return
	}
	st = &serverStream{
		session:         sess,
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
	st.requestHeaders, err = sess.headerReader.Decode(data.Bytes())
	if err == nil {
		log.Println("HEADERS")
		for name, values := range st.requestHeaders {
			log.Printf("  %s:\n", name)
			for _, v := range values {
				log.Println("    " + v)
			}
		}
	}
	return
}

func (st *serverStream) bufferReads() {
	if st.readChan == nil {
		return
	}
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
	req.URL, _ = http.ParseRequestURL(req.RawURL)
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
		if len(p) < MaxDataLength {
			frame.Data = make([]byte, len(p))
		} else {
			frame.Data = make([]byte, MaxDataLength)
		}
		copy(frame.Data, p)
		p = p[len(frame.Data):]
		st.session.out <- frame
		n += len(frame.Data)
	}
	return
}

type synReplyFrame struct {
	stream *serverStream
	header http.Header
	flags  FrameFlags
}

func (frame synReplyFrame) GetFlags() FrameFlags {
	return frame.flags
}

func (frame synReplyFrame) GetData() []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, frame.stream.id&0x7fffffff)
	buf.Write([2]byte{}[:])
	frame.stream.session.headerWriter.WriteHeader(buf, frame.stream.responseHeaders)
	return buf.Bytes()
}

func (frame synReplyFrame) WriteTo(w io.Writer) (n int64, err os.Error) {
	cf := ControlFrame{Type: TypeSynReply, Data: frame.GetData()}
	return cf.WriteTo(w)
}

func (st *serverStream) WriteHeader(code int) {
	if st.wroteHeader {
		return
	}
	st.responseHeaders.Set("status", strconv.Itoa(code)+" "+http.StatusText(code))
	st.responseHeaders.Set("version", "HTTP/1.1")
	if st.responseHeaders.Get("Content-Type") == "" {
		st.responseHeaders.Set("Content-Type", "text/html; charset=utf-8")
	}
	if st.responseHeaders.Get("Date") == "" {
		st.responseHeaders.Set("Date", time.UTC().Format(http.TimeFormat))
	}
	// Write the frame
	// TODO: Copy headers
	st.session.out <- synReplyFrame{stream: st, header: st.responseHeaders}
	st.wroteHeader = true
	// Display response headers
	log.Println("RESPONSE HEADERS")
	for name, values := range st.responseHeaders {
		log.Printf("  %s:\n", name)
		for _, v := range values {
			log.Println("    " + v)
		}
	}
}

func (st *serverStream) Close() (err os.Error) {
	if st.closed {
		return
	}
	st.session.out <- DataFrame{
		StreamID: st.id,
		Flags:    FlagFin,
		Data:     []byte{},
	}
	st.closed = true
	return nil
}

func (st *serverStream) finish() (err os.Error) {
	if !st.wroteHeader {
		st.WriteHeader(http.StatusOK)
	}
	return st.Close()
}
