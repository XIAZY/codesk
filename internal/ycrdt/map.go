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

func (m *YMap) Len() int {
	length, _ := m.Length()
	return length
}

func (m *YMap) Length() (int, error) {
	m.doc.mu.Lock()
	defer m.doc.mu.Unlock()
	if err := m.doc.checkOpen(); err != nil {
		return 0, err
	}
	txn, err := m.doc.readTransaction()
	if err != nil {
		return 0, err
	}
	defer C.ytransaction_commit(txn.ptr)
	branch, err := m.branchLocked()
	if err != nil {
		return 0, err
	}
	return int(C.ymap_len(branch, txn.ptr)), nil
}

func (m *YMap) LenInTxn(txn *Transaction) int {
	length, _ := m.LengthInTxn(txn)
	return length
}

func (m *YMap) LengthInTxn(txn *Transaction) (int, error) {
	if txn == nil || txn.ptr == nil {
		return 0, errors.New("length requires a transaction")
	}
	branch, err := m.branchLocked()
	if err != nil {
		return 0, err
	}
	return int(C.ymap_len(branch, txn.ptr)), nil
}

func (m *YMap) InsertJSON(txn *Transaction, key string, jsonValue string) error {
	if txn == nil || txn.ptr == nil || !txn.write {
		return errors.New("map insert requires a write transaction")
	}
	branch, err := m.branchLocked()
	if err != nil {
		return err
	}
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	cValue := C.CString(jsonValue)
	defer C.free(unsafe.Pointer(cValue))
	input := C.yinput_json(cValue)
	C.ymap_insert(branch, txn.ptr, cKey, &input)
	return nil
}

func (m *YMap) InsertString(txn *Transaction, key string, value string) error {
	if txn == nil || txn.ptr == nil || !txn.write {
		return errors.New("map insert requires a write transaction")
	}
	branch, err := m.branchLocked()
	if err != nil {
		return err
	}
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	cValue := C.CString(value)
	defer C.free(unsafe.Pointer(cValue))
	input := C.yinput_string(cValue)
	C.ymap_insert(branch, txn.ptr, cKey, &input)
	return nil
}

func (m *YMap) Remove(txn *Transaction, key string) (bool, error) {
	if txn == nil || txn.ptr == nil || !txn.write {
		return false, errors.New("map remove requires a write transaction")
	}
	branch, err := m.branchLocked()
	if err != nil {
		return false, err
	}
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	return C.ymap_remove(branch, txn.ptr, cKey) != 0, nil
}

func (m *YMap) RemoveAll(txn *Transaction) error {
	if txn == nil || txn.ptr == nil || !txn.write {
		return errors.New("map remove all requires a write transaction")
	}
	branch, err := m.branchLocked()
	if err != nil {
		return err
	}
	C.ymap_remove_all(branch, txn.ptr)
	return nil
}

func (m *YMap) GetJSON(key string) (string, bool, error) {
	m.doc.mu.Lock()
	defer m.doc.mu.Unlock()
	if err := m.doc.checkOpen(); err != nil {
		return "", false, err
	}
	txn, err := m.doc.readTransaction()
	if err != nil {
		return "", false, err
	}
	defer C.ytransaction_commit(txn.ptr)
	return m.GetJSONInTxn(txn, key)
}

func (m *YMap) JSON() (string, error) {
	m.doc.mu.Lock()
	defer m.doc.mu.Unlock()
	if err := m.doc.checkOpen(); err != nil {
		return "", err
	}
	txn, err := m.doc.readTransaction()
	if err != nil {
		return "", err
	}
	defer C.ytransaction_commit(txn.ptr)
	return m.JSONInTxn(txn)
}

func (m *YMap) JSONInTxn(txn *Transaction) (string, error) {
	if txn == nil || txn.ptr == nil {
		return "", errors.New("map json requires a transaction")
	}
	branch, err := m.branchLocked()
	if err != nil {
		return "", err
	}
	ptr := C.ybranch_json(branch, txn.ptr)
	if ptr == nil {
		return "", errors.New("could not encode map json")
	}
	defer C.ystring_destroy(ptr)
	return C.GoString(ptr), nil
}

func (m *YMap) GetJSONInTxn(txn *Transaction, key string) (string, bool, error) {
	if txn == nil || txn.ptr == nil {
		return "", false, errors.New("map get requires a transaction")
	}
	branch, err := m.branchLocked()
	if err != nil {
		return "", false, err
	}
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	ptr := C.ymap_get_json(branch, txn.ptr, cKey)
	if ptr == nil {
		return "", false, nil
	}
	defer C.ystring_destroy(ptr)
	return C.GoString(ptr), true, nil
}

func (m *YMap) branchLocked() (*C.Branch, error) {
	if m.branch == nil {
		return nil, errors.New("could not get ycrdt map")
	}
	return m.branch, nil
}
