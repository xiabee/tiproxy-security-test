// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package logger

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

var (
	_pool = buffer.NewPool()
)

type tidbEncoder struct {
	line *buffer.Buffer
	zapcore.EncoderConfig
	openNamespaces int
}

func NewTiDBEncoder(cfg zapcore.EncoderConfig) *tidbEncoder {
	if cfg.ConsoleSeparator == "" {
		cfg.ConsoleSeparator = "\t"
	}
	if cfg.LineEnding == "" {
		cfg.LineEnding = zapcore.DefaultLineEnding
	}
	return &tidbEncoder{_pool.Get(), cfg, 0}
}

func (c tidbEncoder) clone(keepOld bool) *tidbEncoder {
	newbuf := _pool.Get()
	if keepOld {
		newbuf.AppendString(c.line.String())
	}
	return &tidbEncoder{newbuf, c.EncoderConfig, 0}
}

func (c tidbEncoder) Clone() zapcore.Encoder {
	// this API is called on logger.With/Named or any other log deriviation action
	// thus old fields must be kept
	return c.clone(true)
}

func (c *tidbEncoder) beginQuoteFiled() {
	if c.line.Len() > 0 {
		c.line.AppendByte(' ')
	}
	c.line.AppendByte('[')
}
func (c *tidbEncoder) endQuoteFiled() {
	c.line.AppendByte(']')
}
func (e *tidbEncoder) EncodeEntry(ent zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	// clone here to ensure concurrent safety, note that:
	// 1. the buffer will be used/freed by caller
	// 2. we want to start with time format.. etc, so we don't copy old fields
	c := e.clone(false)
	if c.TimeKey != "" {
		c.beginQuoteFiled()
		if c.EncodeTime != nil {
			c.EncodeTime(ent.Time, c)
		} else {
			c.AppendString(ent.Time.Format("2006/01/02 15:04:05.000 -07:00"))
		}
		c.endQuoteFiled()
	}
	if c.LevelKey != "" && c.EncodeLevel != nil {
		c.beginQuoteFiled()
		c.EncodeLevel(ent.Level, c)
		c.endQuoteFiled()
	}
	if ent.LoggerName != "" && c.NameKey != "" {
		c.beginQuoteFiled()
		nameEncoder := c.EncodeName
		if nameEncoder == nil {
			nameEncoder = zapcore.FullNameEncoder
		}
		nameEncoder(ent.LoggerName, c)
		c.endQuoteFiled()
	}
	if ent.Caller.Defined {
		c.beginQuoteFiled()
		if c.CallerKey != "" && c.EncodeCaller != nil {
			c.EncodeCaller(ent.Caller, c)
		}
		if c.FunctionKey != "" {
			c.AppendString(ent.Caller.Function)
		}
		c.endQuoteFiled()
	}

	// Add the message itself.
	if c.MessageKey != "" {
		c.beginQuoteFiled()
		c.line.AppendString(ent.Message)
		c.endQuoteFiled()
	}

	if c.line.Len() > 0 {
		c.line.AppendByte(' ')
	}

	// append old fields
	c.line.AppendString(e.line.String())

	for _, f := range fields {
		f.AddTo(c)
	}

	c.closeOpenNamespaces()

	if ent.Stack != "" && c.StacktraceKey != "" {
		c.beginQuoteFiled()
		c.line.AppendString(ent.Stack)
		c.endQuoteFiled()
	}

	c.line.AppendString(c.LineEnding)

	return c.line, nil
}

/* map encoder part */
func (f *tidbEncoder) safeAddString(s string) {
	needQuotes := false
outerloop:
	for _, b := range s {
		if b <= 0x20 {
			needQuotes = true
			break outerloop
		}
		switch b {
		case '\\', '"', '[', ']', '=':
			needQuotes = true
			break outerloop
		}
	}
	if needQuotes {
		f.line.AppendByte('"')
	}

	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError {
			f.line.AppendString(`\ufffd`)
		} else if size == 1 {
			switch r {
			case '"':
				f.line.AppendString("\\\"")
			case '\n':
				f.line.AppendString("\\n")
			case '\r':
				f.line.AppendString("\\r")
			case '\t':
				f.line.AppendString("\\t")
			default:
				if unicode.IsControl(r) {
					f.line.AppendString(`\u`)
					fmt.Fprintf(f.line, "%04x", r)
				} else {
					f.line.AppendByte(s[i])
				}
			}
		} else {
			f.line.AppendString(s[i : i+size])
		}
		i += size
	}

	if needQuotes {
		f.line.AppendByte('"')
	}
}
func (s *tidbEncoder) addKey(key string) {
	s.addElementSeparator()
	s.safeAddString(key)
	s.line.AppendByte('=')
}
func (s *tidbEncoder) AddArray(key string, arr zapcore.ArrayMarshaler) error {
	s.beginQuoteFiled()
	s.addKey(key)
	err := s.AppendArray(arr)
	s.endQuoteFiled()
	return err
}
func (s *tidbEncoder) AddObject(key string, obj zapcore.ObjectMarshaler) error {
	s.beginQuoteFiled()
	s.addKey(key)
	err := s.AppendObject(obj)
	s.endQuoteFiled()
	return err
}
func (s *tidbEncoder) AddBinary(key string, val []byte) {
	s.AddString(key, base64.StdEncoding.EncodeToString(val))
}
func (s *tidbEncoder) AddByteString(key string, val []byte) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendByteString(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddBool(key string, val bool) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendBool(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddComplex128(key string, val complex128) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendComplex128(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddComplex64(key string, val complex64) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendComplex64(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddDuration(key string, val time.Duration) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendDuration(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddFloat64(key string, val float64) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendFloat64(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddFloat32(key string, val float32) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendFloat32(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddInt(key string, val int) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendInt(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddInt8(key string, val int8) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendInt8(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddInt16(key string, val int16) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendInt16(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddInt32(key string, val int32) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendInt32(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddInt64(key string, val int64) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendInt64(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddString(key string, val string) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.safeAddString(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddTime(key string, val time.Time) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendTime(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddUint(key string, val uint) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendUint(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddUint8(key string, val uint8) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendUint8(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddUint16(key string, val uint16) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendUint16(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddUint32(key string, val uint32) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendUint32(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddUint64(key string, val uint64) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendUint64(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddUintptr(key string, val uintptr) {
	s.beginQuoteFiled()
	s.addKey(key)
	s.AppendUintptr(val)
	s.endQuoteFiled()
}
func (s *tidbEncoder) AddReflected(key string, obj interface{}) error {
	s.beginQuoteFiled()
	s.addKey(key)
	err := s.AppendReflected(obj)
	if err == nil {
		s.endQuoteFiled()
	}
	return err
}
func (s *tidbEncoder) OpenNamespace(key string) {
	s.addKey(key)
	s.line.AppendByte('{')
	s.openNamespaces++
}
func (s *tidbEncoder) closeOpenNamespaces() {
	for i := 0; i < s.openNamespaces; i++ {
		s.line.AppendByte('}')
	}
}

/* array encoder part */
func (s *tidbEncoder) addElementSeparator() {
	length := s.line.Len()
	if length == 0 {
		return
	}
	switch s.line.Bytes()[length-1] {
	case '{', '[', ':', ',', ' ', '=':
	default:
		s.line.AppendByte(',')
	}
}
func (s *tidbEncoder) AppendArray(v zapcore.ArrayMarshaler) error {
	s.addElementSeparator()
	s.line.AppendByte('[')
	if err := v.MarshalLogArray(s); err != nil {
		return err
	}
	s.line.AppendByte(']')
	return nil
}
func (s *tidbEncoder) AppendObject(v zapcore.ObjectMarshaler) error {
	s.addElementSeparator()
	s.line.AppendByte('{')
	if err := v.MarshalLogObject(s); err != nil {
		return err
	}
	s.line.WriteByte('}')
	return nil
}
func (s *tidbEncoder) AppendReflected(v interface{}) error {
	s.addElementSeparator()
	bytes, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.safeAddString(strings.TrimSpace(string(bytes)))
	return nil
}
func (s *tidbEncoder) AppendBool(v bool) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendByteString(v []byte) {
	s.addElementSeparator()
	fmt.Fprint(s.line, "0x")
	fmt.Fprint(s.line, hex.EncodeToString(v))
}
func (s *tidbEncoder) AppendComplex128(v complex128) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendComplex64(v complex64) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendDuration(v time.Duration) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendFloat64(v float64) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendFloat32(v float32) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendInt(v int) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendInt64(v int64) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendInt32(v int32) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendInt16(v int16) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendInt8(v int8) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendString(v string) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendTime(v time.Time) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendUint(v uint) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendUint64(v uint64) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendUint32(v uint32) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendUint16(v uint16) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendUint8(v uint8) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
func (s *tidbEncoder) AppendUintptr(v uintptr) {
	s.addElementSeparator()
	fmt.Fprint(s.line, v)
}
