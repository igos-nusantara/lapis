package binder

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Handler serves one incoming transaction. For two-way calls it fills
// reply (usually starting with WriteInt32(0) for "no exception").
type Handler func(code uint32, r *Reader, reply *Parcel) error

// Local is a binder object implemented in this process.
type Local struct {
	Interface string
	handler   Handler
	cookie    uintptr
}

// Conn is an open binder device with one looper thread for incoming
// calls and one client thread for outgoing calls. Binder ties replies to
// kernel threads, so both run on locked OS threads.
type Conn struct {
	fd  int
	mem []byte

	mu         sync.Mutex
	locals     map[uintptr]*Local
	nextCookie uintptr
	deaths     map[uintptr]func()

	calls chan func()
}

// binder_transaction_data, 64-bit layout.
type transactionData struct {
	target      uint64 // handle or ptr
	cookie      uint64
	code        uint32
	flags       uint32
	senderPID   int32
	senderEUID  uint32
	dataSize    uint64
	offsetsSize uint64
	dataPtr     uint64
	offsetsPtr  uint64
}

type writeRead struct {
	writeSize     uint64
	writeConsumed uint64
	writeBuffer   uint64
	readSize      uint64
	readConsumed  uint64
	readBuffer    uint64
}

// Open opens a binder device (e.g. /dev/anbox-binder).
func Open(path string) (*Conn, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var version int32
	if err := ioctl(fd, ioctlVersion, unsafe.Pointer(&version)); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("binder version: %w", err)
	}
	if version != protocolVersion {
		unix.Close(fd)
		return nil, fmt.Errorf("binder protocol %d, want %d", version, protocolVersion)
	}
	mem, err := unix.Mmap(fd, 0, mmapSize, unix.PROT_READ, unix.MAP_PRIVATE|unix.MAP_NORESERVE)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("binder mmap: %w", err)
	}
	var maxThreads uint32
	_ = ioctl(fd, ioctlSetMaxThreads, unsafe.Pointer(&maxThreads))

	c := &Conn{
		fd:         fd,
		mem:        mem,
		locals:     map[uintptr]*Local{},
		nextCookie: 1,
		deaths:     map[uintptr]func(){},
		calls:      make(chan func()),
	}
	go c.clientThread()
	go c.looperThread()
	return c, nil
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}

// NewLocal registers a local object that can be handed to other processes.
func (c *Conn) NewLocal(iface string, h Handler) *Local {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Cookies double as the object's "pointer" on the wire; they only
	// need to be unique and non-zero within this process.
	l := &Local{Interface: iface, handler: h, cookie: c.nextCookie << 4}
	c.nextCookie++
	c.locals[l.cookie] = l
	return l
}

// onClient runs fn on the client thread and waits for it.
func (c *Conn) onClient(fn func()) {
	done := make(chan struct{})
	c.calls <- func() { fn(); close(done) }
	<-done
}

func (c *Conn) clientThread() {
	runtime.LockOSThread()
	for fn := range c.calls {
		fn()
	}
}

func (c *Conn) looperThread() {
	runtime.LockOSThread()
	cmd := le.AppendUint32(nil, bcEnterLooper)
	if _, err := c.writeRead(cmd, nil); err != nil {
		slog.Error("binder looper", "err", err)
		return
	}
	buf := make([]byte, 4096)
	for {
		n, err := c.writeRead(nil, buf)
		if err != nil {
			slog.Error("binder looper read", "err", err)
			return
		}
		if _, err := c.process(buf[:n], false); err != nil {
			slog.Warn("binder looper", "err", err)
		}
	}
}

// writeRead performs one BINDER_WRITE_READ and returns bytes read.
func (c *Conn) writeRead(write, read []byte) (int, error) {
	var bwr writeRead
	if len(write) > 0 {
		bwr.writeSize = uint64(len(write))
		bwr.writeBuffer = uint64(uintptr(unsafe.Pointer(&write[0])))
	}
	if len(read) > 0 {
		bwr.readSize = uint64(len(read))
		bwr.readBuffer = uint64(uintptr(unsafe.Pointer(&read[0])))
	}
	err := ioctl(c.fd, ioctlWriteRead, unsafe.Pointer(&bwr))
	runtime.KeepAlive(write)
	runtime.KeepAlive(read)
	if err != nil {
		return 0, err
	}
	return int(bwr.readConsumed), nil
}

// reply is the outcome of an outgoing transaction.
type reply struct {
	reader *Reader
	err    error
	done   bool
}

// process handles a batch of BR_* returns. When waiting for a reply it
// reports whether the transaction finished.
func (c *Conn) process(buf []byte, oneway bool) (*reply, error) {
	for len(buf) >= 4 {
		cmd := le.Uint32(buf)
		size := payloadSize(cmd)
		if len(buf) < 4+size {
			return nil, errors.New("binder: truncated return")
		}
		arg := buf[4 : 4+size]
		buf = buf[4+size:]

		switch cmd {
		case brNoop, brOK, brSpawnLooper, brRelease, brDecrefs, brClearDeathDone:
		case brIncrefs, brAcquire:
			done := uint32(bcIncrefsDone)
			if cmd == brAcquire {
				done = bcAcquireDone
			}
			out := le.AppendUint32(nil, done)
			out = append(out, arg[:16]...)
			if _, err := c.writeRead(out, nil); err != nil {
				return nil, err
			}
		case brTransaction, brTransactionSecCtx:
			c.serve(arg)
		case brReply:
			r, err := c.takeBuffer(arg)
			return &reply{reader: r, err: err, done: true}, nil
		case brTransactionComplete:
			if oneway {
				return &reply{done: true}, nil
			}
		case brDeadReply:
			return &reply{err: errors.New("binder: dead object"), done: true}, nil
		case brFailedReply, brFrozenReply:
			return &reply{err: errors.New("binder: transaction failed"), done: true}, nil
		case brError:
			return &reply{err: fmt.Errorf("binder: error %d", int32(le.Uint32(arg))), done: true}, nil
		case brDeadBinder:
			cookie := uintptr(le.Uint64(arg))
			c.mu.Lock()
			fn := c.deaths[cookie]
			delete(c.deaths, cookie)
			c.mu.Unlock()
			out := le.AppendUint32(nil, bcDeadBinderDone)
			out = le.AppendUint64(out, uint64(cookie))
			c.writeRead(out, nil)
			if fn != nil {
				go fn()
			}
		default:
			slog.Debug("binder: ignoring return", "cmd", fmt.Sprintf("%#x", cmd))
		}
	}
	return nil, nil
}

// takeBuffer copies a received transaction out of the mmap area and frees
// the kernel buffer.
func (c *Conn) takeBuffer(arg []byte) (*Reader, error) {
	td := (*transactionData)(unsafe.Pointer(&arg[0]))
	if td.flags&flagStatusCode != 0 && td.dataSize >= 4 {
		status := int32(le.Uint32(c.at(td.dataPtr, 4)))
		c.freeBuffer(td.dataPtr)
		return nil, fmt.Errorf("binder: status %d", status)
	}
	data := append([]byte(nil), c.at(td.dataPtr, int(td.dataSize))...)
	var offsets []uint64
	if n := int(td.offsetsSize / 8); n > 0 {
		raw := c.at(td.offsetsPtr, n*8)
		for i := 0; i < n; i++ {
			offsets = append(offsets, le.Uint64(raw[8*i:]))
		}
	}
	r := NewReader(data, offsets)
	// Keep strong references to received handles before the buffer (and
	// with it the kernel's temporary reference) goes away.
	for _, off := range offsets {
		if int(off)+flatObjectSize <= len(data) && le.Uint32(data[off:]) == typeHandle {
			h := le.Uint32(data[off+8:])
			c.refHandle(h)
		}
	}
	c.freeBuffer(td.dataPtr)
	return r, nil
}

// at returns the bytes at a user-space address inside our binder mapping.
func (c *Conn) at(addr uint64, n int) []byte {
	base := uint64(uintptr(unsafe.Pointer(&c.mem[0])))
	off := addr - base
	if addr < base || off+uint64(n) > uint64(len(c.mem)) {
		return nil
	}
	return c.mem[off : off+uint64(n)]
}

func (c *Conn) freeBuffer(ptr uint64) {
	out := le.AppendUint32(nil, bcFreeBuffer)
	out = le.AppendUint64(out, ptr)
	c.writeRead(out, nil)
}

func (c *Conn) refHandle(h uint32) {
	out := le.AppendUint32(nil, bcIncrefs)
	out = le.AppendUint32(out, h)
	out = le.AppendUint32(out, bcAcquire)
	out = le.AppendUint32(out, h)
	c.writeRead(out, nil)
}

// serve dispatches an incoming transaction and sends the reply.
func (c *Conn) serve(arg []byte) {
	td := *(*transactionData)(unsafe.Pointer(&arg[0]))
	r, err := c.takeBuffer(arg)
	oneway := td.flags&FlagOneWay != 0

	c.mu.Lock()
	l := c.locals[uintptr(td.cookie)]
	c.mu.Unlock()

	out := &Parcel{}
	status := int32(0)
	switch {
	case err != nil || l == nil:
		status = statusUnknownTransaction
	case td.code == pingTransaction:
	case td.code == interfaceTransaction:
		out.WriteString16(l.Interface)
	default:
		if _, herr := r.SkipRPCHeader(); herr != nil {
			status = statusUnknownTransaction
		} else if herr := l.handler(td.code, r, out); herr != nil {
			slog.Warn("binder: handler failed", "iface", l.Interface, "code", td.code, "err", herr)
			status = statusUnknownTransaction
		}
	}
	if oneway {
		return
	}
	if status != 0 {
		out = &Parcel{}
		out.WriteInt32(status)
		c.sendReply(out, flagStatusCode)
		return
	}
	c.sendReply(out, 0)
}

func (c *Conn) sendReply(p *Parcel, flags uint32) {
	td := transactionData{flags: flags}
	td.dataSize = uint64(len(p.data))
	if len(p.data) > 0 {
		td.dataPtr = uint64(uintptr(unsafe.Pointer(&p.data[0])))
	}
	if len(p.offsets) > 0 {
		td.offsetsSize = uint64(8 * len(p.offsets))
		td.offsetsPtr = uint64(uintptr(unsafe.Pointer(&p.offsets[0])))
	}
	out := le.AppendUint32(nil, bcReply)
	out = append(out, unsafe.Slice((*byte)(unsafe.Pointer(&td)), unsafe.Sizeof(td))...)
	// The reply completes with BR_TRANSACTION_COMPLETE, read on the next
	// loop iteration of whichever thread is serving.
	if _, err := c.writeRead(out, nil); err != nil {
		slog.Warn("binder: reply failed", "err", err)
	}
	runtime.KeepAlive(p)
}

// Transact calls code on the remote object handle. For one-way calls the
// returned reader is nil.
func (c *Conn) Transact(handle uint32, code uint32, p *Parcel, flags uint32) (*Reader, error) {
	var res *reply
	c.onClient(func() { res = c.transact(handle, code, p, flags) })
	return res.reader, res.err
}

func (c *Conn) transact(handle uint32, code uint32, p *Parcel, flags uint32) *reply {
	oneway := flags&FlagOneWay != 0
	if !oneway {
		flags |= flagAcceptFDs
	}
	td := transactionData{target: uint64(handle), code: code, flags: flags}
	td.dataSize = uint64(len(p.data))
	if len(p.data) > 0 {
		td.dataPtr = uint64(uintptr(unsafe.Pointer(&p.data[0])))
	}
	if len(p.offsets) > 0 {
		td.offsetsSize = uint64(8 * len(p.offsets))
		td.offsetsPtr = uint64(uintptr(unsafe.Pointer(&p.offsets[0])))
	}
	out := le.AppendUint32(nil, bcTransaction)
	out = append(out, unsafe.Slice((*byte)(unsafe.Pointer(&td)), unsafe.Sizeof(td))...)

	buf := make([]byte, 4096)
	n, err := c.writeRead(out, buf)
	runtime.KeepAlive(p)
	for {
		if err != nil {
			return &reply{err: err}
		}
		res, perr := c.process(buf[:n], oneway)
		if perr != nil {
			return &reply{err: perr}
		}
		if res != nil && res.done {
			return res
		}
		n, err = c.writeRead(nil, buf)
	}
}

// LinkToDeath calls fn once when the remote object dies.
func (c *Conn) LinkToDeath(handle uint32, fn func()) {
	c.mu.Lock()
	cookie := c.nextCookie<<4 | 0x8
	c.nextCookie++
	c.deaths[cookie] = fn
	c.mu.Unlock()
	c.onClient(func() {
		out := le.AppendUint32(nil, bcRequestDeath)
		out = le.AppendUint32(out, handle)
		out = le.AppendUint64(out, uint64(cookie))
		c.writeRead(out, nil)
	})
}

// Release drops the strong and weak references taken on a handle.
func (c *Conn) Release(handle uint32) {
	c.onClient(func() {
		out := le.AppendUint32(nil, bcRelease)
		out = le.AppendUint32(out, handle)
		out = le.AppendUint32(out, bcDecrefs)
		out = le.AppendUint32(out, handle)
		c.writeRead(out, nil)
	})
}

// Device returns the binder device path from the environment or default.
func Device() string {
	if d := os.Getenv("LAPIS_BINDER"); d != "" {
		return d
	}
	return "/dev/anbox-binder"
}
