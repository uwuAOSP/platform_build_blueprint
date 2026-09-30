package proptools

import (
	"slices"
	"strings"
	"testing"
	"text/scanner"

	"github.com/google/blueprint/parser"
)

func mustHash(t *testing.T, data interface{}) Hash {
	t.Helper()
	result, err := CalculateHashReflection(data)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHashingMapGetsSameResults(t *testing.T) {
	data := map[string]string{"foo": "bar", "baz": "qux"}
	first := mustHash(t, data)
	second := mustHash(t, data)
	third := mustHash(t, data)
	fourth := mustHash(t, data)
	if first != second || second != third || third != fourth {
		t.Fatal("Did not get the same result every time for a map")
	}
}

func TestHasherResetDropsPointerHashes(t *testing.T) {
	type value struct {
		Text string
	}
	data := &value{Text: "before"}
	hasher := NewHasher()
	hashValue := func(hasher *Hasher) error {
		hasher.WriteString(data.Text)
		return nil
	}
	if err := HashReference(hasher, data, hashValue); err != nil {
		t.Fatal(err)
	}
	first := hasher.Sum64()

	hasher.reset()
	data.Text = "after"
	if err := HashReference(hasher, data, hashValue); err != nil {
		t.Fatal(err)
	}
	if second := hasher.Sum64(); first == second {
		t.Fatalf("hash after reset did not reflect the changed pointed-to value: %x", second)
	}
	if len(hasher.ptrKeys) == 0 {
		t.Fatal("expected pointer tracking to retain the keys until reset")
	}

	hasher.reset()
	if len(hasher.ptrKeys) != 0 {
		t.Fatalf("pointer tracking keys were not cleared: %d remain", len(hasher.ptrKeys))
	}
}

func TestHashingNonSerializableTypesFails(t *testing.T) {
	testCases := []struct {
		name string
		data interface{}
	}{
		{
			name: "function pointer",
			data: []func(){nil},
		},
		{
			name: "channel",
			data: []chan int{make(chan int)},
		},
		{
			name: "list with non-serializable type",
			data: []interface{}{"foo", make(chan int)},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := CalculateHashReflection(testCase)
			if err == nil {
				t.Fatal("Expected hashing error but didn't get one")
			}
			expected := "data may only contain primitives, strings, arrays, slices, structs, maps, and pointers"
			if !strings.Contains(err.Error(), expected) {
				t.Fatalf("Expected %q, got %q", expected, err.Error())
			}
		})
	}
}

var hashTestCases = []struct {
	name string
	data interface{}
}{
	{
		name: "int",
		data: 5,
	},
	{
		name: "string",
		data: "foo",
	},
	{
		name: "empty string",
		data: "",
	},
	{
		name: "*string",
		data: StringPtr("foo"),
	},
	{
		name: "array",
		data: [3]string{"foo", "bar", "baz"},
	},
	{
		name: "slice",
		data: []string{"foo", "bar", "baz"},
	},
	{
		name: "struct",
		data: struct {
			foo string
			bar int
		}{
			foo: "foo",
			bar: 3,
		},
	},
	{
		name: "map",
		data: map[string]int{
			"foo": 3,
			"bar": 4,
		},
	},
	{
		name: "list of interfaces with different types",
		data: []interface{}{"foo", 3, []string{"bar", "baz"}},
	},
	{
		name: "nested maps",
		data: map[string]map[string]map[string]map[string]map[string]int{
			"foo": {"foo": {"foo": {"foo": {"foo": 5}}}},
		},
	},
	{
		name: "multiple maps",
		data: struct {
			foo  map[string]int
			bar  map[string]int
			baz  map[string]int
			qux  map[string]int
			quux map[string]int
		}{
			foo:  map[string]int{"foo": 1, "bar": 2},
			bar:  map[string]int{"bar": 2},
			baz:  map[string]int{"baz": 3, "foo": 1},
			qux:  map[string]int{"qux": 4},
			quux: map[string]int{"quux": 5},
		},
	},
	{
		name: "nested structs",
		data: nestableStruct{
			foo: nestableStruct{
				foo: nestableStruct{
					foo: nestableStruct{
						foo: nestableStruct{
							foo: "foo",
						},
					},
				},
			},
		},
	}, {
		name: "recursive pointer",
		data: func() any {
			type t struct {
				p *t
			}
			v := &t{}
			v.p = v
			return v
		}(),
	},
}

func TestHashSuccessful(t *testing.T) {
	for _, testCase := range hashTestCases {
		t.Run(testCase.name, func(t *testing.T) {
			mustHash(t, testCase.data)
		})
	}
}

func TestHashingDereferencePointers(t *testing.T) {
	str1 := "this is a hash test for pointers"
	str2 := "this is a hash test for pointers"
	data := []struct {
		content *string
	}{
		{content: &str1},
		{content: &str2},
	}
	first := mustHash(t, data[0])
	second := mustHash(t, data[1])
	if first != second {
		t.Fatal("Got different results for the same string")
	}
}

type nestableStruct struct {
	foo interface{}
}

func TestContainsConfigurable(t *testing.T) {
	testCases := []struct {
		name   string
		value  any
		result bool
	}{
		{
			name: "struct without configurable",
			value: struct {
				S string
			}{},
			result: false,
		},
		{
			name: "struct with configurable",
			value: struct {
				S Configurable[string]
			}{},
			result: true,
		},
		{
			name: "struct with allowed configurable",
			value: struct {
				S Configurable[string] `blueprint:"allow_configurable_in_provider"`
			}{},
			result: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got := ContainsConfigurable(testCase.value)
			if got != testCase.result {
				t.Errorf("expected %v, got %v", testCase.value, got)
			}
		})
	}
}

func TestHashingDuplicatePointers(t *testing.T) {
	str1 := "this is a hash test for pointers"
	str2 := "this is a hash test for pointers"
	data1 := struct {
		f1 *string
		f2 *string
	}{
		f1: &str1,
		f2: &str1,
	}
	data2 := struct {
		f1 *string
		f2 *string
	}{
		f1: &str1,
		f2: &str2,
	}
	hash1, err := CalculateHashReflection(data1)
	if err != nil {
		t.Fatal(err)
	}
	hash2, err := CalculateHashReflection(data2)
	if err != nil {
		t.Fatal(err)
	}
	if hash1 != hash2 {
		t.Errorf("hashing pointers to same string vs pointers to identical strings should be equal")
	}

}

func TestHashBytes(t *testing.T) {
	hash := Hash{0x1234567890ABCDEF}
	bytes := hash.Bytes()
	if len(bytes) != HashSize {
		t.Fatalf("Expected %d bytes, got %d", HashSize, len(bytes))
	}

	expected := []byte{0xef, 0xcd, 0xab, 0x90, 0x78, 0x56, 0x34, 0x12}
	if !slices.Equal(bytes, expected) {
		t.Fatalf("Expected %#v, got %#v", expected, bytes)
	}
}

func TestHashOfDifferentTypesIsDifferent(t *testing.T) {
	type t1 struct {
		s string
	}
	type t2 struct {
		s string
	}

	s1 := t1{"foo"}
	s2 := t2{"foo"}

	h1, err := CalculateHashReflection(s1)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := CalculateHashReflection(s2)
	if err != nil {
		t.Fatal(err)
	}

	if h1 == h2 {
		t.Errorf("expected hashes of %#v and %#v to be different, got %v and %v", s1, s2, h1, h2)
	}
}

func TestHashOfDifferentTypesInInterfaceIsDifferent(t *testing.T) {
	type i struct {
		v any
	}
	type t1 struct {
		s string
	}
	type t2 struct {
		s string
	}

	s1 := i{t1{"foo"}}
	s2 := i{t2{"foo"}}

	h1, err := CalculateHashReflection(s1)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := CalculateHashReflection(s2)
	if err != nil {
		t.Fatal(err)
	}

	if h1 == h2 {
		t.Errorf("expected hashes of %#v and %#v to be different, got %v and %v", s1, s2, h1, h2)
	}
}

func BenchmarkCalculateHashReflection(b *testing.B) {
	for _, testCase := range hashTestCases {
		b.Run(testCase.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := CalculateHashReflection(testCase.data)
				if err != nil {
					panic(err)
				}
			}
		})
	}
}

func TestHashCalculationExcludePosition(t *testing.T) {
	instance1 := &parser.String{
		LiteralPos: scanner.Position{
			Line: 10,
		},
		Value: "-Wall",
	}
	instance2 := &parser.String{
		LiteralPos: scanner.Position{
			Line: 20,
		},
		Value: "-Wall",
	}

	hash1, _ := CalculateHashReflection(instance1)
	hash2, _ := CalculateHashReflection(instance2)
	if hash1 != hash2 {
		t.Fatalf("Expect hash values to be equal: %d %d", hash1, hash2)
	}
}
