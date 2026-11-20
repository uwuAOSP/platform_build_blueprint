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

package pathtools

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestEncodeDecodeGlobCache(t *testing.T) {
	globs := []GlobResult{
		{
			Pattern:        "*/*.go",
			Deps:           []string{"a", "b"},
			Matches:        []string{"a/a.go", "b/b.go"},
			MeowSourceOnly: true,
		},
		{
			Pattern:  "*/*.c",
			Excludes: []string{"*/a.c", "*/b.c"},
			Deps:     []string{"c", "d"},
			Matches:  []string{"c/c.c", "d/d.c"},
		},
	}

	buf := &bytes.Buffer{}
	w := bufio.NewWriter(buf)
	encoder, err := newGlobFileEncoder(w)
	if err != nil {
		t.Fatalf("failed to create encoder: %s", err)
	}
	err = encoder.encode(globs)
	if err != nil {
		t.Fatalf("failed to encode globs: %s", err)
	}
	err = w.Flush()
	if err != nil {
		t.Fatalf("failed to flush writer: %s", err)
	}

	r := bufio.NewReader(buf)
	decoder, err := newGlobFileDecoder(r)
	if err != nil {
		t.Fatalf("failed to create decoder: %s", err)
	}
	i, errPromise := decoder.iter()
	decodedGlobs := slices.Collect(i)
	if err := errPromise(); err != nil {
		t.Fatalf("failed to decode globs: %s", err)
	}

	if !reflect.DeepEqual(globs, decodedGlobs) {
		t.Errorf("expected globs %#v, got %#v", globs, decodedGlobs)
	}
}

func TestMeowGlobInvalidation(t *testing.T) {
	for _, operation := range []string{"add", "delete"} {
		for _, sourceOnly := range []bool{false, true} {
			name := operation + "/regular"
			if sourceOnly {
				name = operation + "/executor-managed"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				original := filepath.Join(dir, "original.java")
				if err := os.WriteFile(original, []byte("class Original {}"), 0600); err != nil {
					t.Fatal(err)
				}
				cached, err := glob(OsFs, filepath.Join(dir, "*.java"), nil, FollowSymlinks)
				if err != nil {
					t.Fatal(err)
				}
				cached.MeowSourceOnly = sourceOnly
				var cache bytes.Buffer
				if err := WriteGlobFile(&cache, []GlobResult{cached}); err != nil {
					t.Fatal(err)
				}
				if operation == "add" {
					if err := os.WriteFile(filepath.Join(dir, "added.java"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Remove(original); err != nil {
					t.Fatal(err)
				}
				// Force a membership check independently of filesystem timestamp resolution.
				changed, err := CheckForChangedGlobsForMeow(OsFs, bytes.NewReader(cache.Bytes()), 0)
				if sourceOnly {
					if changed || err != nil {
						t.Fatalf("executor-managed glob requested Soong: %v, %v", changed, err)
					}
				} else if !changed {
					t.Fatal("ordinary configuration glob must rerun Soong")
				}
				changed, _ = CheckForChangedGlobs(OsFs, bytes.NewReader(cache.Bytes()), 0)
				if !changed {
					t.Fatal("traditional builds must continue checking all globs")
				}
			})
		}
	}
}

func TestMeowGlobCacheDecodeFailureInvalidates(t *testing.T) {
	var cache bytes.Buffer
	if err := WriteGlobFile(&cache, []GlobResult{{Pattern: "*.java", MeowSourceOnly: true}}); err != nil {
		t.Fatal(err)
	}
	data := cache.Bytes()
	changed, err := CheckForChangedGlobsForMeow(OsFs, bytes.NewReader(data[:len(data)-1]), 0)
	if !changed || err == nil {
		t.Fatalf("truncated cache accepted: changed=%v err=%v", changed, err)
	}
}
