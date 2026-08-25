package jdbcbridge

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"
)

const (
	protocolVersion byte = 1
	responseMarker  byte = 0x80
	maxFrameSize         = 64 << 20
)

const (
	opHello byte = iota + 1
	opOpenSession
	opPing
	opListSchemas
	opExec
	opQueryInt64
	opBatchBegin
	opBatchAdd
	opBatchCommit
	opBatchAbort
	opCloseSession
	opCancel
	opShutdown
)

const (
	valueNull byte = iota
	valueBool
	valueInt64
	valueUint64
	valueFloat64
	valueDecimal
	valueString
	valueBytes
	valueDate
	valueTime
	valueTimestamp
)

// Decimal keeps an exact decimal value while it crosses the Go/JDBC bridge.
type Decimal string

// Date, Time and Timestamp select the matching JDBC setter.
type Date string
type Time string
type Timestamp string

type encoder struct{ bytes.Buffer }

func (e *encoder) byte(v byte) { _ = e.WriteByte(v) }
func (e *encoder) bool(v bool) {
	if v {
		e.byte(1)
	} else {
		e.byte(0)
	}
}
func (e *encoder) uint32(v uint32) { _ = binary.Write(&e.Buffer, binary.BigEndian, v) }
func (e *encoder) int32(v int32)   { _ = binary.Write(&e.Buffer, binary.BigEndian, v) }
func (e *encoder) uint64(v uint64) { _ = binary.Write(&e.Buffer, binary.BigEndian, v) }
func (e *encoder) int64(v int64)   { _ = binary.Write(&e.Buffer, binary.BigEndian, v) }
func (e *encoder) float64(v float64) {
	e.uint64(math.Float64bits(v))
}
func (e *encoder) string(v string) {
	b := []byte(v)
	e.uint32(uint32(len(b)))
	_, _ = e.Write(b)
}
func (e *encoder) bytes(v []byte) {
	e.uint32(uint32(len(v)))
	_, _ = e.Write(v)
}

type decoder struct{ *bytes.Reader }

func newDecoder(b []byte) *decoder     { return &decoder{Reader: bytes.NewReader(b)} }
func (d *decoder) byte() (byte, error) { return d.ReadByte() }
func (d *decoder) bool() (bool, error) {
	v, err := d.byte()
	return v != 0, err
}
func (d *decoder) uint32() (uint32, error) {
	var v uint32
	err := binary.Read(d.Reader, binary.BigEndian, &v)
	return v, err
}
func (d *decoder) int32() (int32, error) {
	var v int32
	err := binary.Read(d.Reader, binary.BigEndian, &v)
	return v, err
}
func (d *decoder) uint64() (uint64, error) {
	var v uint64
	err := binary.Read(d.Reader, binary.BigEndian, &v)
	return v, err
}
func (d *decoder) int64() (int64, error) {
	var v int64
	err := binary.Read(d.Reader, binary.BigEndian, &v)
	return v, err
}
func (d *decoder) string() (string, error) {
	n, err := d.uint32()
	if err != nil {
		return "", err
	}
	if n > maxFrameSize || uint64(n) > uint64(d.Len()) {
		return "", fmt.Errorf("invalid bridge string length %d", n)
	}
	b := make([]byte, n)
	_, err = io.ReadFull(d.Reader, b)
	return string(b), err
}

func encodeValue(e *encoder, value any) error {
	switch v := value.(type) {
	case nil:
		e.byte(valueNull)
	case bool:
		e.byte(valueBool)
		e.bool(v)
	case int:
		e.byte(valueInt64)
		e.int64(int64(v))
	case int8:
		e.byte(valueInt64)
		e.int64(int64(v))
	case int16:
		e.byte(valueInt64)
		e.int64(int64(v))
	case int32:
		e.byte(valueInt64)
		e.int64(int64(v))
	case int64:
		e.byte(valueInt64)
		e.int64(v)
	case uint:
		e.byte(valueUint64)
		e.string(fmt.Sprint(v))
	case uint8:
		e.byte(valueUint64)
		e.string(fmt.Sprint(v))
	case uint16:
		e.byte(valueUint64)
		e.string(fmt.Sprint(v))
	case uint32:
		e.byte(valueUint64)
		e.string(fmt.Sprint(v))
	case uint64:
		e.byte(valueUint64)
		e.string(fmt.Sprint(v))
	case float32:
		e.byte(valueFloat64)
		e.float64(float64(v))
	case float64:
		e.byte(valueFloat64)
		e.float64(v)
	case Decimal:
		e.byte(valueDecimal)
		e.string(string(v))
	case string:
		e.byte(valueString)
		e.string(v)
	case []byte:
		e.byte(valueBytes)
		e.bytes(v)
	case Date:
		e.byte(valueDate)
		e.string(string(v))
	case Time:
		e.byte(valueTime)
		e.string(string(v))
	case Timestamp:
		e.byte(valueTimestamp)
		e.string(string(v))
	case time.Time:
		e.byte(valueTimestamp)
		e.string(v.Format("2006-01-02 15:04:05.999999999"))
	default:
		return fmt.Errorf("unsupported JDBC bridge value type %T", value)
	}
	return nil
}

func encodeRows(rows [][]any) ([]byte, error) {
	e := &encoder{}
	e.uint32(uint32(len(rows)))
	for _, row := range rows {
		e.uint32(uint32(len(row)))
		for _, value := range row {
			if err := encodeValue(e, value); err != nil {
				return nil, err
			}
		}
	}
	return e.Bytes(), nil
}
