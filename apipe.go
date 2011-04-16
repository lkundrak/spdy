// spdy/apipe.go

package spdy

import (
	"bytes"
	"os"
	"sync"
)

// An asyncPipe is similar to *io.Pipe, but writes never block: they are sent to a buffer, where a reader will block until it has some data in the buffer.
// The write side can close with an error, but the read side cannot.
type asyncPipe struct {
	rl      sync.Mutex   // gates readers one at a time
	wl      sync.Mutex   // gates writers one at a time
	l       sync.Mutex   // protects remaining fields
	data    bytes.Buffer // data remaining
	rwait   sync.Cond    // waiting reader
	rclosed bool         // if reader closed, break pipe
	werr    os.Error     // if writer closed, error to give reads
}

func (p *asyncPipe) read(b []byte) (n int, err os.Error) {
	// One reader at a time.
	p.rl.Lock()
	defer p.rl.Unlock()

	p.l.Lock()
	defer p.l.Unlock()
	for {
		if p.rclosed {
			return 0, os.EINVAL
		}
		if p.data.Len() > 0 {
			break
		}
		if p.werr != nil {
			return 0, p.werr
		}
		p.rwait.Wait()
	}
	return p.data.Read(b)
}

func (p *asyncPipe) write(b []byte) (n int, err os.Error) {
	// One writer at a time.
	p.wl.Lock()
	defer p.wl.Unlock()

	p.l.Lock()
	defer p.l.Unlock()
	p.data.Write(b)
	p.rwait.Signal()
	if p.rclosed {
		err = os.EPIPE
	}
	if p.werr != nil {
		err = os.EINVAL
	}
	n = len(b)
	return
}

func (p *asyncPipe) rclose() {
	p.l.Lock()
	defer p.l.Unlock()
	p.rclosed = true
	p.rwait.Signal()
}

func (p *asyncPipe) wclose(err os.Error) {
	if err == nil {
		err = os.EOF
	}
	p.l.Lock()
	defer p.l.Unlock()
	p.werr = err
	p.rwait.Signal()
}

func apipe() *asyncPipe {
	p := new(asyncPipe)
	p.rwait.L = &p.l
	return p
}
