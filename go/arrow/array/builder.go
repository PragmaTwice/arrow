// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package array

import (
	"fmt"
	"sync/atomic"

	"github.com/apache/arrow/go/v11/arrow"
	"github.com/apache/arrow/go/v11/arrow/bitutil"
	"github.com/apache/arrow/go/v11/arrow/memory"
	"github.com/goccy/go-json"
)

const (
	minBuilderCapacity = 1 << 5
)

// Builder provides an interface to build arrow arrays.
type Builder interface {
	// you can unmarshal a json array to add the values to a builder
	json.Unmarshaler

	// Type returns the datatype that this is building
	Type() arrow.DataType

	// Retain increases the reference count by 1.
	// Retain may be called simultaneously from multiple goroutines.
	Retain()

	// Release decreases the reference count by 1.
	Release()

	// Len returns the number of elements in the array builder.
	Len() int

	// Cap returns the total number of elements that can be stored
	// without allocating additional memory.
	Cap() int

	// NullN returns the number of null values in the array builder.
	NullN() int

	// AppendNull adds a new null value to the array being built.
	AppendNull()

	// AppendEmptyValue adds a new zero value of the appropriate type
	AppendEmptyValue()

	// Reserve ensures there is enough space for appending n elements
	// by checking the capacity and calling Resize if necessary.
	Reserve(n int)

	// Resize adjusts the space allocated by b to n elements. If n is greater than b.Cap(),
	// additional memory will be allocated. If n is smaller, the allocated memory may reduced.
	Resize(n int)

	// NewArray creates a new array from the memory buffers used
	// by the builder and resets the Builder so it can be used to build
	// a new array.
	NewArray() arrow.Array

	UnsafeAppendBoolToBitmap(bool)

	init(capacity int)
	resize(newBits int, init func(int))

	unmarshalOne(*json.Decoder) error
	unmarshal(*json.Decoder) error

	newData() *Data
}

// builder provides common functionality for managing the validity bitmap (nulls) when building arrays.
type builder struct {
	refCount   int64
	mem        memory.Allocator
	nullBitmap *memory.Buffer
	nulls      int
	length     int
	capacity   int
}

// Retain increases the reference count by 1.
// Retain may be called simultaneously from multiple goroutines.
func (b *builder) Retain() {
	atomic.AddInt64(&b.refCount, 1)
}

// Len returns the number of elements in the array builder.
func (b *builder) Len() int { return b.length }

// Cap returns the total number of elements that can be stored without allocating additional memory.
func (b *builder) Cap() int { return b.capacity }

// NullN returns the number of null values in the array builder.
func (b *builder) NullN() int { return b.nulls }

func (b *builder) init(capacity int) {
	toAlloc := bitutil.CeilByte(capacity) / 8
	b.nullBitmap = memory.NewResizableBuffer(b.mem)
	b.nullBitmap.Resize(toAlloc)
	b.capacity = capacity
	memory.Set(b.nullBitmap.Buf(), 0)
}

func (b *builder) reset() {
	if b.nullBitmap != nil {
		b.nullBitmap.Release()
		b.nullBitmap = nil
	}

	b.nulls = 0
	b.length = 0
	b.capacity = 0
}

func (b *builder) resize(newBits int, init func(int)) {
	if b.nullBitmap == nil {
		init(newBits)
		return
	}

	newBytesN := bitutil.CeilByte(newBits) / 8
	oldBytesN := b.nullBitmap.Len()
	b.nullBitmap.Resize(newBytesN)
	b.capacity = newBits
	if oldBytesN < newBytesN {
		// TODO(sgc): necessary?
		memory.Set(b.nullBitmap.Buf()[oldBytesN:], 0)
	}
	if newBits < b.length {
		b.length = newBits
		b.nulls = newBits - bitutil.CountSetBits(b.nullBitmap.Buf(), 0, newBits)
	}
}

func (b *builder) reserve(elements int, resize func(int)) {
	if b.nullBitmap == nil {
		b.nullBitmap = memory.NewResizableBuffer(b.mem)
	}
	if b.length+elements > b.capacity {
		newCap := bitutil.NextPowerOf2(b.length + elements)
		resize(newCap)
	}
}

// unsafeAppendBoolsToBitmap appends the contents of valid to the validity bitmap.
// As an optimization, if the valid slice is empty, the next length bits will be set to valid (not null).
func (b *builder) unsafeAppendBoolsToBitmap(valid []bool, length int) {
	if len(valid) == 0 {
		b.unsafeSetValid(length)
		return
	}

	byteOffset := b.length / 8
	bitOffset := byte(b.length % 8)
	nullBitmap := b.nullBitmap.Bytes()
	bitSet := nullBitmap[byteOffset]

	for _, v := range valid {
		if bitOffset == 8 {
			bitOffset = 0
			nullBitmap[byteOffset] = bitSet
			byteOffset++
			bitSet = nullBitmap[byteOffset]
		}

		if v {
			bitSet |= bitutil.BitMask[bitOffset]
		} else {
			bitSet &= bitutil.FlippedBitMask[bitOffset]
			b.nulls++
		}
		bitOffset++
	}

	if bitOffset != 0 {
		nullBitmap[byteOffset] = bitSet
	}
	b.length += len(valid)
}

// unsafeSetValid sets the next length bits to valid in the validity bitmap.
func (b *builder) unsafeSetValid(length int) {
	padToByte := min(8-(b.length%8), length)
	if padToByte == 8 {
		padToByte = 0
	}
	bits := b.nullBitmap.Bytes()
	for i := b.length; i < b.length+padToByte; i++ {
		bitutil.SetBit(bits, i)
	}

	start := (b.length + padToByte) / 8
	fastLength := (length - padToByte) / 8
	memory.Set(bits[start:start+fastLength], 0xff)

	newLength := b.length + length
	// trailing bytes
	for i := b.length + padToByte + (fastLength * 8); i < newLength; i++ {
		bitutil.SetBit(bits, i)
	}

	b.length = newLength
}

func (b *builder) UnsafeAppendBoolToBitmap(isValid bool) {
	if isValid {
		bitutil.SetBit(b.nullBitmap.Bytes(), b.length)
	} else {
		b.nulls++
	}
	b.length++
}

func NewBuilder(mem memory.Allocator, dtype arrow.DataType) Builder {
	// FIXME(sbinet): use a type switch on dtype instead?
	switch dtype.ID() {
	case arrow.NULL:
		return NewNullBuilder(mem)
	case arrow.BOOL:
		return NewBooleanBuilder(mem)
	case arrow.UINT8:
		return NewUint8Builder(mem)
	case arrow.INT8:
		return NewInt8Builder(mem)
	case arrow.UINT16:
		return NewUint16Builder(mem)
	case arrow.INT16:
		return NewInt16Builder(mem)
	case arrow.UINT32:
		return NewUint32Builder(mem)
	case arrow.INT32:
		return NewInt32Builder(mem)
	case arrow.UINT64:
		return NewUint64Builder(mem)
	case arrow.INT64:
		return NewInt64Builder(mem)
	case arrow.FLOAT16:
		return NewFloat16Builder(mem)
	case arrow.FLOAT32:
		return NewFloat32Builder(mem)
	case arrow.FLOAT64:
		return NewFloat64Builder(mem)
	case arrow.STRING:
		return NewStringBuilder(mem)
	case arrow.LARGE_STRING:
		return NewLargeStringBuilder(mem)
	case arrow.BINARY:
		return NewBinaryBuilder(mem, arrow.BinaryTypes.Binary)
	case arrow.LARGE_BINARY:
		return NewBinaryBuilder(mem, arrow.BinaryTypes.LargeBinary)
	case arrow.FIXED_SIZE_BINARY:
		typ := dtype.(*arrow.FixedSizeBinaryType)
		return NewFixedSizeBinaryBuilder(mem, typ)
	case arrow.DATE32:
		return NewDate32Builder(mem)
	case arrow.DATE64:
		return NewDate64Builder(mem)
	case arrow.TIMESTAMP:
		typ := dtype.(*arrow.TimestampType)
		return NewTimestampBuilder(mem, typ)
	case arrow.TIME32:
		typ := dtype.(*arrow.Time32Type)
		return NewTime32Builder(mem, typ)
	case arrow.TIME64:
		typ := dtype.(*arrow.Time64Type)
		return NewTime64Builder(mem, typ)
	case arrow.INTERVAL_MONTHS:
		return NewMonthIntervalBuilder(mem)
	case arrow.INTERVAL_DAY_TIME:
		return NewDayTimeIntervalBuilder(mem)
	case arrow.INTERVAL_MONTH_DAY_NANO:
		return NewMonthDayNanoIntervalBuilder(mem)
	case arrow.DECIMAL128:
		if typ, ok := dtype.(*arrow.Decimal128Type); ok {
			return NewDecimal128Builder(mem, typ)
		}
	case arrow.DECIMAL256:
		if typ, ok := dtype.(*arrow.Decimal256Type); ok {
			return NewDecimal256Builder(mem, typ)
		}
	case arrow.LIST:
		typ := dtype.(*arrow.ListType)
		return NewListBuilderWithField(mem, typ.ElemField())
	case arrow.STRUCT:
		typ := dtype.(*arrow.StructType)
		return NewStructBuilder(mem, typ)
	case arrow.SPARSE_UNION:
		typ := dtype.(*arrow.SparseUnionType)
		return NewSparseUnionBuilder(mem, typ)
	case arrow.DENSE_UNION:
		typ := dtype.(*arrow.DenseUnionType)
		return NewDenseUnionBuilder(mem, typ)
	case arrow.DICTIONARY:
		typ := dtype.(*arrow.DictionaryType)
		return NewDictionaryBuilder(mem, typ)
	case arrow.LARGE_LIST:
		typ := dtype.(*arrow.LargeListType)
		return NewLargeListBuilderWithField(mem, typ.ElemField())
	case arrow.MAP:
		typ := dtype.(*arrow.MapType)
		return NewMapBuilder(mem, typ.KeyType(), typ.ItemType(), typ.KeysSorted)
	case arrow.EXTENSION:
		typ := dtype.(arrow.ExtensionType)
		return NewExtensionBuilder(mem, typ)
	case arrow.FIXED_SIZE_LIST:
		typ := dtype.(*arrow.FixedSizeListType)
		return NewFixedSizeListBuilder(mem, typ.Len(), typ.Elem())
	case arrow.DURATION:
		typ := dtype.(*arrow.DurationType)
		return NewDurationBuilder(mem, typ)
	case arrow.RUN_END_ENCODED:
		typ := dtype.(*arrow.RunEndEncodedType)
		return NewRunEndEncodedBuilder(mem, typ.RunEnds(), typ.Encoded())
	}
	panic(fmt.Errorf("arrow/array: unsupported builder for %T", dtype))
}
