package ycrdt

/*
#include <stdlib.h>
#include "yrs_min.h"

static inline char *notty_youtput_string(const struct YOutput *out) {
	return out->value.str;
}
*/
import "C"

import (
	"errors"
	"sort"
	"strings"
	"unicode/utf8"
	"unsafe"
)

const (
	YMapEntryString = "string"
	YMapEntryMap    = "map"
)

type YMapEntry struct {
	Key         string
	ValueKind   string
	StringValue string
	MapValue    *YMap
}

func (m *YMap) GetString(txn *Transaction, key string) (string, bool, error) {
	if txn == nil || txn.ptr == nil {
		return "", false, errors.New("map get requires a transaction")
	}
	if err := validateYMapKey(key); err != nil {
		return "", false, err
	}
	branch, err := m.branchLocked()
	if err != nil {
		return "", false, err
	}
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	out := C.ymap_get(branch, txn.ptr, cKey)
	if out == nil {
		return "", false, nil
	}
	defer C.youtput_destroy(out)
	if out.tag != C.Y_JSON_STR || C.notty_youtput_string(out) == nil {
		return "", false, nil
	}
	return C.GoString(C.notty_youtput_string(out)), true, nil
}

func (m *YMap) SetString(txn *Transaction, key string, value string) error {
	if txn == nil || txn.ptr == nil || !txn.write {
		return errors.New("map set requires a write transaction")
	}
	if err := validateYMapKey(key); err != nil {
		return err
	}
	if !utf8.ValidString(value) {
		return errors.New("map string value is not valid UTF-8")
	}
	if strings.IndexByte(value, 0) >= 0 {
		return errors.New("map string value contains NUL byte")
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

func (m *YMap) GetMap(txn *Transaction, key string) (*YMap, bool, error) {
	if txn == nil || txn.ptr == nil {
		return nil, false, errors.New("map get requires a transaction")
	}
	if err := validateYMapKey(key); err != nil {
		return nil, false, err
	}
	branch, err := m.branchLocked()
	if err != nil {
		return nil, false, err
	}
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	out := C.ymap_get(branch, txn.ptr, cKey)
	if out == nil {
		return nil, false, nil
	}
	defer C.youtput_destroy(out)
	nested := C.youtput_read_ymap(out)
	if nested == nil {
		return nil, false, nil
	}
	return &YMap{doc: m.doc, name: key, branch: nested}, true, nil
}

func (m *YMap) SetMap(txn *Transaction, key string) (*YMap, error) {
	if txn == nil || txn.ptr == nil || !txn.write {
		return nil, errors.New("map set requires a write transaction")
	}
	if err := validateYMapKey(key); err != nil {
		return nil, err
	}
	branch, err := m.branchLocked()
	if err != nil {
		return nil, err
	}
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	input := C.yinput_ymap(nil, nil, 0)
	C.ymap_insert(branch, txn.ptr, cKey, &input)
	nested, ok, err := m.GetMap(txn, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("could not create nested map")
	}
	return nested, nil
}

func (m *YMap) Delete(txn *Transaction, key string) error {
	if txn == nil || txn.ptr == nil || !txn.write {
		return errors.New("map delete requires a write transaction")
	}
	if err := validateYMapKey(key); err != nil {
		return err
	}
	branch, err := m.branchLocked()
	if err != nil {
		return err
	}
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	C.ymap_remove(branch, txn.ptr, cKey)
	return nil
}

func (m *YMap) Entries(txn *Transaction) ([]YMapEntry, error) {
	if txn == nil || txn.ptr == nil {
		return nil, errors.New("map entries require a transaction")
	}
	branch, err := m.branchLocked()
	if err != nil {
		return nil, err
	}
	iter := C.ymap_iter(branch, txn.ptr)
	if iter == nil {
		return nil, errors.New("could not create map iterator")
	}
	defer C.ymap_iter_destroy(iter)
	entries := []YMapEntry{}
	for {
		item := C.ymap_iter_next(iter)
		if item == nil {
			break
		}
		func() {
			defer C.ymap_entry_destroy(item)
			entry := YMapEntry{Key: C.GoString(item.key)}
			if item.value == nil {
				entries = append(entries, entry)
				return
			}
			switch item.value.tag {
			case C.Y_JSON_STR:
				entry.ValueKind = YMapEntryString
				if C.notty_youtput_string(item.value) != nil {
					entry.StringValue = C.GoString(C.notty_youtput_string(item.value))
				}
			case C.Y_MAP:
				entry.ValueKind = YMapEntryMap
				if nested := C.youtput_read_ymap(item.value); nested != nil {
					entry.MapValue = &YMap{doc: m.doc, name: entry.Key, branch: nested}
				}
			}
			entries = append(entries, entry)
		}()
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries, nil
}

func (m *YMap) branchLocked() (*C.Branch, error) {
	if m == nil || m.branch == nil {
		return nil, errors.New("could not get ycrdt map")
	}
	return m.branch, nil
}

func validateYMapKey(key string) error {
	if key == "" {
		return errors.New("map key is required")
	}
	if !utf8.ValidString(key) {
		return errors.New("map key is not valid UTF-8")
	}
	if strings.IndexByte(key, 0) >= 0 {
		return errors.New("map key contains NUL byte")
	}
	return nil
}
