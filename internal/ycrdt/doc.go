package ycrdt

/*
#cgo LDFLAGS: ${SRCDIR}/../../third_party/y-crdt/target/release/libyrs.a
#cgo windows LDFLAGS: -lws2_32 -lntdll -luserenv -ldbghelp
#include <stdlib.h>
#include "yrs_min.h"
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

type Option func(*options)

type options struct {
	clientID ClientID
}

func WithClientID(id ClientID) Option {
	return func(opts *options) {
		opts.clientID = id
	}
}

type Doc struct {
	mu         sync.Mutex
	ptr        *C.YDoc
	closed     bool
	nextUpdate uint64
	updateSubs []updateSub
}

type updateSub struct {
	id uint64
	fn func(update []byte, origin any)
}

type Transaction struct {
	ptr   *C.YTransaction
	write bool
}

type YText struct {
	doc    *Doc
	name   string
	branch *C.Branch
}

type YMap struct {
	doc    *Doc
	name   string
	branch *C.Branch
}

func New(opts ...Option) *Doc {
	cfg := options{}
	for _, opt := range opts {
		opt(&cfg)
	}
	yopts := C.yoptions()
	if cfg.clientID != 0 {
		yopts.id = C.uint64_t(cfg.clientID)
	}
	yopts.encoding = C.Y_OFFSET_UTF16
	ptr := C.ydoc_new_with_options(yopts)
	doc := &Doc{ptr: ptr}
	runtime.SetFinalizer(doc, (*Doc).Close)
	return doc
}

func (d *Doc) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.ptr == nil {
		return
	}
	C.ydoc_destroy(d.ptr)
	d.ptr = nil
	d.closed = true
	runtime.SetFinalizer(d, nil)
}

func (d *Doc) ClientID() ClientID {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.ptr == nil {
		return 0
	}
	return ClientID(C.ydoc_id(d.ptr))
}

func (d *Doc) GetText(name string) *YText {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.ptr == nil {
		return &YText{doc: d, name: name}
	}
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	return &YText{doc: d, name: name, branch: C.ytext(d.ptr, cName)}
}

func (d *Doc) GetMap(name string) *YMap {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.ptr == nil {
		return &YMap{doc: d, name: name}
	}
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	return &YMap{doc: d, name: name, branch: C.ymap(d.ptr, cName)}
}

func (d *Doc) Transact(fn func(*Transaction), origin ...any) {
	_, _ = d.Update(func(txn *Transaction) error {
		fn(txn)
		return nil
	}, origin...)
}

func (d *Doc) Update(fn func(*Transaction) error, origin ...any) ([]byte, error) {
	d.mu.Lock()
	if err := d.checkOpen(); err != nil {
		d.mu.Unlock()
		return nil, err
	}
	txn, err := d.writeTransaction(origin...)
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	before, err := txn.stateVector()
	if err != nil {
		C.ytransaction_commit(txn.ptr)
		d.mu.Unlock()
		return nil, err
	}
	userErr := fn(txn)
	C.ytransaction_commit(txn.ptr)
	if userErr != nil {
		d.mu.Unlock()
		return nil, userErr
	}
	update, err := d.encodeStateAsUpdateLocked(before)
	if err != nil {
		d.mu.Unlock()
		return nil, err
	}
	subs := d.updateSubscribersLocked()
	d.mu.Unlock()
	d.notifyUpdate(subs, update, firstOrigin(origin))
	return update, nil
}

func (d *Doc) Read(fn func(*Transaction) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(); err != nil {
		return err
	}
	txn, err := d.readTransaction()
	if err != nil {
		return err
	}
	defer C.ytransaction_commit(txn.ptr)
	return fn(txn)
}

func (d *Doc) ApplyUpdate(update []byte) error {
	return ApplyUpdateV1(d, update, nil)
}

func ApplyUpdateV1(doc *Doc, update []byte, origin any) error {
	if len(update) == 0 {
		return nil
	}
	doc.mu.Lock()
	if err := doc.checkOpen(); err != nil {
		doc.mu.Unlock()
		return err
	}
	txn, err := doc.writeTransaction(origin)
	if err != nil {
		doc.mu.Unlock()
		return err
	}
	code := C.ytransaction_apply(txn.ptr, cBytes(update), C.uint32_t(len(update)))
	C.ytransaction_commit(txn.ptr)
	if code != 0 {
		doc.mu.Unlock()
		return fmt.Errorf("apply yjs update failed: code %d", uint8(code))
	}
	subs := doc.updateSubscribersLocked()
	doc.mu.Unlock()
	doc.notifyUpdate(subs, update, origin)
	return nil
}

func EncodeStateVectorV1(doc *Doc) []byte {
	vector, _ := doc.StateVectorV1()
	return vector
}

func (d *Doc) StateVectorV1() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(); err != nil {
		return nil, err
	}
	txn, err := d.readTransaction()
	if err != nil {
		return nil, err
	}
	defer C.ytransaction_commit(txn.ptr)
	return txn.stateVector()
}

func (d *Doc) EncodeStateAsUpdate() []byte {
	update, _ := d.EncodeStateAsUpdateV1(nil)
	return update
}

func (d *Doc) EncodeStateAsUpdateV1(remoteStateVector []byte) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(); err != nil {
		return nil, err
	}
	return d.encodeStateAsUpdateLocked(remoteStateVector)
}

func (d *Doc) OnUpdate(fn func(update []byte, origin any)) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return func() {}
	}
	d.nextUpdate++
	id := d.nextUpdate
	d.updateSubs = append(d.updateSubs, updateSub{id: id, fn: fn})
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		for i, sub := range d.updateSubs {
			if sub.id == id {
				d.updateSubs = append(d.updateSubs[:i], d.updateSubs[i+1:]...)
				return
			}
		}
	}
}

func (d *Doc) checkOpen() error {
	if d == nil || d.closed || d.ptr == nil {
		return errors.New("ycrdt document is closed")
	}
	return nil
}

func (d *Doc) updateSubscribersLocked() []func([]byte, any) {
	if len(d.updateSubs) == 0 {
		return nil
	}
	subs := make([]func([]byte, any), len(d.updateSubs))
	for i, sub := range d.updateSubs {
		subs[i] = sub.fn
	}
	return subs
}

func (d *Doc) notifyUpdate(subs []func([]byte, any), update []byte, origin any) {
	if isEmptyUpdate(update) {
		return
	}
	for _, fn := range subs {
		fn(append([]byte(nil), update...), origin)
	}
}

func (d *Doc) readTransaction() (*Transaction, error) {
	ptr := C.ydoc_read_transaction(d.ptr)
	if ptr == nil {
		return nil, errors.New("could not open ycrdt read transaction")
	}
	return &Transaction{ptr: ptr}, nil
}

func (d *Doc) writeTransaction(origin ...any) (*Transaction, error) {
	var originBytes []byte
	if len(origin) > 0 && origin[0] != nil {
		originBytes = []byte(fmt.Sprint(origin[0]))
	}
	ptr := C.ydoc_write_transaction(d.ptr, C.uint32_t(len(originBytes)), cBytes(originBytes))
	if ptr == nil {
		return nil, errors.New("could not open ycrdt write transaction")
	}
	return &Transaction{ptr: ptr, write: true}, nil
}

func (d *Doc) encodeStateAsUpdateLocked(remoteStateVector []byte) ([]byte, error) {
	txn, err := d.readTransaction()
	if err != nil {
		return nil, err
	}
	defer C.ytransaction_commit(txn.ptr)
	var length C.uint32_t
	ptr := C.ytransaction_state_diff_v1(txn.ptr, cBytes(remoteStateVector), C.uint32_t(len(remoteStateVector)), &length)
	if ptr == nil {
		return nil, errors.New("could not encode ycrdt state diff")
	}
	defer C.ybinary_destroy(ptr, length)
	return C.GoBytes(unsafe.Pointer(ptr), C.int(length)), nil
}

func (txn *Transaction) stateVector() ([]byte, error) {
	var length C.uint32_t
	ptr := C.ytransaction_state_vector_v1(txn.ptr, &length)
	if ptr == nil {
		return nil, errors.New("could not encode ycrdt state vector")
	}
	defer C.ybinary_destroy(ptr, length)
	return C.GoBytes(unsafe.Pointer(ptr), C.int(length)), nil
}

func cBytes(data []byte) *C.char {
	if len(data) == 0 {
		return nil
	}
	return (*C.char)(unsafe.Pointer(&data[0]))
}

func firstOrigin(origin []any) any {
	if len(origin) == 0 {
		return nil
	}
	return origin[0]
}

func isEmptyUpdate(update []byte) bool {
	return len(update) == 0 || bytes.Equal(update, []byte{0, 0})
}
