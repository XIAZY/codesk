package yproto

import (
	"bytes"
	"errors"
	"io"
)

const (
	MessageSync          = 0
	MessageAwareness     = 1
	MessageNottyDocument = 42
)

const (
	SyncStep1  = 0
	SyncStep2  = 1
	SyncUpdate = 2
)

type AwarenessState struct {
	Clock int64
	State []byte
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

func encodeVarBytes(value []byte) []byte {
	return append(encodeVarUint(uint64(len(value))), value...)
}

func decodeVarBytes(reader *bytes.Reader) ([]byte, error) {
	length, err := decodeVarUint(reader)
	if err != nil {
		return nil, err
	}
	buffer := make([]byte, length)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return nil, err
	}
	return buffer, nil
}

func encodeVarString(value string) []byte {
	return encodeVarBytes([]byte(value))
}

func decodeVarString(reader *bytes.Reader) (string, error) {
	value, err := decodeVarBytes(reader)
	if err != nil {
		return "", err
	}
	return string(value), nil
}

func BuildDocumentMessage(documentID string, payload []byte) []byte {
	message := append(encodeVarUint(MessageNottyDocument), encodeVarString(documentID)...)
	return append(message, encodeVarBytes(payload)...)
}

func DecodeDocumentMessage(payload []byte) (string, []byte, error) {
	messageType, reader, err := DecodeProtocolMessage(payload)
	if err != nil {
		return "", nil, err
	}
	if messageType != MessageNottyDocument {
		return "", nil, errors.New("unexpected document message type")
	}
	documentID, err := decodeVarString(reader)
	if err != nil {
		return "", nil, err
	}
	if documentID == "" {
		return "", nil, errors.New("document id is required")
	}
	documentPayload, err := decodeVarBytes(reader)
	if err != nil {
		return "", nil, err
	}
	if len(documentPayload) == 0 {
		return "", nil, errors.New("document payload is required")
	}
	if reader.Len() != 0 {
		return "", nil, errors.New("document message has trailing bytes")
	}
	return documentID, documentPayload, nil
}

func BuildSyncStep1FromStateVector(stateVector []byte) []byte {
	message := append(encodeVarUint(MessageSync), encodeVarUint(SyncStep1)...)
	return append(message, encodeVarBytes(stateVector)...)
}

func BuildSyncStep2FromUpdate(update []byte) []byte {
	message := append(encodeVarUint(MessageSync), encodeVarUint(SyncStep2)...)
	return append(message, encodeVarBytes(update)...)
}

func BuildSyncUpdate(update []byte) []byte {
	message := append(encodeVarUint(MessageSync), encodeVarUint(SyncUpdate)...)
	return append(message, encodeVarBytes(update)...)
}

func DecodeProtocolMessage(payload []byte) (uint64, *bytes.Reader, error) {
	reader := bytes.NewReader(payload)
	messageType, err := decodeVarUint(reader)
	if err != nil {
		return 0, nil, err
	}
	return messageType, reader, nil
}

func DecodeSyncMessage(reader *bytes.Reader) (uint64, []byte, error) {
	messageType, err := decodeVarUint(reader)
	if err != nil {
		return 0, nil, err
	}
	switch messageType {
	case SyncStep1, SyncStep2, SyncUpdate:
		value, err := decodeVarBytes(reader)
		return messageType, value, err
	default:
		return 0, nil, errors.New("unknown sync message type")
	}
}

func BuildAwarenessUpdate(states map[uint64]AwarenessState, clients []uint64) []byte {
	message := append(encodeVarUint(MessageAwareness), encodeVarUint(uint64(len(clients)))...)
	for _, clientID := range clients {
		state := states[clientID]
		message = append(message, encodeVarUint(clientID)...)
		message = append(message, encodeVarUint(uint64(state.Clock))...)
		message = append(message, encodeVarString(string(state.State))...)
	}
	return message
}

func DecodeAwarenessUpdate(reader *bytes.Reader) (map[uint64]AwarenessState, error) {
	count, err := decodeVarUint(reader)
	if err != nil {
		return nil, err
	}
	states := make(map[uint64]AwarenessState, count)
	for index := uint64(0); index < count; index++ {
		clientID, err := decodeVarUint(reader)
		if err != nil {
			return nil, err
		}
		clock, err := decodeVarUint(reader)
		if err != nil {
			return nil, err
		}
		stateText, err := decodeVarString(reader)
		if err != nil {
			return nil, err
		}
		states[clientID] = AwarenessState{
			Clock: int64(clock),
			State: []byte(stateText),
		}
	}
	return states, nil
}
