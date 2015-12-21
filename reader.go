package torrent

import (
	"errors"
	"io"
	"log"
	"os"
	"sync"
)

// Accesses torrent data via a client.
type Reader struct {
	t          *Torrent
	pos        int64
	responsive bool
	readahead  int64

	mu    sync.Mutex
	reads map[*read]struct{}
}

type read struct {
	off int64
	len int
}

var _ io.ReadCloser = &Reader{}

// Don't wait for pieces to complete and be verified. Read calls return as
// soon as they can when the underlying chunks become available.
func (r *Reader) SetResponsive() {
	r.responsive = true
}

// Configure the number of bytes ahead of a read that should also be
// prioritized in preparation for further reads.
func (r *Reader) SetReadahead(readahead int64) {
	r.readahead = readahead
}

func (r *Reader) readable(off int64) (ret bool) {
	log.Println("readable", off)
	defer func() {
		log.Println("readable", ret)
	}()
	if r.t.isClosed() {
		return true
	}
	req, ok := r.t.offsetRequest(off)
	if !ok {
		return true
	}
	if r.responsive {
		return r.t.haveChunk(req)
	}
	return r.t.pieceComplete(int(req.Index))
}

func (r *Reader) waitReadable(off int64) {
	r.t.cl.event.Wait()
}

func (r *Reader) ReadAt(b []byte, off int64) (n int, err error) {
	for {
		var n1 int
		n1, err = r.readAt(b, off)
		n += n1
		b = b[n1:]
		off += int64(n1)
		if len(b) == 0 {
			err = nil
			return
		}
		if err != io.ErrUnexpectedEOF {
			return
		}
	}
}

func (r *Reader) Read(b []byte) (n int, err error) {
	n, err = r.readAt(b, r.pos)
	r.pos += int64(n)
	return
}

// Must only return EOF at the end of the torrent.
func (r *Reader) readAt(b []byte, pos int64) (n int, err error) {
	rd := &read{
		off: pos,
		len: len(b),
	}
	r.reads[rd] = struct{}{}
	defer func() {
		delete(r.reads, rd)
	}()
	for {
		r.t.cl.mu.Lock()
		for !r.readable(pos) {
			r.tickleClient()
			r.waitReadable(pos)
		}
		if r.t.isClosed() {
			r.t.cl.mu.Unlock()
			err = errors.New("torrent closed")
			return
		}
		r.t.cl.mu.Unlock()
		n, err = r.t.torrent.readAt(b, pos)
		log.Println(n, err)
		if n != 0 {
			err = nil
			return
		}
		if err != nil {
			return
		}
	}
	return
}

func (r *Reader) Close() error {
	r.t.torrent.deleteReaderUnlocked(r, r.t.cl)
	r.t = nil
	return nil
}

func (r *Reader) Seek(off int64, whence int) (ret int64, err error) {
	switch whence {
	case os.SEEK_SET:
		r.pos = off
	case os.SEEK_CUR:
		if off == 0 {
			// Avoid creating a race condition.
			ret = r.pos
			return
		}
		r.pos += off
	case os.SEEK_END:
		r.pos = r.t.torrent.Info.TotalLength() + off
	default:
		err = errors.New("bad whence")
	}
	r.tickleClient()
	ret = r.pos
	return
}

func (r *Reader) tickleClient() {
	r.t.torrent.prioritiesChanged(r.t.cl)
}
