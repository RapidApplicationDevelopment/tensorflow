// Copyright 2016 The TensorFlow Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tensorflow

// #include <string.h>
// #include "tensorflow/c/c_api.h"
import "C"

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"unsafe"
)

// DataType holds the type for a scalar value.  E.g., one slot in a tensor.
type DataType C.TF_DataType

// Types of scalar values in the TensorFlow type system.
const (
	Float      DataType = C.TF_FLOAT
	Double     DataType = C.TF_DOUBLE
	Int32      DataType = C.TF_INT32
	Uint8      DataType = C.TF_UINT8
	Int16      DataType = C.TF_INT16
	Int8       DataType = C.TF_INT8
	String     DataType = C.TF_STRING
	Complex64  DataType = C.TF_COMPLEX64
	Complex    DataType = C.TF_COMPLEX
	Int64      DataType = C.TF_INT64
	Bool       DataType = C.TF_BOOL
	Qint8      DataType = C.TF_QINT8
	Quint8     DataType = C.TF_QUINT8
	Qint32     DataType = C.TF_QINT32
	Bfloat16   DataType = C.TF_BFLOAT16
	Qint16     DataType = C.TF_QINT16
	Quint16    DataType = C.TF_QUINT16
	Uint16     DataType = C.TF_UINT16
	Complex128 DataType = C.TF_COMPLEX128
	Half       DataType = C.TF_HALF
)

// Tensor holds a multi-dimensional array of elements of a single data type.
type Tensor struct {
	c     *C.TF_Tensor
	shape []int64
}

// NewTensor converts from a Go value to a Tensor. Valid values are scalars,
// slices, and arrays. Every element of a slice must have the same length so
// that the resulting Tensor has a valid shape.
func NewTensor(value interface{}) (*Tensor, error) {
	val := reflect.ValueOf(value)
	dims, dataType, err := dimsAndDataTypeOf(val.Type())
	if err != nil {
		return nil, err
	}
	// TODO(ashankar): Remove the bytes.Buffer and endcode directly into
	// C-memory, avoiding the memcpy and cutting down memory usage in half.
	shape := make([]int64, dims)
	buf := new(bytes.Buffer)
	if err := encodeTensor(buf, shape, val); err != nil {
		return nil, err
	}
	var shapePtr *C.int64_t
	if len(shape) > 0 {
		shapePtr = (*C.int64_t)(unsafe.Pointer(&shape[0]))
	}
	t := &Tensor{
		c:     C.TF_AllocateTensor(C.TF_DataType(dataType), shapePtr, C.int(len(shape)), C.size_t(buf.Len())),
		shape: shape,
	}
	runtime.SetFinalizer(t, (*Tensor).finalize)
	if buf.Len() > 0 {
		slice := buf.Bytes() // https://github.com/golang/go/issues/14210
		C.memcpy(C.TF_TensorData(t.c), unsafe.Pointer(&slice[0]), C.size_t(buf.Len()))
	}
	return t, nil
}

// newTensorFromC takes ownership of c and returns the owning Tensor.
func newTensorFromC(c *C.TF_Tensor) *Tensor {
	var shape []int64
	if ndims := int(C.TF_NumDims(c)); ndims > 0 {
		shape = make([]int64, ndims)
	}
	for i := range shape {
		shape[i] = int64(C.TF_Dim(c, C.int(i)))
	}
	t := &Tensor{c: c, shape: shape}
	runtime.SetFinalizer(t, (*Tensor).finalize)
	return t
}

func (t *Tensor) finalize() { C.TF_DeleteTensor(t.c) }

// DataType returns the scalar datatype of the Tensor.
func (t *Tensor) DataType() DataType { return DataType(C.TF_TensorType(t.c)) }

// Shape returns the shape of the Tensor.
func (t *Tensor) Shape() []int64 { return t.shape }

// Value converts the Tensor to a Go value. For now, not all Tensor types are
// supported, and this function may panic if it encounters an unsupported
// DataType.
//
// The type of the output depends on the Tensor type and dimensions.
// For example:
// Tensor(int64, 0): int64
// Tensor(float64, 3): [][][]float64
func (t *Tensor) Value() interface{} {
	typ, err := typeOf(t.DataType(), t.Shape())
	if err != nil {
		panic(err)
	}
	val := reflect.New(typ)
	// See: https://github.com/golang/go/wiki/cgo#turning-c-arrays-into-go-slices
	cbytes := C.TF_TensorData(t.c)
	length := int(C.TF_TensorByteSize(t.c))
	slice := (*[1 << 30]byte)(unsafe.Pointer(cbytes))[:length:length]
	if err := decodeTensor(bytes.NewReader(slice), t.Shape(), typ, val); err != nil {
		panic(err)
	}
	return reflect.Indirect(val).Interface()
}

var types = []struct {
	typ      reflect.Type
	dataType C.TF_DataType
}{
	{reflect.TypeOf(float32(0)), C.TF_FLOAT},
	{reflect.TypeOf(float64(0)), C.TF_DOUBLE},
	{reflect.TypeOf(int32(0)), C.TF_INT32},
	{reflect.TypeOf(uint8(0)), C.TF_UINT8},
	{reflect.TypeOf(int16(0)), C.TF_INT16},
	{reflect.TypeOf(int8(0)), C.TF_INT8},
	{reflect.TypeOf(""), C.TF_STRING},
	{reflect.TypeOf(complex(float32(0), float32(0))), C.TF_COMPLEX64},
	{reflect.TypeOf(int64(0)), C.TF_INT64},
	{reflect.TypeOf(false), C.TF_BOOL},
	{reflect.TypeOf(uint16(0)), C.TF_UINT16},
	{reflect.TypeOf(complex(float64(0), float64(0))), C.TF_COMPLEX128},
	// TODO(apassos): support DT_RESOURCE representation in go.
}

// dimsAndDataTypeOf returns the data type and dimensions of a Go type for use
// when encoding. We fetch them separately from encoding to support 0-D tensors.
func dimsAndDataTypeOf(typ reflect.Type) (int, DataType, error) {
	dims := 0
	elem := typ
	for ; elem.Kind() == reflect.Array || elem.Kind() == reflect.Slice; elem = elem.Elem() {
		dims++
	}
	for _, t := range types {
		if elem.Kind() == t.typ.Kind() {
			return dims, DataType(t.dataType), nil
		}
	}
	return 0, DataType(0), fmt.Errorf("unsupported type %v", typ)
}

// typeOf converts from a DataType and Shape to the equivalent Go type.
func typeOf(dt DataType, shape []int64) (reflect.Type, error) {
	var ret reflect.Type
	for _, t := range types {
		if dt == DataType(t.dataType) {
			ret = t.typ
			break
		}
	}
	if ret == nil {
		return nil, fmt.Errorf("DataType %v unsupported", dt)
	}
	for _ = range shape {
		ret = reflect.SliceOf(ret)
	}
	return ret, nil
}

// encodeTensor writes v to the specified buffer using the format specified in
// c_api.h
func encodeTensor(w io.Writer, shape []int64, v reflect.Value) error {
	switch v.Kind() {
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint8, reflect.Uint16, reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		if err := binary.Write(w, nativeEndian, v.Interface()); err != nil {
			return err
		}

	case reflect.Array, reflect.Slice:
		// If slice elements are slices, verify that all of them have the same size.
		// Go's type system makes that guarantee for arrays.
		if v.Len() > 0 && v.Type().Elem().Kind() == reflect.Slice {
			expected := v.Index(0).Len()
			for i := 1; i < v.Len(); i++ {
				if v.Index(i).Len() != expected {
					return fmt.Errorf("mismatched slice lengths: %d and %d", v.Index(i).Len(), expected)
				}
			}
		}

		shape[0] = int64(v.Len())
		for i := 0; i < v.Len(); i++ {
			err := encodeTensor(w, shape[1:], v.Index(i))
			if err != nil {
				return err
			}
		}

	default:
		return fmt.Errorf("unsupported type %v", v.Type())
	}
	return nil
}

// decodeTensor decodes the Tensor from the buffer to ptr using the format
// specified in c_api.h
func decodeTensor(r io.Reader, shape []int64, typ reflect.Type, ptr reflect.Value) error {
	switch typ.Kind() {
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint8, reflect.Uint16, reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		if err := binary.Read(r, nativeEndian, ptr.Interface()); err != nil {
			return err
		}

	case reflect.Slice:
		val := reflect.Indirect(ptr)
		val.Set(reflect.MakeSlice(typ, int(shape[0]), int(shape[0])))
		for i := 0; i < val.Len(); i++ {
			if err := decodeTensor(r, shape[1:], typ.Elem(), val.Index(i).Addr()); err != nil {
				return err
			}
		}

	default:
		return fmt.Errorf("unsupported type %v", typ)
	}
	return nil
}

// nativeEndian is the byte order for the local platform. Used to send back and
// forth Tensors with the C API. We test for endianness at runtime because
// some architectures can be booted into different endian modes.
var nativeEndian binary.ByteOrder

func init() {
	buf := [2]byte{}
	*(*uint16)(unsafe.Pointer(&buf[0])) = uint16(0xABCD)

	switch buf {
	case [2]byte{0xCD, 0xAB}:
		nativeEndian = binary.LittleEndian
	case [2]byte{0xAB, 0xCD}:
		nativeEndian = binary.BigEndian
	default:
		panic("Could not determine native endianness.")
	}
}
