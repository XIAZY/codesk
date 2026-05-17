package ycrdt

import (
	"bytes"
	"errors"
	"io"
)

type ClientID uint64

type StateVector map[ClientID]uint64

func (s StateVector) Clock(client ClientID) uint64 {
	if s == nil {
		return 0
	}
	return s[client]
}

func DecodeStateVectorV1(data []byte) (StateVector, error) {
	reader := bytes.NewReader(data)
	count, err := decodeVarUint(reader)
	if err != nil {
		return nil, err
	}
	vector := make(StateVector, count)
	for i := uint64(0); i < count; i++ {
		client, err := decodeVarUint(reader)
		if err != nil {
			return nil, err
		}
		clock, err := decodeVarUint(reader)
		if err != nil {
			return nil, err
		}
		vector[ClientID(client)] = clock
	}
	if reader.Len() != 0 {
		return nil, errors.New("state vector has trailing bytes")
	}
	return vector, nil
}

func encodeVarUint(value uint64) []byte {
	buffer := make([]byte, 0, 10)
	for {
		next := byte(value & 0x7f)
		value >>= 7
		if value == 0 {
			buffer = append(buffer, next)
			return buffer
		}
		buffer = append(buffer, next|0x80)
	}
}

func decodeVarUint(reader *bytes.Reader) (uint64, error) {
	var (
		value uint64
		shift uint
	)
	for {
		current, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, err
		}
		value |= uint64(current&0x7f) << shift
		if current&0x80 == 0 {
			return value, nil
		}
		shift += 7
		if shift > 63 {
			return 0, errors.New("varuint overflow")
		}
	}
}
