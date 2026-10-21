package binder

import "testing"

func TestParcelRoundTrip(t *testing.T) {
	p := NewRequest("lineageos.example.IPlatform")
	p.WriteString16("héllo, 世界")
	p.WriteNullString16()
	p.WriteInt64(-5)
	p.WriteBool(true)
	if len(p.Bytes())%4 != 0 {
		t.Fatalf("parcel not 4-byte aligned: %d", len(p.Bytes()))
	}

	r := NewReader(p.Bytes(), nil)
	iface, err := r.SkipRPCHeader()
	if err != nil || iface != "lineageos.example.IPlatform" {
		t.Fatalf("header: %q %v", iface, err)
	}
	if s, _ := r.ReadString16(); s != "héllo, 世界" {
		t.Errorf("string16 = %q", s)
	}
	if s, err := r.ReadString16(); s != "" || err != nil {
		t.Errorf("null string = %q %v", s, err)
	}
	if v, _ := r.ReadInt64(); v != -5 {
		t.Errorf("int64 = %d", v)
	}
	if v, _ := r.ReadBool(); !v {
		t.Error("bool = false")
	}
	if _, err := r.ReadInt32(); err == nil {
		t.Error("expected end of parcel")
	}
}

func TestReadException(t *testing.T) {
	p := &Parcel{}
	p.WriteInt32(-3) // EX_ILLEGAL_ARGUMENT
	p.WriteString16("bad")
	err := NewReader(p.Bytes(), nil).ReadException()
	if e, ok := err.(*ExceptionError); !ok || e.Code != -3 || e.Message != "bad" {
		t.Errorf("got %v", err)
	}

	// Reply header (-128) is skipped, then the payload follows.
	p = &Parcel{}
	p.WriteInt32(exHasReplyHeader)
	p.WriteInt32(8)
	p.WriteInt32(0)
	p.WriteInt32(42)
	r := NewReader(p.Bytes(), nil)
	if err := r.ReadException(); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.ReadInt32(); v != 42 {
		t.Errorf("payload = %d", v)
	}
}

func TestLocalObjectEncoding(t *testing.T) {
	l := &Local{cookie: 0x10}
	p := &Parcel{}
	p.WriteInt32(7)
	p.WriteLocalObject(l)
	if len(p.offsets) != 1 || p.offsets[0] != 4 {
		t.Fatalf("offsets = %v", p.offsets)
	}
	b := p.Bytes()
	if le.Uint32(b[4:]) != typeBinder || le.Uint64(b[12:]) != 0x10 || le.Uint32(b[28:]) != StabilitySystem {
		t.Errorf("bad flat_binder_object: % x", b)
	}
}
