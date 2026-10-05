// Copyright 2025 Google Inc. All rights reserved.
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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unique"

	"github.com/google/blueprint/depset"
	"github.com/google/blueprint/gobtools"
	"github.com/google/blueprint/gobtools/test"
	"github.com/google/blueprint/proptools"
	"github.com/google/blueprint/uniquelist"
)

func TestEncDec(t *testing.T) {
	strValue := "string value for test"
	defaultEcho := TestEcho{"111111111"}
	newEcho := TestEcho{"111111111"}
	transString := depset.New(depset.PREORDER, []string{"111111111"}, nil)
	depsetString := depset.New(depset.PREORDER, []string{"222222222"}, []depset.DepSet[string]{transString})
	transTestEcho := depset.New(depset.PREORDER, []TestEcho{defaultEcho}, nil)
	depsetTestEcho := depset.New(depset.PREORDER, []TestEcho{defaultEcho}, []depset.DepSet[TestEcho]{transTestEcho})
	transTestEchoInterface := depset.New(depset.PREORDER, []TestEchoInterface{defaultEcho}, nil)
	depsetTestEchoInterface := depset.New(depset.PREORDER, []TestEchoInterface{defaultEcho}, []depset.DepSet[TestEchoInterface]{transTestEchoInterface})
	testCases := []struct {
		name    string
		origin  gobtools.CustomEnc
		decoded gobtools.CustomDec
	}{
		{
			name:    "TestStruct with default values",
			origin:  &TestStruct{},
			decoded: &TestStruct{},
		},
		{
			name: "TestStruct with value interface",
			origin: &TestStruct{
				f18: TestEcho{"aaaa"},
				f21: uniquelist.Make([]TestEchoInterface{defaultEcho}),
				f24: test.TypeStruct{Name: "fffffffff"},
			},
			decoded: &TestStruct{},
		},
		{
			name: "duplicate pointer",
			origin: &TestPtrs{
				f1: &defaultEcho,
				f2: &defaultEcho,
			},
			decoded: &TestPtrs{},
		},
		{
			name: "TestStruct",
			origin: &TestStruct{
				TestEcho: TestEcho{"222222222"},
				f1:       "333333333",
				f2:       111,
				f3:       222,
				f4:       true,
				f5:       333,
				f6:       "444444444",
				f7:       TestEcho{"444444444"},
				f8:       444,
				f9:       555,
				f10:      666,
				f11:      []string{"a", "bb", "ccc"},
				f12: map[string]int{
					"f12a": 1,
					"f12b": 2,
					"f12c": 3,
				},
				f13: &strValue,
				f14: 777,
				f15: []int{1, 2, 3},
				f16: []TestEcho{
					{"555555555"},
					{"666666666"},
				},
				f17: &defaultEcho,
				f18: &TestEcho{"aaaa"},
				f19: testStrings{"bbbb", "cccc", "dddd"},
				f20: uniquelist.Make([]TestEcho{defaultEcho}),
				// Use newEcho for now before we generate hash code for uniquelist, otherwise
				// the hash for the decoded will be different from the origin.
				f21: uniquelist.Make([]TestEchoInterface{&newEcho}),
				f22: test.TypeStruct{Name: "aaaaaaaaa"},
				f23: []test.TypeAlias{
					{
						{Name: "bbbbbbbbb"},
						{Name: "ccccccccc"},
					},
					{
						{Name: "ddddddddd"},
						{Name: "eeeeeeeee"},
					},
				},
				f24: &test.TypeStruct{Name: "fffffffff"},
				f25: test.TypeIdent{Name: "ggggggggg"},
				f26: depsetTestEcho,
				f27: depsetTestEchoInterface,
				f28: map[int][]string{
					1: {"aaaaaaaaa", "bbbbbbbbb"},
					2: {"ccccccccc", "ddddddddd"},
				},
				f29: [][]string{
					{"aaaaaaaaa", "bbbbbbbbb"},
					{"ccccccccc", "ddddddddd"},
				},
				f30:          depsetString,
				f31:          &defaultEcho,
				f32:          []*test.TypeStruct{{Name: "hhhhhhhh"}},
				f33:          "iiiiiiii",
				f34:          "jjjjjjjj",
				f35:          struct{ s string }{s: "kkkkkkkkk"},
				f36:          TestGeneric[int32]{t: 12345},
				TestEmbedPtr: &TestEmbedPtr{f37: "mmmmmmmm"},
				f38:          test.TypeBasic(1),
				f39:          [2]uint64{1, 2},
				f40: map[string]map[int][]*bool{
					"llllllll": {
						1: {boolPtr(true)},
					},
				},
				f41: unique.Make(TestEcho{"aaaaaa"}),
				f42: unique.Make(TestEcho{"aaaaaa"}),
			},
			decoded: &TestStruct{},
		},
		{
			name:    "testEchos",
			origin:  &testEchos{defaultEcho},
			decoded: &testEchos{},
		},
		{
			name: "testStringMap",
			origin: &testStringMap{
				"111111111": []string{"222222222", "333333333"},
				"222222222": []string{"444444444", "555555555"},
			},
			decoded: &testStringMap{},
		},
		{
			name: "testEchoMap",
			origin: &testEchoMap{
				defaultEcho: &TestEcho{"aaaaaaaaa"},
			},
			decoded: &testEchoMap{},
		},
		{
			name: "TestStruct with empty slice and map",
			origin: &TestStruct{
				f12: map[string]int{},
				f15: []int{},
			},
			decoded: &TestStruct{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			buf := new(bytes.Buffer)
			ctx := gobtools.NewReferencesEncoderForTest()
			if err := tc.origin.Encode(ctx, buf); err != nil {
				t.Errorf("failed to encode %s: %v", tc.name, err)
			}
			if err := ctx.EncodeReferences(); err != nil {
				t.Errorf("failed to encode references: %v", err)
			}
			if err := tc.decoded.Decode(ctx, bytes.NewReader(buf.Bytes())); err != nil {
				t.Errorf("failed to decode %s: %v", tc.name, err)
			}
			if !reflect.DeepEqual(tc.origin, tc.decoded) {
				t.Errorf("the decoded data is different from the origin: expected:\n  %#v\n got:\n  %#v", tc.origin, tc.decoded)
			}

			originalHash, err := proptools.CalculateHash(tc.origin.(proptools.CustomHash))
			if err != nil {
				t.Fatal(err)
			}
			decodedHash, err := proptools.CalculateHash(tc.decoded.(proptools.CustomHash))
			if err != nil {
				t.Fatal(err)
			}
			if originalHash != decodedHash {
				t.Errorf("the decoded data has a different hash from the origin: expected: %v got %v", originalHash, decodedHash)
			}
		})
	}
}

func TestGenerate(t *testing.T) {
	g := newGobGen()
	curDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get current directory: %v", err)
	}

	parts := strings.Split(curDir, blueprintPkgPath)
	if len(parts) < 2 {
		t.Fatalf("could not determine source root from path %q, which does not contain %q",
			curDir, blueprintPkgPath)
	}
	g.sourceDir = parts[0]

	sourceFile := "gob_test_data.go"
	expectedOutputFile := "main_enc.go"

	generatedBytes, outputFile, err := g.generate(sourceFile, nil, false)
	if err != nil {
		t.Fatalf("g.generate() failed for %s: %v", sourceFile, err)
	}

	if outputFile != expectedOutputFile {
		t.Fatalf("output file is different from the expected: %s %s", outputFile, expectedOutputFile)
	}

	expectedBytes, err := os.ReadFile(expectedOutputFile)
	if err != nil {
		t.Fatalf("failed to read expected output file %s: %v", expectedOutputFile, err)
	}

	if !bytes.Equal(generatedBytes, expectedBytes) {
		t.Errorf("Generated code from %s does not match expected output in %s.\nexpected:\n%s\ngot:\n%s",
			sourceFile, expectedOutputFile, expectedBytes, generatedBytes)
	}
}

func TestGenerateMultipleSourcesPreservesEarlierOutput(t *testing.T) {
	dir := t.TempDir()
	firstSource := filepath.Join(dir, "first.go")
	secondSource := filepath.Join(dir, "second.go")

	for source, content := range map[string]string{
		firstSource: "package testpkg\n\n// @auto-generate: gob\ntype First struct { Value string }\n",
		secondSource: "package testpkg\n\n// @auto-generate: gob\ntype Second struct { Value int }\n",
	} {
		if err := os.WriteFile(source, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	g := newGobGen()
	g.sourceDir = dir

	generated, outputFile, err := g.generate(firstSource, nil, false)
	if err != nil {
		t.Fatalf("generating %s: %v", firstSource, err)
	}
	generated, outputFile, err = g.generate(secondSource, generated, false)
	if err != nil {
		t.Fatalf("generating %s: %v", secondSource, err)
	}

	if outputFile != filepath.Join(dir, "testpkg_enc.go") {
		t.Fatalf("unexpected output file %q", outputFile)
	}
	for _, marker := range []string{"// begin of first.go", "// begin of second.go"} {
		if !strings.Contains(string(generated), marker) {
			t.Errorf("generated output does not contain %q", marker)
		}
	}
}

func TestCustomHash(t *testing.T) {
	// Helper variables for pointer/value tests
	intVal1 := 10
	intVal2 := 10 // Same value, different address
	intVal3 := 20
	strVal1 := "hello"
	strVal2 := "hello" // Same value, different address
	zero := 0
	ptrStr1 := "this is a hash test for pointers"
	ptrStr2 := "this is a hash test for pointers"

	testCases := []struct {
		name          string
		data1         proptools.CustomHash
		data2         proptools.CustomHash
		shouldBeEqual bool // true = hashes must match, false = hashes must NOT match
	}{
		// --- Primitives (HashStruct) ---
		{
			name:          "Primitives: Identical default values",
			data1:         HashStruct{},
			data2:         HashStruct{},
			shouldBeEqual: true,
		},
		{
			name:          "Primitives: Identical non-default values",
			data1:         HashStruct{I: 1, S: "a", B: true, F64: 123},
			data2:         HashStruct{I: 1, S: "a", B: true, F64: 123},
			shouldBeEqual: true,
		},
		{
			name:          "Primitives: Different int",
			data1:         HashStruct{I: 1},
			data2:         HashStruct{I: 2},
			shouldBeEqual: false,
		},
		{
			name:          "Primitives: Different string",
			data1:         HashStruct{S: "a"},
			data2:         HashStruct{S: "b"},
			shouldBeEqual: false,
		},
		{
			name:          "Primitives: Different bool",
			data1:         HashStruct{B: true},
			data2:         HashStruct{B: false},
			shouldBeEqual: false,
		},

		// --- Pointers ---
		{
			name:          "Pointers: Identical nil",
			data1:         Pointers{A: nil, S: nil},
			data2:         Pointers{A: nil, S: nil},
			shouldBeEqual: true,
		},
		{
			name:          "Pointers: Different pointers, same value (CRITICAL TEST)",
			data1:         Pointers{A: &intVal1},
			data2:         Pointers{A: &intVal2},
			shouldBeEqual: true, // Should hash the value, not the pointer address
		},
		{
			name:          "Pointers: Nil vs. Non-nil",
			data1:         Pointers{A: nil},
			data2:         Pointers{A: &intVal1},
			shouldBeEqual: false,
		},
		{
			name:          "Pointers: Different values",
			data1:         Pointers{A: &intVal1},
			data2:         Pointers{A: &intVal3},
			shouldBeEqual: false,
		},
		{
			name:          "Pointers: Nil vs. Zero-value pointer",
			data1:         Pointers{A: nil},
			data2:         Pointers{A: &zero},
			shouldBeEqual: false,
		},

		// --- Slices ---
		{
			name:          "Slices: Identical nil",
			data1:         Slices{A: nil},
			data2:         Slices{A: nil},
			shouldBeEqual: true,
		},
		{
			name:          "Slices: Identical empty",
			data1:         Slices{A: []int{}},
			data2:         Slices{A: []int{}},
			shouldBeEqual: true,
		},
		{
			name:          "Slices: Nil vs. Empty (Semantic equality)",
			data1:         Slices{A: nil},
			data2:         Slices{A: []int{}},
			shouldBeEqual: true, // A robust hash should treat nil and empty slices as equal
		},
		{
			name:          "Slices: Different instances, same content",
			data1:         Slices{A: []int{1, 2, 3}},
			data2:         Slices{A: []int{1, 2, 3}},
			shouldBeEqual: true,
		},
		{
			name:          "Slices: Different order",
			data1:         Slices{A: []int{1, 2, 3}},
			data2:         Slices{A: []int{3, 2, 1}},
			shouldBeEqual: false,
		},
		{
			name:          "Slices: Different length",
			data1:         Slices{A: []int{1, 2, 3}},
			data2:         Slices{A: []int{1, 2}},
			shouldBeEqual: false,
		},

		// --- Maps (CRITICAL: requires key sorting) ---
		{
			name:          "Maps: Identical nil",
			data1:         Maps{M: nil},
			data2:         Maps{M: nil},
			shouldBeEqual: true,
		},
		{
			name:          "Maps: Nil vs. Empty (Semantic equality)",
			data1:         Maps{M: nil},
			data2:         Maps{M: map[string]int{}},
			shouldBeEqual: true, // Robust hash should treat nil and empty maps as equal
		},
		{
			name:          "Maps: Different instances, same content (Tests key sorting)",
			data1:         Maps{M: map[string]int{"a": 1, "b": 2}},
			data2:         Maps{M: map[string]int{"b": 2, "a": 1}},
			shouldBeEqual: true,
		},
		{
			name:          "Maps: Different value",
			data1:         Maps{M: map[string]int{"a": 1}},
			data2:         Maps{M: map[string]int{"a": 2}},
			shouldBeEqual: false,
		},
		{
			name:          "Maps: Different key",
			data1:         Maps{M: map[string]int{"a": 1}},
			data2:         Maps{M: map[string]int{"b": 1}},
			shouldBeEqual: false,
		},

		// --- Embedded Structs ---
		{
			name:          "Embedded: Identical",
			data1:         Embedded{Inner: Inner{Val: "hi"}, F: 1.0},
			data2:         Embedded{Inner: Inner{Val: "hi"}, F: 1.0},
			shouldBeEqual: true,
		},
		{
			name:          "Embedded: Different embedded value",
			data1:         Embedded{Inner: Inner{Val: "hi"}, F: 1.0},
			data2:         Embedded{Inner: Inner{Val: "bye"}, F: 1.0},
			shouldBeEqual: false,
		},
		{
			name:          "Embedded: Different outer field",
			data1:         Embedded{Inner: Inner{Val: "hi"}, F: 1.0},
			data2:         Embedded{Inner: Inner{Val: "hi"}, F: 2.0},
			shouldBeEqual: false,
		},

		// --- Nested Structs ---
		{
			name:          "Nested: Identical",
			data1:         Nested{A: Inner{Val: "a"}, B: &Inner{Val: "b"}},
			data2:         Nested{A: Inner{Val: "a"}, B: &Inner{Val: "b"}},
			shouldBeEqual: true, // This may fail if you don't hash pointer values
		},
		{
			name:          "Nested: Different pointer values, same content",
			data1:         Nested{B: &Inner{Val: strVal1}},
			data2:         Nested{B: &Inner{Val: strVal2}},
			shouldBeEqual: true,
		},
		{
			name:          "Nested: Different nested value",
			data1:         Nested{A: Inner{Val: "a"}},
			data2:         Nested{A: Inner{Val: "b"}},
			shouldBeEqual: false,
		},

		// --- Interfaces ---
		{
			name:          "Interface: Identical nil",
			data1:         InterfaceHolder{I: nil},
			data2:         InterfaceHolder{I: nil},
			shouldBeEqual: true,
		},
		{
			name:          "Interface: Nil vs. Non-nil",
			data1:         InterfaceHolder{I: nil},
			data2:         InterfaceHolder{I: InterfaceImpl1{"a"}},
			shouldBeEqual: false,
		},
		{
			name:          "Interface: Identical concrete value",
			data1:         InterfaceHolder{I: InterfaceImpl1{"a"}},
			data2:         InterfaceHolder{I: InterfaceImpl1{"a"}},
			shouldBeEqual: true,
		},
		{
			name:          "Interface: Different concrete value, same type",
			data1:         InterfaceHolder{I: InterfaceImpl1{"a"}},
			data2:         InterfaceHolder{I: InterfaceImpl1{"b"}},
			shouldBeEqual: false,
		},
		{
			name:          "Interface: Different concrete type, same value (CRITICAL TEST)",
			data1:         InterfaceHolder{I: InterfaceImpl1{"a"}},
			data2:         InterfaceHolder{I: InterfaceImpl2{"a"}},
			shouldBeEqual: false, // Hash must include the concrete type's info
		},

		// --- Unexported Fields (May require unsafe) ---
		{
			name:          "Unexported: Identical",
			data1:         Unexported{Exported: "a", unexported: "b"},
			data2:         Unexported{Exported: "a", unexported: "b"},
			shouldBeEqual: true,
		},
		{
			name:          "Unexported: Different exported",
			data1:         Unexported{Exported: "a", unexported: "b"},
			data2:         Unexported{Exported: "X", unexported: "b"},
			shouldBeEqual: false,
		},
		{
			name:          "Unexported: Different unexported (Tests unsafe access)",
			data1:         Unexported{Exported: "a", unexported: "b"},
			data2:         Unexported{Exported: "a", unexported: "X"},
			shouldBeEqual: false, // This will FAIL if you don't hash unexported fields
		},
		{
			name: "Pointers: Pointers to same string vs pointers to identical strings",
			data1: PointerDuplication{
				F1: &ptrStr1,
				F2: &ptrStr1, // Both F1 and F2 point to the *same* string
			},
			data2: PointerDuplication{
				F1: &ptrStr1,
				F2: &ptrStr2, // F1 and F2 point to *different* strings with identical content
			},
			shouldBeEqual: true, // Hashes should be equal (hashing the value, not the address)
		},
		{
			name:          "Types: Different struct types, same fields and content",
			data1:         Type1{S: "foo"},
			data2:         Type2{S: "foo"},
			shouldBeEqual: false, // Hashes should be different (hash includes type info)
		},
		{
			name: "Interfaces: Different concrete types in interface, same content",
			data1: InterfaceWrapper{
				V: Type1{S: "foo"},
			},
			data2: InterfaceWrapper{
				V: Type2{S: "foo"},
			},
			shouldBeEqual: false, // Hashes should be different (hash includes concrete type info)
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Hash data1
			hasher1 := proptools.NewHasher()
			err1 := tc.data1.CustomHash(hasher1)
			if err1 != nil {
				t.Fatalf("data1 hash failed: %v", err1)
			}
			hash1 := proptools.Hash{hasher1.Sum64()}

			// Hash data2
			hasher2 := proptools.NewHasher()
			err2 := tc.data2.CustomHash(hasher2)
			if err2 != nil {
				t.Fatalf("data2 hash failed: %v", err2)
			}
			hash2 := proptools.Hash{hasher2.Sum64()}

			// Compare results against our expectation
			if (hash1 == hash2) != tc.shouldBeEqual {
				if tc.shouldBeEqual {
					t.Errorf("hashes should be EQUAL but were different.\nhash1: %v\nhash2: %v\ndata1: %#v\ndata2: %#v", hash1, hash2, tc.data1, tc.data2)
				} else {
					t.Errorf("hashes should be DIFFERENT but were equal.\nhash: %v\ndata1: %#v\ndata2: %#v", hash1, tc.data1, tc.data2)
				}
			}
		})
	}
}

func boolPtr(b bool) *bool {
	return &b
}
