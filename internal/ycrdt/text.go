package ycrdt

/*
#include <stdlib.h>
#include "yrs_min.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

func (t *YText) ToString() string {
	value, _ := t.String()
	return value
}

func (t *YText) String() (string, error) {
	t.doc.mu.Lock()
	defer t.doc.mu.Unlock()
	if err := t.doc.checkOpen(); err != nil {
		return "", err
	}
	txn, err := t.doc.readTransaction()
	if err != nil {
		return "", err
	}
	defer C.ytransaction_commit(txn.ptr)
	return t.stringLocked(txn)
}

func (t *YText) Len() int {
	length, _ := t.Length()
	return length
}

func (t *YText) Length() (int, error) {
	t.doc.mu.Lock()
	defer t.doc.mu.Unlock()
	if err := t.doc.checkOpen(); err != nil {
		return 0, err
	}
	txn, err := t.doc.readTransaction()
	if err != nil {
		return 0, err
	}
	defer C.ytransaction_commit(txn.ptr)
	branch, err := t.branchLocked()
	if err != nil {
		return 0, err
	}
	return int(C.ytext_len(branch, txn.ptr)), nil
}

func (t *YText) LenInTxn(txn *Transaction) int {
	length, _ := t.LengthInTxn(txn)
	return length
}

func (t *YText) LengthInTxn(txn *Transaction) (int, error) {
	if txn == nil || txn.ptr == nil {
		return 0, errors.New("length requires a transaction")
	}
	branch, err := t.branchLocked()
	if err != nil {
		return 0, err
	}
	return int(C.ytext_len(branch, txn.ptr)), nil
}

func (t *YText) Insert(txn *Transaction, index int, value string, attrs any) {
	_ = t.InsertValue(txn, index, value)
}

func (t *YText) InsertValue(txn *Transaction, index int, value string) error {
	if txn == nil || txn.ptr == nil || !txn.write {
		return errors.New("insert requires a write transaction")
	}
	if index < 0 {
		return errors.New("insert index is negative")
	}
	branch, err := t.branchLocked()
	if err != nil {
		return err
	}
	currentLen := int(C.ytext_len(branch, txn.ptr))
	if index > currentLen {
		return errors.New("insert index is outside text length")
	}
	cValue := C.CString(value)
	defer C.free(unsafe.Pointer(cValue))
	C.ytext_insert(branch, txn.ptr, C.uint32_t(index), cValue, nil)
	return nil
}

func (t *YText) Delete(txn *Transaction, index int, length int) {
	_ = t.DeleteRange(txn, index, length)
}

func (t *YText) DeleteRange(txn *Transaction, index int, length int) error {
	if txn == nil || txn.ptr == nil || !txn.write {
		return errors.New("delete requires a write transaction")
	}
	if index < 0 || length < 0 {
		return errors.New("delete range is negative")
	}
	if length == 0 {
		return nil
	}
	branch, err := t.branchLocked()
	if err != nil {
		return err
	}
	currentLen := int(C.ytext_len(branch, txn.ptr))
	if index+length > currentLen {
		return errors.New("delete range is outside text length")
	}
	C.ytext_remove_range(branch, txn.ptr, C.uint32_t(index), C.uint32_t(length))
	return nil
}

func (t *YText) EncodeRelativeAnchor(index int, assoc int) ([]byte, error) {
	t.doc.mu.Lock()
	defer t.doc.mu.Unlock()
	if err := t.doc.checkOpen(); err != nil {
		return nil, err
	}
	txn, err := t.doc.writeTransaction("relative-anchor")
	if err != nil {
		return nil, err
	}
	defer C.ytransaction_commit(txn.ptr)
	return t.RelativeAnchorFromIndex(txn, index, assoc)
}

func (t *YText) DecodeRelativeAnchor(anchor []byte) (int, bool, error) {
	t.doc.mu.Lock()
	defer t.doc.mu.Unlock()
	if err := t.doc.checkOpen(); err != nil {
		return 0, false, err
	}
	txn, err := t.doc.readTransaction()
	if err != nil {
		return 0, false, err
	}
	defer C.ytransaction_commit(txn.ptr)
	return t.IndexFromRelativeAnchor(txn, anchor)
}

func (t *YText) RelativeAnchorFromIndex(txn *Transaction, index int, assoc int) ([]byte, error) {
	if txn == nil || txn.ptr == nil {
		return nil, errors.New("relative anchor requires a transaction")
	}
	if index < 0 {
		return nil, errors.New("relative anchor index is negative")
	}
	branch, err := t.branchLocked()
	if err != nil {
		return nil, err
	}
	if index > int(C.ytext_len(branch, txn.ptr)) {
		return nil, errors.New("relative anchor index is outside text length")
	}
	pos := C.ysticky_index_from_index(branch, txn.ptr, C.uint32_t(index), C.int8_t(assoc))
	if pos == nil {
		return nil, errors.New("could not create ycrdt sticky index")
	}
	defer C.ysticky_index_destroy(pos)
	var length C.uint32_t
	ptr := C.ysticky_index_encode(pos, &length)
	if ptr == nil {
		return nil, errors.New("could not encode ycrdt sticky index")
	}
	defer C.ybinary_destroy(ptr, length)
	return C.GoBytes(unsafe.Pointer(ptr), C.int(length)), nil
}

func (t *YText) IndexFromRelativeAnchor(txn *Transaction, anchor []byte) (int, bool, error) {
	if txn == nil || txn.ptr == nil {
		return 0, false, errors.New("relative anchor requires a transaction")
	}
	pos := C.ysticky_index_decode(cBytes(anchor), C.uint32_t(len(anchor)))
	if pos == nil {
		return 0, false, nil
	}
	defer C.ysticky_index_destroy(pos)
	var branch *C.Branch
	var index C.uint32_t
	C.ysticky_index_read(pos, txn.ptr, &branch, &index)
	if branch == nil {
		return 0, false, nil
	}
	return int(index), true, nil
}

func (t *YText) stringLocked(txn *Transaction) (string, error) {
	branch, err := t.branchLocked()
	if err != nil {
		return "", err
	}
	ptr := C.ytext_string(branch, txn.ptr)
	if ptr == nil {
		return "", errors.New("could not read ycrdt text")
	}
	defer C.ystring_destroy(ptr)
	return C.GoString(ptr), nil
}

func (t *YText) branchLocked() (*C.Branch, error) {
	if t.branch == nil {
		return nil, errors.New("could not get ycrdt text")
	}
	return t.branch, nil
}
