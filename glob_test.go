// Copyright 2018 Google Inc. All rights reserved.
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

package blueprint

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/blueprint/pathtools"
)

func TestGlobCache(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": nil,
		"a/a":        nil,
		"a/b":        nil,
	})

	// Test a simple glob with no excludes
	matches, err := ctx.glob("a/*", nil)
	if err != nil {
		t.Error("unexpected error", err)
	}
	if len(matches) != 2 || matches[0] != "a/a" || matches[1] != "a/b" {
		t.Error(`expected ["a/a", "a/b"], got`, matches)
	}

	// Test the same glob with an empty excludes array to make sure
	// excludes=nil does not conflict with excludes=[]string{} in the
	// cache.
	matches, err = ctx.glob("a/*", []string{})
	if err != nil {
		t.Error("unexpected error", err)
	}
	if len(matches) != 2 || matches[0] != "a/a" || matches[1] != "a/b" {
		t.Error(`expected ["a/a", "a/b"], got`, matches)
	}

	// Test the same glob with a different excludes array
	matches, err = ctx.glob("a/*", []string{"a/b"})
	if err != nil {
		t.Error("unexpected error", err)
	}
	if len(matches) != 1 || matches[0] != "a/a" {
		t.Error(`expected ["a/a"], got`, matches)
	}
}

func TestMeowGlobSharedWithConfigurationStillInvalidates(t *testing.T) {
	for _, regularFirst := range []bool{false, true} {
		ctx := NewContext()
		ctx.MockFileSystem(map[string][]byte{"Android.bp": nil, "src/a.java": nil})
		if regularFirst {
			if _, err := ctx.glob("src/*.java", nil); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ctx.globForMeowSource("src/*.java", nil); err != nil {
			t.Fatal(err)
		}
		if !regularFirst {
			if _, err := ctx.glob("src/*.java", nil); err != nil {
				t.Fatal(err)
			}
		}
		path := filepath.Join(t.TempDir(), "build.ninja")
		if err := ctx.WriteGlobFile(path, time.Now()); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path + ".globs")
		if err != nil {
			t.Fatal(err)
		}
		// Deliberately use an empty filesystem: a regular glob must be checked,
		// whereas an incorrectly marked source-only glob would be skipped.
		changed, _ := pathtools.CheckForChangedGlobsForMeow(pathtools.MockFs(nil), bytes.NewReader(data), 0)
		if !changed {
			t.Fatal("configuration use was hidden by dynamic source use")
		}
	}
}

func TestCachedBuildActionsPreserveMeowGlobClassification(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{"Android.bp": nil, "src/a.java": nil})
	key := globToKey("src/*.java", nil)
	ctx.restoredGlobsFromCache = map[globKey]pathtools.GlobResult{
		key: {Pattern: "src/*.java", Matches: []string{"src/a.java"}, MeowSourceOnly: true},
	}
	if _, err := ctx.globForCachedBuildActions("src/*.java", nil); err != nil {
		t.Fatal(err)
	}
	if _, regular := ctx.nonMeowSourceGlobs.Load(key); regular {
		t.Fatal("cache validation converted a runtime source set into a configuration glob")
	}
	if _, dynamic := ctx.meowSourceGlobs.Load(key); !dynamic {
		t.Fatal("source-only classification was lost")
	}
	if _, err := ctx.glob("src/*.java", nil); err != nil {
		t.Fatal(err)
	}
	if _, regular := ctx.nonMeowSourceGlobs.Load(key); !regular {
		t.Fatal("real configuration use must still override source-only classification")
	}
}

func TestRejectedCachedGlobDoesNotOverrideFreshRuntimeConsumer(t *testing.T) {
	// Changed directories are not restored into restoredGlobsFromCache. A
	// membership check before rejecting cached actions must not become a new
	// configuration consumer and poison the freshly regenerated runtime glob.
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{"Android.bp": nil, "res/values/new.xml": nil})
	if _, err := ctx.globForCachedBuildActions("res/**/*", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ctx.globForMeowSource("res/**/*", nil); err != nil {
		t.Fatal(err)
	}
	if _, regular := ctx.nonMeowSourceGlobs.Load(globToKey("res/**/*", nil)); regular {
		t.Fatal("cache validation registered a configuration consumer")
	}
	path := filepath.Join(t.TempDir(), "build.ninja")
	if err := ctx.WriteGlobFile(path, time.Now()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path + ".globs")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := pathtools.CheckForChangedGlobsForMeow(pathtools.MockFs(map[string][]byte{"res/values/another.xml": nil}), bytes.NewReader(data), -1<<63)
	if err != nil || changed {
		t.Fatalf("cache validation poisoned a runtime resource glob: changed=%t err=%v", changed, err)
	}
}

func TestRestoredConfigurationGlobStillOverridesRuntimeConsumer(t *testing.T) {
	for _, known := range []bool{false, true} {
		ctx := NewContext()
		ctx.MockFileSystem(map[string][]byte{"Android.bp": nil, "res/values/strings.xml": nil})
		key := globToKey("res/**/*", nil)
		if known {
			ctx.restoredGlobsFromCache = map[globKey]pathtools.GlobResult{
				key: {Pattern: "res/**/*", Matches: []string{"res/values/strings.xml"}},
			}
		}
		if _, err := ctx.globForCachedBuildActions("res/**/*", nil); err != nil {
			t.Fatal(err)
		}
		ctx.recordRestoredBuildActionGlobs([]globResultCache{{Pattern: "res/**/*"}})
		if _, err := ctx.globForMeowSource("res/**/*", nil); err != nil {
			t.Fatal(err)
		}
		if _, regular := ctx.nonMeowSourceGlobs.Load(key); !regular {
			t.Fatalf("known=%t: restored configuration consumer was lost", known)
		}
	}
}
