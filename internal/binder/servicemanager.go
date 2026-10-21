package binder

import (
	"context"
	"fmt"
	"time"
)

// Service manager (android.os.IServiceManager, Android 11+) lives at
// handle 0.
const (
	smInterface    = "android.os.IServiceManager"
	smGetService   = FirstCall
	smCheckService = FirstCall + 1
	smAddService   = FirstCall + 2
	smListServices = FirstCall + 3

	dumpPriorityDefault = 0x08
	dumpPriorityAll     = 0x0f
)

// CheckService returns the handle of a registered service without waiting.
func (c *Conn) CheckService(name string) (uint32, bool, error) {
	p := NewRequest(smInterface)
	p.WriteString16(name)
	r, err := c.Transact(0, smCheckService, p, 0)
	if err != nil {
		return 0, false, err
	}
	if err := r.ReadException(); err != nil {
		return 0, false, err
	}
	return r.ReadHandle()
}

// WaitService polls until a service is registered.
func (c *Conn) WaitService(ctx context.Context, name string) (uint32, error) {
	for {
		h, ok, err := c.CheckService(name)
		if err == nil && ok {
			return h, nil
		}
		select {
		case <-ctx.Done():
			if err == nil {
				err = fmt.Errorf("service %s not available", name)
			}
			return 0, fmt.Errorf("%w (%v)", ctx.Err(), err)
		case <-time.After(time.Second):
		}
	}
}

// AddService publishes a local object under name.
func (c *Conn) AddService(name string, l *Local) error {
	p := NewRequest(smInterface)
	p.WriteString16(name)
	p.WriteLocalObject(l)
	p.WriteBool(false) // allowIsolated
	p.WriteInt32(dumpPriorityDefault)
	r, err := c.Transact(0, smAddService, p, 0)
	if err != nil {
		return err
	}
	return r.ReadException()
}

// ListServices returns all registered service names.
func (c *Conn) ListServices() ([]string, error) {
	p := NewRequest(smInterface)
	p.WriteInt32(dumpPriorityAll)
	r, err := c.Transact(0, smListServices, p, 0)
	if err != nil {
		return nil, err
	}
	if err := r.ReadException(); err != nil {
		return nil, err
	}
	n, err := r.ReadInt32()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	for i := int32(0); i < n; i++ {
		s, err := r.ReadString16()
		if err != nil {
			return out, err
		}
		out = append(out, s)
	}
	return out, nil
}
