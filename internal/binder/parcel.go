package binder

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"
)

var le = binary.LittleEndian

// Parcel is an outgoing transaction payload.
type Parcel struct {
	data    []byte
	offsets []uint64
}

// NewRequest starts a parcel with the aidl3 RPC header for iface.
func NewRequest(iface string) *Parcel {
	p := &Parcel{}
	p.WriteInt32(strictModePenaltyGather)
	p.WriteInt32(unsetWorkSource)
	p.WriteInt32(sysHeader)
	p.WriteString16(iface)
	return p
}

func (p *Parcel) Bytes() []byte { return p.data }

func (p *Parcel) pad() {
	for len(p.data)%4 != 0 {
		p.data = append(p.data, 0)
	}
}

func (p *Parcel) WriteInt32(v int32) {
	p.data = le.AppendUint32(p.data, uint32(v))
}

func (p *Parcel) WriteUint32(v uint32) { p.data = le.AppendUint32(p.data, v) }

func (p *Parcel) WriteInt64(v int64) {
	p.data = le.AppendUint64(p.data, uint64(v))
}

func (p *Parcel) WriteBool(v bool) {
	if v {
		p.WriteInt32(1)
	} else {
		p.WriteInt32(0)
	}
}

// WriteString16 writes a Java String: length in UTF-16 units, the units,
// a terminating NUL, padded to 4 bytes.
func (p *Parcel) WriteString16(s string) {
	units := utf16.Encode([]rune(s))
	p.WriteInt32(int32(len(units)))
	for _, u := range units {
		p.data = le.AppendUint16(p.data, u)
	}
	p.data = le.AppendUint16(p.data, 0)
	p.pad()
}

// WriteNullString16 writes a null String.
func (p *Parcel) WriteNullString16() { p.WriteInt32(-1) }

// WriteLocalObject flattens a binder object owned by this process.
func (p *Parcel) WriteLocalObject(l *Local) {
	p.offsets = append(p.offsets, uint64(len(p.data)))
	p.WriteUint32(typeBinder)
	p.WriteUint32(flagsLocalObject)
	p.data = le.AppendUint64(p.data, uint64(l.cookie)) // binder (ptr)
	p.data = le.AppendUint64(p.data, uint64(l.cookie)) // cookie
	p.WriteInt32(StabilitySystem)
}

// Reader parses an incoming transaction or reply.
type Reader struct {
	data    []byte
	pos     int
	offsets []uint64
}

var errShort = errors.New("binder: parcel too short")

func NewReader(data []byte, offsets []uint64) *Reader {
	return &Reader{data: data, offsets: offsets}
}

func (r *Reader) need(n int) error {
	if r.pos+n > len(r.data) {
		return errShort
	}
	return nil
}

func (r *Reader) ReadInt32() (int32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := int32(le.Uint32(r.data[r.pos:]))
	r.pos += 4
	return v, nil
}

func (r *Reader) ReadInt64() (int64, error) {
	if err := r.need(8); err != nil {
		return 0, err
	}
	v := int64(le.Uint64(r.data[r.pos:]))
	r.pos += 8
	return v, nil
}

func (r *Reader) ReadBool() (bool, error) {
	v, err := r.ReadInt32()
	return v != 0, err
}

// ReadByte reads a Java byte, which occupies a full int32 slot.
func (r *Reader) ReadByte() (byte, error) {
	v, err := r.ReadInt32()
	return byte(v), err
}

// ReadString16 reads a Java String; a null string reads as "".
func (r *Reader) ReadString16() (string, error) {
	n, err := r.ReadInt32()
	if err != nil || n < 0 {
		return "", err
	}
	size := (int(n) + 1) * 2
	if err := r.need(size); err != nil {
		return "", err
	}
	units := make([]uint16, n)
	for i := range units {
		units[i] = le.Uint16(r.data[r.pos+2*i:])
	}
	r.pos += (size + 3) &^ 3
	return string(utf16.Decode(units)), nil
}

// ReadByteArray reads a byte[]; null reads as nil.
func (r *Reader) ReadByteArray() ([]byte, error) {
	n, err := r.ReadInt32()
	if err != nil || n < 0 {
		return nil, err
	}
	if err := r.need(int(n)); err != nil {
		return nil, err
	}
	b := append([]byte(nil), r.data[r.pos:r.pos+int(n)]...)
	r.pos += (int(n) + 3) &^ 3
	return b, nil
}

// ReadHandle reads a flattened binder and returns its handle. ok is false
// for a null binder.
func (r *Reader) ReadHandle() (handle uint32, ok bool, err error) {
	if err := r.need(flatObjectSize + stabilitySize); err != nil {
		return 0, false, err
	}
	typ := le.Uint32(r.data[r.pos:])
	h := le.Uint64(r.data[r.pos+8:])
	r.pos += flatObjectSize + stabilitySize
	switch typ {
	case typeHandle:
		return uint32(h), true, nil
	case typeBinder:
		if h == 0 {
			return 0, false, nil // null object
		}
		return 0, false, errors.New("binder: received a local object, expected a handle")
	}
	return 0, false, fmt.Errorf("binder: unexpected object type %#x", typ)
}

// SkipRPCHeader consumes the aidl3 header of an incoming request and
// returns the interface name.
func (r *Reader) SkipRPCHeader() (string, error) {
	for i := 0; i < 3; i++ {
		if _, err := r.ReadInt32(); err != nil {
			return "", err
		}
	}
	return r.ReadString16()
}

// ExceptionError is an exception returned by an AIDL service.
type ExceptionError struct {
	Code    int32
	Message string
}

func (e *ExceptionError) Error() string {
	return fmt.Sprintf("binder: remote exception %d: %s", e.Code, e.Message)
}

// ReadException consumes the AIDL status header of a reply.
func (r *Reader) ReadException() error {
	code, err := r.ReadInt32()
	if err != nil {
		return err
	}
	if code == exHasReplyHeader || code == exHasNotedAppOpsReplyHeader {
		size, err := r.ReadInt32()
		if err != nil {
			return err
		}
		// size includes its own int32
		r.pos += int(size) - 4
		if r.pos > len(r.data) {
			return errShort
		}
		code = 0
	}
	if code == 0 {
		return nil
	}
	msg, _ := r.ReadString16()
	return &ExceptionError{Code: code, Message: msg}
}
