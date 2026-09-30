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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenWithTruncateOnCloseTruncate(t *testing.T) {
	type testCase struct {
		name     string
		content1 string
		content2 string
	}

	testCases := []testCase{
		{"grow", "abc", "abcde"},
		{"shrink", "abc", "ab"},
		{"same", "abc", "def"},
		{"zero", "abc", ""},
		{"from zero", "", "abc"},
	}

	run := func(t *testing.T, fs FileSystem, test testCase) {
		path := "TestOpenWithTruncateOnClose_" + test.name
		writeContent := func(content string) {
			t.Helper()
			f, err := OpenWithTruncateOnClose(fs, path)
			if err != nil {
				t.Fatalf("OpenWithTruncateOnClose: %v", err)
			}
			_, err = f.Write([]byte(content))
			if err != nil {
				f.Close()
				t.Fatalf("Write: %v", err)
			}
			err = f.Close()
			if err != nil {
				t.Fatalf("Close: %v", err)
			}
		}

		writeContent(test.content1)
		writeContent(test.content2)

		got, err := readFile(fs, path)
		if err != nil {
			t.Fatalf("readFile: %v", err)
		}

		if got != test.content2 {
			t.Errorf("expected %q got %q", test.content2, got)
		}
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			t.Run("mock", func(t *testing.T) {
				run(t, MockFs(nil), test)
			})
			t.Run("os", func(t *testing.T) {
				run(t, NewOsFs(os.TempDir()), test)
			})
		})
	}
}

func TestOpenWithContentComparison(t *testing.T) {
	largePrefix := strings.Repeat("same-prefix", 12*1024)
	testCases := []struct {
		name      string
		initial   string
		contents  []string
		unchanged bool
	}{
		{name: "same", initial: "unchanged", contents: []string{"unchanged"}, unchanged: true},
		{name: "large same", initial: largePrefix, contents: []string{largePrefix}, unchanged: true},
		{name: "large suffix change", initial: largePrefix + "old", contents: []string{largePrefix + "new"}},
		{name: "grow", initial: "old", contents: []string{"older"}},
		{name: "shrink", initial: "longer", contents: []string{"short"}},
		{name: "change", initial: "before", contents: []string{"after"}},
		{name: "to empty", initial: "before", contents: []string{""}},
		{name: "from empty", initial: "", contents: []string{"after"}},
		{name: "create empty", contents: []string{""}},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "ninja")
			if test.initial != "" || test.name == "from empty" {
				if err := os.WriteFile(path, []byte(test.initial), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var initialInfo os.FileInfo
			if info, err := os.Stat(path); err == nil {
				initialInfo = info
			}

			writer, err := OpenWithContentComparison(OsFs, path)
			if err != nil {
				t.Fatalf("OpenWithContentComparison: %v", err)
			}
			buffered := bufio.NewWriter(writer)
			for _, contents := range test.contents {
				if _, err := io.WriteString(buffered, contents); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if err := buffered.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if got, want := writer.ContentChanged(), !test.unchanged; got != want {
				t.Errorf("ContentChanged() = %t, want %t", got, want)
			}
			if !writer.ContentChanged() && writer.BytesWritten() != 0 {
				t.Errorf("unchanged output wrote %d bytes", writer.BytesWritten())
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read output: %v", err)
			}
			if want := test.contents[len(test.contents)-1]; string(got) != want {
				t.Fatalf("expected %q, got %q", want, got)
			}
			if test.unchanged {
				time.Sleep(20 * time.Millisecond)
				if info, err := os.Stat(path); err != nil {
					t.Fatal(err)
				} else if !info.ModTime().Equal(initialInfo.ModTime()) {
					t.Errorf("unchanged output modified mtime: was %v, now %v", initialInfo.ModTime(), info.ModTime())
				}
			}
		})
	}
}
