// Copyright 2023 Google Inc. All rights reserved.
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

package proptools

import (
	"cmp"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"math"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"text/scanner"
	"unsafe"

	"github.com/google/blueprint/pool"
)

var hasherPool = pool.New[Hasher]()

type CustomHash interface {
	CustomHash(hasher *Hasher) error
}

const HashSize = 8

type Hash [1]uint64

func (h *Hash) UnmarshalJSON(bytes []byte) error {
	var s []uint64
	err := json.Unmarshal(bytes, &s)
	if err != nil {
		return err
	}
	if len(s) != len(h) {
		return fmt.Errorf("expected %d elements, got %d", len(h), len(s))
	}
	copy(h[:], s)
	return nil
}

func (h *Hash) MarshalJSON() ([]byte, error) {
	return json.Marshal(h[:])
}

var ZeroHash Hash

func (h *Hash) FormatUint(base int) string {
	return strconv.FormatUint(h[0], base)
}

func (h *Hash) PutBigEndian(buf []byte) {
	binary.BigEndian.PutUint64(buf, h[0])
}

func (h *Hash) Bytes() []byte {
	ptr := unsafe.Pointer(unsafe.SliceData(h[:]))
	return unsafe.Slice((*byte)(ptr), len(h)*int(unsafe.Sizeof(h[0])))
}

// CalculateHashReflection calculates the hash of any value, using the CustomHash
// interface if the type implements it, or falling back to reflection if it does not.
func CalculateHashReflection(value any) (Hash, error) {
	if value == nil {
		return ZeroHash, nil
	}
	if ch, ok := value.(CustomHash); ok {
		return CalculateHash(ch)
	}

	hasher := hasherPool.Get()
	defer hasherPool.Put(hasher)
	hasher.reset()

	v := reflect.ValueOf(value)
	err := hasher.CalculateHashReflection(v)
	if err != nil {
		return ZeroHash, err
	}

	return Hash{hasher.Sum64()}, nil
}

// CalculateHash calculates the hash of a value that implements CustomHash.
func CalculateHash[T CustomHash](v T) (Hash, error) {
	hasher := hasherPool.Get()
	defer hasherPool.Put(hasher)
	hasher.reset()

	// We need to distinguish A from *A.
	hasher.HashType(reflect.TypeOf(v))
	err := v.CustomHash(hasher)
	if err != nil {
		return Hash{}, err
	}
	return Hash{hasher.Sum64()}, nil
}

type Hasher struct {
	hash.Hash64
	int64Buf      [8]byte
	ptrs          map[any]uint64
	ptrKeys       []any
	visiting      map[any]bool
	typeHashType  reflect.Type
	typeHash      Hash
	typeHashes    map[reflect.Type]Hash
	mapStateCache *mapState
}

// Preallocate a modest number of pointer entries. Most values use far fewer than
// the largest provider graphs, and the hasher pool retains these maps between calls.
const ptrsMapSize = 256

type mapState struct {
	indexes []int
	keys    []reflect.Value
	values  []reflect.Value
}

func NewHasher() *Hasher {
	hasher := &Hasher{}
	hasher.reset()
	return hasher
}

func HashReference(hasher *Hasher, addr any, hash func(*Hasher) error) error {
	if hasher.ptrs == nil {
		hasher.ptrs = make(map[any]uint64, ptrsMapSize)
	}
	if hasher.visiting == nil {
		hasher.visiting = make(map[any]bool, ptrsMapSize)
	}
	if _, ok := hasher.visiting[addr]; ok {
		// Circular dependency detected (we have this in Scope at least), just return nil for now.
		return nil
	}

	// The special logic below is to avoid hashing the same pointer more than once.
	// We store the current hash value, then reset the hasher to a clean state in
	// order to calculate the hash of the pointer which will be cached for future
	// encounters. Once we have the hash value of the pointer, we hash both the
	// stored hash value and the hash value of the pointer.
	prevHash := hasher.Sum64()
	hasher.Reset()

	ptrHash, ok := hasher.ptrs[addr]
	if !ok {
		hasher.visiting[addr] = true
		err := hash(hasher)
		if err != nil {
			return fmt.Errorf("in pointer: %s", err.Error())
		}
		ptrHash = hasher.Sum64()
		hasher.ptrs[addr] = ptrHash
		hasher.ptrKeys = append(hasher.ptrKeys, addr)
		delete(hasher.visiting, addr)
	}

	hasher.Reset()
	hasher.WriteUint64(prevHash)
	hasher.WriteUint64(ptrHash)
	return nil
}

func (hasher *Hasher) reset() {
	if hasher.Hash64 == nil {
		hasher.Hash64 = fnv.New64()
	} else {
		hasher.Hash64.Reset()
	}

	for _, key := range hasher.ptrKeys {
		delete(hasher.ptrs, key)
	}
	clear(hasher.ptrKeys)
	hasher.ptrKeys = hasher.ptrKeys[:0]
	clear(hasher.visiting)
	hasher.typeHashType = nil
	clear(hasher.typeHashes)
}

func (hasher *Hasher) WriteUint64(i uint64) {
	binary.LittleEndian.PutUint64(hasher.int64Buf[:], i)
	hasher.Write(hasher.int64Buf[:])
}

func (hasher *Hasher) WriteInt(i int) {
	hasher.WriteUint64(uint64(i))
}

func (hasher *Hasher) WriteByte(i byte) {
	hasher.int64Buf[0] = i
	hasher.Write(hasher.int64Buf[:1])
}

func (hasher *Hasher) WriteString(s string) {
	strLen := len(s)
	if strLen == 0 {
		// unsafe.StringData is unspecified in this case
		hasher.WriteByte(0)
		return
	}

	hasher.Write(unsafe.Slice(unsafe.StringData(s), strLen))
}

func (hasher *Hasher) WriteHash(h Hash) {
	for _, e := range h {
		hasher.WriteUint64(e)
	}
}

func (hasher *Hasher) getMapState(size int) *mapState {
	s := hasher.mapStateCache
	// Clear hasher.mapStateCache so that any recursive uses don't collide with this frame.
	hasher.mapStateCache = nil

	if s == nil {
		s = &mapState{}
	}

	// Reset the slices to length `size` and capacity at least `size`
	s.indexes = slices.Grow(s.indexes[:0], size)[0:size]
	s.keys = slices.Grow(s.keys[:0], size)[0:size]
	s.values = slices.Grow(s.values[:0], size)[0:size]

	return s
}

func (hasher *Hasher) putMapState(s *mapState) {
	if hasher.mapStateCache == nil || cap(hasher.mapStateCache.indexes) < cap(s.indexes) {
		hasher.mapStateCache = s
	}
}

func (hasher *Hasher) HashType(t reflect.Type) {
	if t == hasher.typeHashType {
		hasher.WriteHash(hasher.typeHash)
		return
	}
	if h, ok := hasher.typeHashes[t]; ok {
		hasher.WriteHash(h)
		return
	}
	h, err := TypeHash(t)
	if err != nil {
		panic(err)
	}
	if hasher.typeHashType == nil {
		hasher.typeHashType = t
		hasher.typeHash = h
	} else {
		if hasher.typeHashes == nil {
			hasher.typeHashes = make(map[reflect.Type]Hash, 8)
			hasher.typeHashes[hasher.typeHashType] = hasher.typeHash
		}
		hasher.typeHashes[t] = h
	}
	hasher.WriteHash(h)
}

func (hasher *Hasher) CalculateHashReflection(v reflect.Value) error {
	// Include the hash of the type so that hashes of types with the same contents, for example
	// empty slices of different types, produce different hashes.  The hash of each type is cached,
	// so this should be very fast.
	hasher.HashType(v.Type())
	switch v.Kind() {
	case reflect.Struct:
		// The scanner.Position is intentionally excluded from the hash calculation.
		// This field should only be used for printing user-facing error messages,
		// as it is sensitive to formatting changes like comments and whitespace.
		// Including it would cause the hash to change and trigger an unnecessary
		// re-analysis when no actual property has been modified.
		if v.Type() == reflect.TypeOf(scanner.Position{}) {
			return nil
		}
		l := v.NumField()
		hasher.WriteInt(l)
		for i := 0; i < l; i++ {
			err := hasher.CalculateHashReflection(v.Field(i))
			if err != nil {
				return fmt.Errorf("in field %s: %s", v.Type().Field(i).Name, err.Error())
			}
		}
	case reflect.Map:
		l := v.Len()
		hasher.WriteInt(l)
		iter := v.MapRange()
		s := hasher.getMapState(l)
		for i := 0; iter.Next(); i++ {
			s.indexes[i] = i
			s.keys[i] = iter.Key()
			s.values[i] = iter.Value()
		}
		slices.SortFunc(s.indexes, func(i, j int) int {
			return compare_values(s.keys[i], s.keys[j])
		})
		for i := 0; i < l; i++ {
			err := hasher.CalculateHashReflection(s.keys[s.indexes[i]])
			if err != nil {
				return fmt.Errorf("in map: %s", err.Error())
			}
			err = hasher.CalculateHashReflection(s.values[s.indexes[i]])
			if err != nil {
				return fmt.Errorf("in map: %s", err.Error())
			}
		}
		hasher.putMapState(s)
	case reflect.Slice, reflect.Array:
		l := v.Len()
		hasher.WriteInt(l)
		for i := 0; i < l; i++ {
			err := hasher.CalculateHashReflection(v.Index(i))
			if err != nil {
				return fmt.Errorf("in %s at index %d: %s", v.Kind().String(), i, err.Error())
			}
		}
	case reflect.Pointer:
		if v.IsNil() {
			hasher.WriteByte(0)
			return nil
		}
		addr := v.Pointer()
		return HashReference(hasher, addr, func(hasher *Hasher) error {
			return hasher.CalculateHashReflection(v.Elem())
		})
	case reflect.Interface:
		if v.IsNil() {
			hasher.WriteByte(0)
		} else {
			var err error
			// The only way get the pointer out of an interface to hash it or check for cycles
			// would be InterfaceData(), but that's deprecated and seems like it has undefined behavior.
			if ch, ok := isCustomHash(v); ok {
				err = ch.CustomHash(hasher)
			} else {
				err = hasher.CalculateHashReflection(v.Elem())
			}
			if err != nil {
				return fmt.Errorf("in interface: %s", err.Error())
			}
		}
	case reflect.String:
		hasher.WriteString(v.String())
	case reflect.Bool:
		if v.Bool() {
			hasher.WriteByte(1)
		} else {
			hasher.WriteByte(0)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		hasher.WriteUint64(v.Uint())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		hasher.WriteUint64(uint64(v.Int()))
	case reflect.Float32, reflect.Float64:
		hasher.WriteUint64(math.Float64bits(v.Float()))
	default:
		return fmt.Errorf("data may only contain primitives, strings, arrays, slices, structs, maps, and pointers, found: %s", v.Kind().String())
	}
	return nil
}

func isCustomHash(v reflect.Value) (CustomHash, bool) {
	if !v.CanInterface() {
		return nil, false
	}
	ch, ok := v.Interface().(CustomHash)
	return ch, ok
}

type Comparer[T any] interface {
	Compare(other T) int
}

// SortOrdered sorts slices of any type that is in cmp.Ordered
// (int, string, float64, etc.)
func SortOrdered[T cmp.Ordered](s []T) {
	sort.Slice(s, func(i, j int) bool {
		return s[i] < s[j]
	})
}

// SortCustom sorts slices of any type that implements Comparer interface.
func SortCustom[T Comparer[T]](s []T) {
	sort.Slice(s, func(i, j int) bool {
		return s[i].Compare(s[j]) < 0
	})
}

func compare_values(x, y reflect.Value) int {
	if x.Type() != y.Type() {
		panic("Expected equal types")
	}

	switch x.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return cmp.Compare(x.Uint(), y.Uint())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return cmp.Compare(x.Int(), y.Int())
	case reflect.Float32, reflect.Float64:
		return cmp.Compare(x.Float(), y.Float())
	case reflect.String:
		return cmp.Compare(x.String(), y.String())
	case reflect.Bool:
		if x.Bool() == y.Bool() {
			return 0
		} else if x.Bool() {
			return 1
		} else {
			return -1
		}
	case reflect.Pointer:
		return cmp.Compare(x.Pointer(), y.Pointer())
	case reflect.Array:
		l := x.Len()
		for i := 0; i < l; i++ {
			if result := compare_values(x.Index(i), y.Index(i)); result != 0 {
				return result
			}
		}
		return 0
	case reflect.Struct:
		l := x.NumField()
		for i := 0; i < l; i++ {
			if result := compare_values(x.Field(i), y.Field(i)); result != 0 {
				return result
			}
		}
		return 0
	case reflect.Interface:
		if x.IsNil() && y.IsNil() {
			return 0
		} else if x.IsNil() {
			return 1
		} else if y.IsNil() {
			return -1
		}
		return compare_values(x.Elem(), y.Elem())
	default:
		panic(fmt.Sprintf("Could not compare types %s and %s", x.Type().String(), y.Type().String()))
	}
}

func ContainsConfigurable(value interface{}) bool {
	ptrs := make(map[uintptr]bool)
	v := reflect.ValueOf(value)
	if v.IsValid() {
		return containsConfigurableInternal(v, ptrs)
	}
	return false
}

func containsConfigurableInternal(v reflect.Value, ptrs map[uintptr]bool) bool {
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		if IsConfigurable(t) {
			return true
		}
		typeFields := typeFields(t)
		for i := 0; i < v.NumField(); i++ {
			if HasTag(typeFields[i], "blueprint", "allow_configurable_in_provider") {
				continue
			}
			if containsConfigurableInternal(v.Field(i), ptrs) {
				return true
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			key := iter.Key()
			value := iter.Value()
			if containsConfigurableInternal(key, ptrs) {
				return true
			}
			if containsConfigurableInternal(value, ptrs) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		l := v.Len()
		for i := 0; i < l; i++ {
			if containsConfigurableInternal(v.Index(i), ptrs) {
				return true
			}
		}
	case reflect.Pointer:
		if v.IsNil() {
			return false
		}
		addr := v.Pointer()
		if _, ok := ptrs[addr]; ok {
			// pointer cycle
			return false
		}
		ptrs[addr] = true
		if containsConfigurableInternal(v.Elem(), ptrs) {
			return true
		}
	case reflect.Interface:
		if v.IsNil() {
			return false
		} else {
			// The only way get the pointer out of an interface to hash it or check for cycles
			// would be InterfaceData(), but that's deprecated and seems like it has undefined behavior.
			if containsConfigurableInternal(v.Elem(), ptrs) {
				return true
			}
		}
	default:
		return false
	}
	return false
}
