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

package blueprint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/blueprint/bpmodify"
	"github.com/google/blueprint/dbtools"
	"github.com/google/blueprint/gobtools"
	"github.com/google/blueprint/pathtools"
	"github.com/google/blueprint/proptools"
)

type countingOpenFileSystem struct {
	pathtools.FileSystem
	mu         sync.Mutex
	openWrites int
}

type countingBarOutputsMutatorStateCache struct {
	mu            sync.Mutex
	snapshotCalls int
	restoreCalls  int
	version       string
}

func (cache *countingBarOutputsMutatorStateCache) CacheVersion() string {
	return cache.version
}

func (cache *countingBarOutputsMutatorStateCache) Snapshot(module Module) ([]byte, error) {
	cache.mu.Lock()
	cache.snapshotCalls++
	cache.mu.Unlock()
	bar := module.(*barModule)
	return json.Marshal(bar.properties.Outputs)
}

func (cache *countingBarOutputsMutatorStateCache) Restore(module Module, state []byte) error {
	cache.mu.Lock()
	cache.restoreCalls++
	cache.mu.Unlock()
	bar := module.(*barModule)
	return json.Unmarshal(state, &bar.properties.Outputs)
}

func (cache *countingBarOutputsMutatorStateCache) calls() (snapshots, restores int) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.snapshotCalls, cache.restoreCalls
}

type filteredBarOutputsMutatorStateCache struct{}

func (filteredBarOutputsMutatorStateCache) ShouldRun(module Module) bool {
	return module.(*barModule).Name() == "A"
}

func (filteredBarOutputsMutatorStateCache) Snapshot(module Module) ([]byte, error) {
	bar := module.(*barModule)
	return json.Marshal(bar.properties.Outputs)
}

func (filteredBarOutputsMutatorStateCache) Restore(module Module, state []byte) error {
	bar := module.(*barModule)
	return json.Unmarshal(state, &bar.properties.Outputs)
}

type unstableSourceNameTestModule struct {
	baseTestModule
}

func newUnstableSourceNameTestModule() (Module, []interface{}) {
	module := &unstableSourceNameTestModule{}
	return module, []interface{}{&module.baseTestModule.properties, &module.SimpleName.Properties}
}

func (m *unstableSourceNameTestModule) Name() string {
	return m.SimpleName.Properties.Name + fmt.Sprintf("-%p", m)
}

func (m *unstableSourceNameTestModule) SourceDeclarationName() string {
	return m.SimpleName.Properties.Name
}

func TestSourceDeclarationKeysUseStableNameAndPosition(t *testing.T) {
	blueprint := `unstable_module { name: "same" }
unstable_module { name: "same" }`
	parseKeys := func() []string {
		ctx := NewContext()
		ctx.MockFileSystem(map[string][]byte{"Android.bp": []byte(blueprint)})
		ctx.RegisterModuleType("unstable_module", newUnstableSourceNameTestModule)
		if _, errs := ctx.ParseBlueprintsFiles("Android.bp", nil); len(errs) != 0 {
			t.Fatalf("ParseBlueprintsFiles failed: %v", errs)
		}
		keys := make([]string, 0, len(ctx.sourceModuleDeclarations))
		for key := range ctx.sourceModuleDeclarations {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		return keys
	}
	first := parseKeys()
	second := parseKeys()
	if len(first) != 2 {
		t.Fatalf("source declaration keys = %q, want two distinct declarations", first)
	}
	if !slices.Equal(first, second) {
		t.Fatalf("source declaration keys changed across parses: first=%q second=%q", first, second)
	}
}

type noModuleStateMutatorCache struct {
	mu            sync.Mutex
	snapshotCalls int
	restoreCalls  int
}

func (cache *noModuleStateMutatorCache) Snapshot(Module) ([]byte, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.snapshotCalls++
	return nil, nil
}

func (cache *noModuleStateMutatorCache) Restore(Module, []byte) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.restoreCalls++
	return nil
}

func (*noModuleStateMutatorCache) NoModuleState() {}

func (fs *countingOpenFileSystem) OpenFile(name string, flag int, perm os.FileMode) (pathtools.WriteTruncateCloser, error) {
	fs.mu.Lock()
	fs.openWrites++
	fs.mu.Unlock()
	return fs.FileSystem.OpenFile(name, flag, perm)
}

func (fs *countingOpenFileSystem) resetOpenWrites() {
	fs.mu.Lock()
	fs.openWrites = 0
	fs.mu.Unlock()
}

func (fs *countingOpenFileSystem) openWriteCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.openWrites
}

type appendReadTestStore struct {
	dbtools.InMemKeyValueStore
}

func (s *appendReadTestStore) GetAppend(key, buf []byte) ([]byte, error) {
	value, err := s.Get(key)
	if err != nil || value == nil {
		return nil, err
	}
	return append(buf, value...), nil
}

type appendReadTestDecoder struct {
	value string
}

func (d *appendReadTestDecoder) Decode(_ gobtools.EncContext, buf *bytes.Reader) error {
	value, err := io.ReadAll(buf)
	d.value = string(value)
	return err
}

func TestReadReusesAppendStoreBuffer(t *testing.T) {
	store := &appendReadTestStore{}
	if err := store.Put([]byte("cache-key"), []byte("cached value")); err != nil {
		t.Fatal(err)
	}
	decoder := &appendReadTestDecoder{}
	ok, err := read(nil, store, []byte("cache-key"), decoder)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || decoder.value != "cached value" {
		t.Fatalf("read() = (%t, %q), want (true, %q)", ok, decoder.value, "cached value")
	}
	if ok, err := read(nil, store, []byte("missing"), &appendReadTestDecoder{}); err != nil || ok {
		t.Fatalf("read() for missing key = (%t, %v), want (false, nil)", ok, err)
	}
}

func TestSourceModuleDeclarationSnapshotDetectsAddedChangedAndRemovedModules(t *testing.T) {
	cache := &KeyValueStoreCache{}
	if err := cache.openForTests(); err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.close()
	encContext := gobtools.NewEncContext(cache.referencesDb)

	first := &Context{
		keyValueStoreCache: cache,
		EncContext:         encContext,
		sourceModuleDeclarations: map[string]proptools.Hash{
			"Android.bp\x00module\x00same":    {1},
			"Android.bp\x00module\x00removed": {2},
		},
	}
	if err := first.writeSourceModuleDeclarationSnapshots(); err != nil {
		t.Fatalf("write initial snapshot: %v", err)
	}

	second := &Context{
		keyValueStoreCache: cache,
		EncContext:         encContext,
		sourceModuleDeclarations: map[string]proptools.Hash{
			"Android.bp\x00module\x00same":    {1},
			"Android.bp\x00module\x00changed": {3},
		},
	}
	if err := second.compareSourceModuleDeclarationSnapshot(); err != nil {
		t.Fatalf("compare snapshot: %v", err)
	}
	if !second.sourceDeclarationSnapshotValid {
		t.Fatal("existing snapshot was not accepted")
	}
	if want := []string{"Android.bp\x00module\x00changed"}; !slices.Equal(second.changedSourceModuleDeclarations, want) {
		t.Fatalf("changed declarations = %q, want %q", second.changedSourceModuleDeclarations, want)
	}
	if want := []string{"Android.bp\x00module\x00removed"}; !slices.Equal(second.removedSourceModuleDeclarations, want) {
		t.Fatalf("removed declarations = %q, want %q", second.removedSourceModuleDeclarations, want)
	}
}

func TestSourceModuleDeclarationSnapshotWithoutCacheMarksAllModulesChanged(t *testing.T) {
	cache := &KeyValueStoreCache{}
	if err := cache.openForTests(); err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.close()

	ctx := &Context{
		keyValueStoreCache: cache,
		EncContext:         gobtools.NewEncContext(cache.referencesDb),
		sourceModuleDeclarations: map[string]proptools.Hash{
			"Android.bp\x00module\x00second": {2},
			"Android.bp\x00module\x00first":  {1},
		},
	}
	if err := ctx.compareSourceModuleDeclarationSnapshot(); err != nil {
		t.Fatalf("compare missing snapshot: %v", err)
	}
	if ctx.sourceDeclarationSnapshotValid {
		t.Fatal("missing snapshot was marked valid")
	}
	want := []string{"Android.bp\x00module\x00first", "Android.bp\x00module\x00second"}
	if !slices.Equal(ctx.changedSourceModuleDeclarations, want) {
		t.Fatalf("changed declarations = %q, want %q", ctx.changedSourceModuleDeclarations, want)
	}
}

func TestSourceModuleDeclarationSnapshotBuildsReverseDependencyClosure(t *testing.T) {
	cache := &KeyValueStoreCache{}
	if err := cache.openForTests(); err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.close()
	encContext := gobtools.NewEncContext(cache.referencesDb)
	a := "Android.bp\x00module\x00a"
	b := "Android.bp\x00module\x00b"
	c := "Android.bp\x00module\x00c"

	first := &Context{
		keyValueStoreCache: cache,
		EncContext:         encContext,
		sourceModuleDeclarations: map[string]proptools.Hash{
			a: {1},
			b: {2},
			c: {3},
		},
		sourceDeclarationModuleCacheKeys: map[string][]string{
			a: {"module-a-variant"},
			b: {"module-b-variant"},
			c: {"module-c-variant"},
		},
		sourceDeclarationDependents: map[string][]string{
			a: {b},
			b: {c},
		},
		sourceDeclarationGraphComplete: true,
	}
	if err := first.writeSourceModuleDeclarationSnapshots(); err != nil {
		t.Fatalf("write initial snapshot: %v", err)
	}

	second := &Context{
		keyValueStoreCache: cache,
		EncContext:         encContext,
		sourceModuleDeclarations: map[string]proptools.Hash{
			a: {10},
			b: {2},
			c: {3},
		},
	}
	if err := second.compareSourceModuleDeclarationSnapshot(); err != nil {
		t.Fatalf("compare changed declaration snapshot: %v", err)
	}
	if !second.sourceDeclarationGraphValid {
		t.Fatal("complete graph snapshot was not accepted")
	}
	if want := []string{a, b, c}; !slices.Equal(second.affectedSourceModuleDeclarations, want) {
		t.Fatalf("affected declarations = %q, want %q", second.affectedSourceModuleDeclarations, want)
	}
	wantModuleKeys := []string{"module-a-variant", "module-b-variant", "module-c-variant"}
	if !slices.Equal(second.affectedModuleCacheKeys, wantModuleKeys) {
		t.Fatalf("affected module cache keys = %q, want %q", second.affectedModuleCacheKeys, wantModuleKeys)
	}
}

func TestSourceModuleDeclarationSnapshotDetectsGlobChangesBeforeMutators(t *testing.T) {
	cache := &KeyValueStoreCache{}
	if err := cache.openForTests(); err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.close()
	encContext := gobtools.NewEncContext(cache.referencesDb)
	module := "packages/apps/Launcher3/Android.bp\x00android_app\x00Launcher3QuickStep"
	oldMatches := stringList{"src/A.java"}
	oldHash, err := proptools.CalculateHash(oldMatches)
	if err != nil {
		t.Fatal(err)
	}
	first := &Context{
		keyValueStoreCache: cache,
		EncContext:         encContext,
		sourceModuleDeclarations: map[string]proptools.Hash{
			module: {1},
		},
		sourceDeclarationModuleCacheKeys: map[string][]string{
			module: {"launcher3-quickstep-arm64"},
		},
		sourceDeclarationGlobs: map[string][]globResultCache{
			module: {{Pattern: "src/*.java", Result: oldHash}},
		},
		sourceDeclarationGraphComplete: true,
	}
	if err := first.writeSourceModuleDeclarationSnapshots(); err != nil {
		t.Fatalf("write initial snapshot: %v", err)
	}

	second := newContext()
	second.fs = pathtools.MockFs(map[string][]byte{
		"src/A.java": nil,
		"src/B.java": nil,
	})
	second.keyValueStoreCache = cache
	second.EncContext = encContext
	second.incrementalEnabled = true
	second.incrementalAnalysis = true
	second.sourceModuleDeclarations = map[string]proptools.Hash{module: {1}}
	if err := second.PrepareIncrementalAnalysis(); err != nil {
		t.Fatalf("prepare incremental analysis: %v", err)
	}
	if !second.sourceDeclarationGraphValid {
		t.Fatal("complete graph snapshot was not accepted")
	}
	if want := []string{module}; !slices.Equal(second.changedSourceModuleDeclarations, want) {
		t.Fatalf("changed declarations = %q, want %q", second.changedSourceModuleDeclarations, want)
	}
	if want := []string{module}; !slices.Equal(second.affectedSourceModuleDeclarations, want) {
		t.Fatalf("affected declarations = %q, want %q", second.affectedSourceModuleDeclarations, want)
	}
	if want := []string{"launcher3-quickstep-arm64"}; !slices.Equal(second.affectedModuleCacheKeys, want) {
		t.Fatalf("affected module cache keys = %q, want %q", second.affectedModuleCacheKeys, want)
	}

	unchanged := newContext()
	unchanged.fs = pathtools.MockFs(map[string][]byte{"src/A.java": nil})
	unchanged.keyValueStoreCache = cache
	unchanged.EncContext = encContext
	unchanged.incrementalEnabled = true
	unchanged.incrementalAnalysis = true
	unchanged.sourceModuleDeclarations = map[string]proptools.Hash{module: {1}}
	if err := unchanged.PrepareIncrementalAnalysis(); err != nil {
		t.Fatalf("prepare unchanged incremental analysis: %v", err)
	}
	if len(unchanged.changedSourceModuleDeclarations) != 0 {
		t.Fatalf("unchanged glob marked declarations changed: %q", unchanged.changedSourceModuleDeclarations)
	}
}

func TestIncrementalMutatorStateCacheSkipsUnchangedModules(t *testing.T) {
	cache := &KeyValueStoreCache{}
	if err := cache.openForTests(); err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.close()

	stateCache := &countingBarOutputsMutatorStateCache{version: "v1"}
	var visitMu sync.Mutex
	visits := make(map[string]int)
	makeContext := func(bp string, incrementalAnalysis bool) *Context {
		ctx := NewContext()
		ctx.SetSrcDir(t.TempDir())
		ctx.MockFileSystem(map[string][]byte{"Android.bp": []byte(bp)})
		ctx.RegisterModuleType("bar_module", newBarModule)
		ctx.RegisterBottomUpMutator("cached_property", func(mctx BottomUpMutatorContext) {
			bar := mctx.Module().(*barModule)
			visitMu.Lock()
			visits[bar.Name()]++
			visitMu.Unlock()
			bar.properties.Outputs = append(bar.properties.Outputs, "-mutated")
		}).IncrementalStateCache(stateCache)
		ctx.SetIncrementalEnabled(true)
		ctx.SetIncrementalAnalysis(incrementalAnalysis)
		ctx.SetMutatorVisitStatsEnabled(true)
		ctx.keyValueStoreCache = cache
		ctx.EncContext = gobtools.NewEncContext(cache.referencesDb)
		if _, errs := ctx.ParseBlueprintsFiles("Android.bp", nil); len(errs) > 0 {
			t.Fatalf("parse Blueprint files: %v", errs)
		}
		if incrementalAnalysis {
			if err := ctx.PrepareIncrementalAnalysis(); err != nil {
				t.Fatalf("prepare incremental analysis: %v", err)
			}
		}
		if _, errs := ctx.ResolveDependencies(nil); len(errs) > 0 {
			t.Fatalf("resolve dependencies: %v", errs)
		}
		return ctx
	}
	blueprint := `bar_module { name: "A", outputs: ["a"] }
bar_module { name: "B", outputs: ["b"] }`
	first := makeContext(blueprint, false)
	first.sourceDeclarationGraphComplete = true
	if err := first.writeSourceModuleDeclarationSnapshots(); err != nil {
		t.Fatalf("write first source declaration snapshot: %v", err)
	}
	if visits["A"] != 1 || visits["B"] != 1 {
		t.Fatalf("first run visits = %v, want one visit per module", visits)
	}
	if snapshots, restores := stateCache.calls(); snapshots != 0 || restores != 0 {
		t.Fatalf("non-analysis run cache calls = snapshot %d, restore %d; want 0, 0", snapshots, restores)
	}

	visits = make(map[string]int)
	second := makeContext(blueprint, true)
	if visits["A"] != 1 || visits["B"] != 1 {
		t.Fatalf("first incremental-analysis run visits = %v, want full traversal to initialize cache", visits)
	}
	if snapshots, restores := stateCache.calls(); snapshots != 2 || restores != 0 {
		t.Fatalf("first analysis run cache calls = snapshot %d, restore %d; want 2, 0", snapshots, restores)
	}
	stat := second.MutatorVisitStats()["cached_property"]
	if stat.StateCacheScannedVariants != 2 || stat.StateCacheHits != 0 || stat.StateCacheMisses != 0 ||
		stat.StateCacheRestoreErrors != 0 || stat.VisitedVariants != 2 {
		t.Fatalf("first analysis run mutator stats = %+v, want full two-module traversal", stat)
	}
	if !stat.StateCacheEnabled || stat.StateCacheSubsetEnabled || !stat.IncrementalBuildActionsEnabled ||
		!stat.IncrementalAnalysisEnabled || !stat.SourceDeclarationGraphValid || !stat.StateCacheDatabaseAvailable ||
		stat.AffectedSourceDeclarations != 0 || stat.AffectedSourceVariants != 0 || stat.UnkeyedSourceVariants != 0 {
		t.Fatalf("first analysis run cache selection stats = %+v, want version-triggered full traversal", stat)
	}
	for _, name := range []string{"A", "B"} {
		module := second.moduleGroupFromName(name, nil).modules.firstModule().logicModule.(*barModule)
		want := strings.ToLower(name) + ",-mutated"
		if got := strings.Join(module.properties.Outputs, ","); got != want {
			t.Fatalf("restored %s outputs = %q, want mutated cached output", name, got)
		}
	}

	visits = make(map[string]int)
	third := makeContext(`bar_module { name: "A", outputs: ["changed"] }
bar_module { name: "B", outputs: ["b"] }`, true)
	if visits["A"] != 1 || visits["B"] != 0 {
		t.Fatalf("single-module edit visits = %v, want only A", visits)
	}
	if snapshots, restores := stateCache.calls(); snapshots != 3 || restores != 1 {
		t.Fatalf("single-module edit cache calls = snapshot %d, restore %d; want 3, 1", snapshots, restores)
	}
	stat = third.MutatorVisitStats()["cached_property"]
	if stat.StateCacheScannedVariants != 2 || stat.StateCacheHits != 1 || stat.StateCacheMisses != 0 ||
		stat.StateCacheRestoreErrors != 0 || stat.VisitedVariants != 1 {
		t.Fatalf("single-module edit mutator stats = %+v, want 2 scanned, 1 hit, 1 callback", stat)
	}
	if !stat.StateCacheSubsetEnabled || stat.AffectedSourceDeclarations != 1 ||
		stat.AffectedSourceVariants != 1 || stat.UnkeyedSourceVariants != 0 {
		t.Fatalf("single-module edit cache selection stats = %+v, want one affected declaration/variant and no unkeyed variants", stat)
	}
	if got := strings.Join(third.moduleGroupFromName("A", nil).modules.firstModule().logicModule.(*barModule).properties.Outputs, ","); got != "changed,-mutated" {
		t.Fatalf("changed A outputs = %q, want changed-mutated", got)
	}
	if got := strings.Join(third.moduleGroupFromName("B", nil).modules.firstModule().logicModule.(*barModule).properties.Outputs, ","); got != "b,-mutated" {
		t.Fatalf("unchanged B outputs = %q, want b-mutated", got)
	}

	stateCache.version = "v2"
	visits = make(map[string]int)
	fourth := makeContext(blueprint, true)
	if visits["A"] != 1 || visits["B"] != 1 {
		t.Fatalf("mutator cache version change visits = %v, want full module traversal", visits)
	}
	stat = fourth.MutatorVisitStats()["cached_property"]
	if stat.StateCacheSubsetEnabled || stat.VisitedVariants != 2 {
		t.Fatalf("mutator cache version change stats = %+v, want full traversal", stat)
	}
}

func TestDependencyResolutionCacheReusesUnchangedVariantLookup(t *testing.T) {
	cache := &KeyValueStoreCache{}
	if err := cache.openForTests(); err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.close()

	makeContext := func(bp string, incrementalAnalysis bool) *Context {
		ctx := NewContext()
		ctx.SetSrcDir(t.TempDir())
		ctx.MockFileSystem(map[string][]byte{"Android.bp": []byte(bp)})
		ctx.RegisterModuleType("bar_module", newBarModule)
		ctx.RegisterBottomUpMutator("cached_deps", func(mctx BottomUpMutatorContext) {
			if mctx.ModuleName() == "A" {
				mctx.AddDependency(mctx.Module(), dependencyResolutionTestTag{}, "B")
			}
		}).CacheDependencyLookups("v1")
		ctx.SetIncrementalEnabled(true)
		ctx.SetIncrementalAnalysis(incrementalAnalysis)
		ctx.SetMutatorVisitStatsEnabled(true)
		ctx.keyValueStoreCache = cache
		ctx.EncContext = gobtools.NewEncContext(cache.referencesDb)
		if _, errs := ctx.ParseBlueprintsFiles("Android.bp", nil); len(errs) > 0 {
			t.Fatalf("parse Blueprint files: %v", errs)
		}
		if incrementalAnalysis {
			if err := ctx.PrepareIncrementalAnalysis(); err != nil {
				t.Fatalf("prepare incremental analysis: %v", err)
			}
		}
		if _, errs := ctx.ResolveDependencies(nil); len(errs) > 0 {
			t.Fatalf("resolve dependencies: %v", errs)
		}
		return ctx
	}
	blueprint := `bar_module { name: "A", outputs: ["a"] }
bar_module { name: "B", outputs: ["b"] }`
	first := makeContext(blueprint, false)
	first.sourceDeclarationGraphComplete = true
	if err := first.writeSourceModuleDeclarationSnapshots(); err != nil {
		t.Fatalf("write initial source declaration snapshot: %v", err)
	}
	if stat := first.MutatorVisitStats()["cached_deps"]; stat.DependencyLookupCacheHits != 0 || stat.DependencyLookupCacheMisses != 1 {
		t.Fatalf("initial cache stats = %+v, want 0 hits and 1 miss", stat)
	}

	second := makeContext(blueprint, true)
	if stat := second.MutatorVisitStats()["cached_deps"]; stat.DependencyLookupCacheHits != 1 || stat.DependencyLookupCacheMisses != 0 {
		t.Fatalf("warm cache stats = %+v, want 1 hit and 0 misses", stat)
	}
	third := makeContext(blueprint, true)
	if stat := third.MutatorVisitStats()["cached_deps"]; stat.DependencyLookupCacheHits != 1 || stat.DependencyLookupCacheMisses != 0 {
		t.Fatalf("second warm cache stats = %+v, want 1 hit and 0 misses", stat)
	}

	changed := makeContext(`bar_module { name: "A", outputs: ["a"] }
bar_module { name: "B", outputs: ["changed"] }`, true)
	if stat := changed.MutatorVisitStats()["cached_deps"]; stat.DependencyLookupCacheHits != 0 || stat.DependencyLookupCacheMisses != 1 {
		t.Fatalf("changed target source stats = %+v, want 0 hits and 1 miss", stat)
	}
}

type dependencyResolutionTestTag struct {
	BaseDependencyTag
}

func TestMutatorStateCachesDoNotSplitDefaultMutatorSchedule(t *testing.T) {
	mutators := []*mutatorInfo{
		{
			name:                  "cached",
			bottomUpMutator:       func(BottomUpMutatorContext) {},
			incrementalStateCache: &noModuleStateMutatorCache{},
		},
		{
			name:            "neighbor",
			bottomUpMutator: func(BottomUpMutatorContext) {},
		},
	}

	defaultGroups := coalesceMutators(mutators, false)
	if len(defaultGroups) != 1 || len(defaultGroups[0]) != 2 {
		t.Fatalf("default mutator groups = %v, want both adjacent mutators coalesced", defaultGroups)
	}
	incrementalGroups := coalesceMutators(mutators, true)
	if len(incrementalGroups) != 2 || len(incrementalGroups[0]) != 1 || len(incrementalGroups[1]) != 1 {
		t.Fatalf("incremental-analysis mutator groups = %v, want cacheable mutator isolated", incrementalGroups)
	}
}

func TestIncrementalStatelessMutatorUsesSourceGroupIndex(t *testing.T) {
	cache := &KeyValueStoreCache{}
	if err := cache.openForTests(); err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.close()

	stateCache := &noModuleStateMutatorCache{}
	var visitMu sync.Mutex
	visits := make(map[string]int)
	makeContext := func(bp string, incrementalAnalysis bool) *Context {
		ctx := NewContext()
		ctx.SetSrcDir(t.TempDir())
		ctx.MockFileSystem(map[string][]byte{"Android.bp": []byte(bp)})
		ctx.RegisterModuleType("bar_module", newBarModule)
		ctx.RegisterBottomUpMutator("cached_validation", func(mctx BottomUpMutatorContext) {
			bar := mctx.Module().(*barModule)
			visitMu.Lock()
			visits[bar.Name()]++
			visitMu.Unlock()
		}).IncrementalStateCache(stateCache)
		ctx.SetIncrementalEnabled(true)
		ctx.SetIncrementalAnalysis(incrementalAnalysis)
		ctx.keyValueStoreCache = cache
		ctx.EncContext = gobtools.NewEncContext(cache.referencesDb)
		if _, errs := ctx.ParseBlueprintsFiles("Android.bp", nil); len(errs) > 0 {
			t.Fatalf("parse Blueprint files: %v", errs)
		}
		if incrementalAnalysis {
			if err := ctx.PrepareIncrementalAnalysis(); err != nil {
				t.Fatalf("prepare incremental analysis: %v", err)
			}
		}
		if _, errs := ctx.ResolveDependencies(nil); len(errs) > 0 {
			t.Fatalf("resolve dependencies: %v", errs)
		}
		return ctx
	}
	blueprint := `bar_module { name: "A" }
bar_module { name: "B" }`
	first := makeContext(blueprint, false)
	first.sourceDeclarationGraphComplete = true
	if err := first.writeSourceModuleDeclarationSnapshots(); err != nil {
		t.Fatalf("write first source declaration snapshot: %v", err)
	}
	if visits["A"] != 1 || visits["B"] != 1 {
		t.Fatalf("first run visits = %v, want one visit per module", visits)
	}
	if stateCache.snapshotCalls != 0 || stateCache.restoreCalls != 0 {
		t.Fatalf("stateless cache invoked per-module persistence: snapshot=%d restore=%d", stateCache.snapshotCalls, stateCache.restoreCalls)
	}

	visits = make(map[string]int)
	_ = makeContext(blueprint, true)
	if len(visits) != 0 {
		t.Fatalf("unchanged run visited source groups: %v", visits)
	}

	visits = make(map[string]int)
	_ = makeContext(`bar_module { name: "A", outputs: ["changed"] }
bar_module { name: "B" }`, true)
	if visits["A"] != 1 || visits["B"] != 0 {
		t.Fatalf("single-module edit visits = %v, want only A", visits)
	}
}

func TestIncrementalMutatorStateCacheFiltersOutUnaffectedModules(t *testing.T) {
	cache := &KeyValueStoreCache{}
	if err := cache.openForTests(); err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.close()

	var visitMu sync.Mutex
	visits := make(map[string]int)
	makeContext := func(bp string, incrementalAnalysis bool) *Context {
		ctx := NewContext()
		ctx.SetSrcDir(t.TempDir())
		ctx.MockFileSystem(map[string][]byte{"Android.bp": []byte(bp)})
		ctx.RegisterModuleType("bar_module", newBarModule)
		ctx.RegisterBottomUpMutator("filtered_mutator", func(mctx BottomUpMutatorContext) {
			bar := mctx.Module().(*barModule)
			visitMu.Lock()
			visits[bar.Name()]++
			visitMu.Unlock()
			bar.properties.Outputs = append(bar.properties.Outputs, "-mutated")
		}).IncrementalStateCache(filteredBarOutputsMutatorStateCache{})
		ctx.SetIncrementalEnabled(true)
		ctx.SetIncrementalAnalysis(incrementalAnalysis)
		ctx.keyValueStoreCache = cache
		ctx.EncContext = gobtools.NewEncContext(cache.referencesDb)
		if _, errs := ctx.ParseBlueprintsFiles("Android.bp", nil); len(errs) > 0 {
			t.Fatalf("parse Blueprint files: %v", errs)
		}
		if incrementalAnalysis {
			if err := ctx.PrepareIncrementalAnalysis(); err != nil {
				t.Fatalf("prepare incremental analysis: %v", err)
			}
		}
		if _, errs := ctx.ResolveDependencies(nil); len(errs) > 0 {
			t.Fatalf("resolve dependencies: %v", errs)
		}
		return ctx
	}
	first := makeContext(`bar_module { name: "A", outputs: ["a"] }
bar_module { name: "B", outputs: ["b"] }`, true)
	if visits["A"] != 1 || visits["B"] != 0 {
		t.Fatalf("filtered first run visits = %v, want only A", visits)
	}
	if got := strings.Join(first.moduleGroupFromName("B", nil).modules.firstModule().logicModule.(*barModule).properties.Outputs, ","); got != "b" {
		t.Fatalf("filtered B outputs = %q, want unchanged b", got)
	}
	first.sourceDeclarationGraphComplete = true
	if err := first.writeSourceModuleDeclarationSnapshots(); err != nil {
		t.Fatalf("write first source declaration snapshot: %v", err)
	}

	visits = make(map[string]int)
	second := makeContext(`bar_module { name: "A", outputs: ["a"] }
bar_module { name: "B", outputs: ["changed"] }`, true)
	if len(visits) != 0 {
		t.Fatalf("filtered incremental run visits = %v, want no callbacks", visits)
	}
	if got := strings.Join(second.moduleGroupFromName("A", nil).modules.firstModule().logicModule.(*barModule).properties.Outputs, ","); got != "a,-mutated" {
		t.Fatalf("restored A outputs = %q, want cached mutation", got)
	}
	if got := strings.Join(second.moduleGroupFromName("B", nil).modules.firstModule().logicModule.(*barModule).properties.Outputs, ","); got != "changed" {
		t.Fatalf("filtered changed B outputs = %q, want source value unchanged by mutator", got)
	}
}

func TestPrepareIncrementalAnalysisLoadsAffectedClosureBeforeMutators(t *testing.T) {
	cache := &KeyValueStoreCache{}
	if err := cache.openForTests(); err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.close()
	encContext := gobtools.NewEncContext(cache.referencesDb)
	a := "Android.bp\x00module\x00a"
	b := "Android.bp\x00module\x00b"
	previous := &Context{
		keyValueStoreCache: cache,
		EncContext:         encContext,
		sourceModuleDeclarations: map[string]proptools.Hash{
			a: {1},
			b: {2},
		},
		sourceDeclarationModuleCacheKeys: map[string][]string{
			a: {"module-a-variant"},
			b: {"module-b-variant"},
		},
		sourceDeclarationDependents: map[string][]string{
			a: {b},
		},
		sourceDeclarationGraphComplete: true,
	}
	if err := previous.writeSourceModuleDeclarationSnapshots(); err != nil {
		t.Fatalf("write previous declaration snapshot: %v", err)
	}

	current := &Context{
		keyValueStoreCache:  cache,
		EncContext:          encContext,
		incrementalEnabled:  true,
		incrementalAnalysis: true,
		sourceModuleDeclarations: map[string]proptools.Hash{
			a: {9},
			b: {2},
		},
	}
	if err := current.PrepareIncrementalAnalysis(); err != nil {
		t.Fatalf("prepare incremental analysis: %v", err)
	}
	if !current.sourceDeclarationGraphValid {
		t.Fatal("previous declaration graph was not accepted")
	}
	if want := []string{a, b}; !slices.Equal(current.affectedSourceModuleDeclarations, want) {
		t.Fatalf("affected declarations = %q, want %q", current.affectedSourceModuleDeclarations, want)
	}
	if want := []string{"module-a-variant", "module-b-variant"}; !slices.Equal(current.affectedModuleCacheKeys, want) {
		t.Fatalf("affected module cache keys = %q, want %q", current.affectedModuleCacheKeys, want)
	}
}

func TestSourceModuleDeclarationSnapshotFallsBackForModuleSetChanges(t *testing.T) {
	cache := &KeyValueStoreCache{}
	if err := cache.openForTests(); err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.close()
	encContext := gobtools.NewEncContext(cache.referencesDb)
	old := "Android.bp\x00module\x00old"
	kept := "Android.bp\x00module\x00kept"
	first := &Context{
		keyValueStoreCache: cache,
		EncContext:         encContext,
		sourceModuleDeclarations: map[string]proptools.Hash{
			old:  {1},
			kept: {2},
		},
		sourceDeclarationModuleCacheKeys: map[string][]string{
			old:  {"old-variant"},
			kept: {"kept-variant"},
		},
		sourceDeclarationDependents:    map[string][]string{old: {kept}},
		sourceDeclarationGraphComplete: true,
	}
	if err := first.writeSourceModuleDeclarationSnapshots(); err != nil {
		t.Fatalf("write initial snapshot: %v", err)
	}

	added := "Android.bp\x00module\x00added"
	second := &Context{
		keyValueStoreCache: cache,
		EncContext:         encContext,
		sourceModuleDeclarations: map[string]proptools.Hash{
			added: {3},
			kept:  {2},
		},
	}
	if err := second.compareSourceModuleDeclarationSnapshot(); err != nil {
		t.Fatalf("compare changed module set: %v", err)
	}
	if second.sourceDeclarationGraphValid {
		t.Fatal("module additions/removals must force full analysis")
	}
	if want := []string{added}; !slices.Equal(second.addedSourceModuleDeclarations, want) {
		t.Fatalf("added declarations = %q, want %q", second.addedSourceModuleDeclarations, want)
	}
	if want := []string{old}; !slices.Equal(second.removedSourceModuleDeclarations, want) {
		t.Fatalf("removed declarations = %q, want %q", second.removedSourceModuleDeclarations, want)
	}
	if len(second.affectedSourceModuleDeclarations) != 0 {
		t.Fatalf("affected declarations = %q, want no partial closure", second.affectedSourceModuleDeclarations)
	}
}

func TestPrepareSingletonDependencyStateBuildsSourceDeclarationGraph(t *testing.T) {
	ctx := incrementalSetup(t)
	ctx.SetIncrementalEnabled(true)
	ctx.SetIncrementalAnalysis(false)
	defer ctx.keyValueStoreCache.close()
	if _, errs := ctx.PrepareBuildActions(nil); len(errs) > 0 {
		t.Fatalf("PrepareBuildActions failed: %v", errs)
	}
	if !ctx.sourceDeclarationGraphComplete {
		t.Fatal("source declaration dependency graph was unexpectedly incomplete")
	}

	bar := sourceModuleDeclarationKey("Android.bp", "bar_module", "MyBarModule")
	incremental := sourceModuleDeclarationKey("Android.bp", "incremental_module", "MyIncrementalModule")
	foo := sourceModuleDeclarationKey("Android.bp", "foo_module", "MyFooModule")
	if got := ctx.sourceDeclarationDependents[bar]; !slices.Equal(got, []string{incremental}) {
		t.Fatalf("dependents of bar = %q, want [%q]", got, incremental)
	}
	if got := ctx.sourceDeclarationDependents[incremental]; !slices.Equal(got, []string{foo}) {
		t.Fatalf("dependents of incremental = %q, want [%q]", got, foo)
	}
	if got := len(ctx.sourceDeclarationModuleCacheKeys[incremental]); got == 0 {
		t.Fatal("incremental module variants were not associated with their source declaration")
	}
}

func TestMutatorVisitStatsCountVisitedVariants(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`bar_module { name: "test" }`),
	})
	ctx.RegisterModuleType("bar_module", newBarModule)
	ctx.RegisterBottomUpMutator("visit_stats_test", func(BottomUpMutatorContext) {}).MutatesGlobalState()
	if _, errs := ctx.ParseBlueprintsFiles("Android.bp", nil); len(errs) != 0 {
		t.Fatalf("ParseBlueprintsFiles failed: %v", errs)
	}
	ctx.SetMutatorVisitStatsEnabled(true)
	if _, errs := ctx.ResolveDependencies(nil); len(errs) != 0 {
		t.Fatalf("ResolveDependencies failed: %v", errs)
	}
	stats := ctx.MutatorVisitStats()
	stat, ok := stats["visit_stats_test"]
	if !ok {
		t.Fatalf("visit statistics did not include test mutator: %v", stats)
	}
	if stat.VisitedVariants != 1 {
		t.Fatalf("visited variants = %d, want 1", stat.VisitedVariants)
	}
}

func TestShardModulesByStableModuleIDKeepsVariantsAndAssignmentsStable(t *testing.T) {
	modules := []*moduleInfo{
		{relBlueprintsFile: "packages/apps/Launcher/Android.bp", cachedUniqueName: "LauncherLib", typeName: "java_library", variant: variant{name: "android_common"}},
		{relBlueprintsFile: "packages/apps/Launcher/Android.bp", cachedUniqueName: "LauncherLib", typeName: "java_library", variant: variant{name: "android_arm64"}},
		{relBlueprintsFile: "packages/apps/Launcher/Android.bp", cachedUniqueName: "LauncherApp", typeName: "android_app", variant: variant{name: "android_common"}},
		{relBlueprintsFile: "frameworks/base/Android.bp", cachedUniqueName: "framework", typeName: "java_library", variant: variant{name: "android_common"}},
	}
	first := shardModulesByStableModuleID(modules, ninjaShardCount)
	firstShard := make(map[string]int)
	launcherLibShard := -1
	for shardIndex, shard := range first {
		for _, module := range shard {
			firstShard[module.cachedUniqueName] = shardIndex
			if module.cachedUniqueName == "LauncherLib" {
				if launcherLibShard == -1 {
					launcherLibShard = shardIndex
				} else if launcherLibShard != shardIndex {
					t.Fatal("variants of one module were split across shards")
				}
			}
		}
	}
	if launcherLibShard < 0 {
		t.Fatal("module was not assigned to a shard")
	}
	if firstShard["LauncherLib"] == firstShard["LauncherApp"] {
		t.Fatal("distinct modules in one Blueprint package were not distributed across shards")
	}

	extended := append(slices.Clone(modules), &moduleInfo{
		relBlueprintsFile: "packages/apps/Launcher/Android.bp",
		cachedUniqueName:  "Settings",
		typeName:          "android_app",
		variant:           variant{name: "android_common"},
	})
	second := shardModulesByStableModuleID(extended, ninjaShardCount)
	for shardIndex, shard := range second {
		for _, module := range shard {
			if want, exists := firstShard[module.cachedUniqueName]; exists && shardIndex != want {
				t.Fatalf("module %s moved from shard %d to %d after an unrelated addition", module.cachedUniqueName, want, shardIndex)
			}
		}
	}
}

type moduleWithStableCacheIdentity struct {
	Module
	identity string
}

func (m *moduleWithStableCacheIdentity) ModuleActionCacheIdentity() string {
	return m.identity
}

func TestModuleActionCacheIdentityIgnoresProcessSpecificUniqueNames(t *testing.T) {
	newModule := func(file, uniqueName string) *moduleInfo {
		return &moduleInfo{
			logicModule:       &moduleWithStableCacheIdentity{Module: &fooModule{}, identity: "stable-properties-hash"},
			relBlueprintsFile: file,
			cachedUniqueName:  uniqueName,
			typeName:          "generated_noop",
			variant:           variant{name: "android_common"},
		}
	}
	first := newModule("device/foo/Android.bp", "generated0xc001")
	second := newModule("device/foo/Android.bp", "generated0xc002")
	if first.moduleCacheKey() != second.moduleCacheKey() {
		t.Fatalf("stable cache identity changed with process-specific module names: %q != %q", first.moduleCacheKey(), second.moduleCacheKey())
	}
	if firstShard := stableNinjaShardIndex(moduleShardIdentity(first), ninjaShardCount); firstShard != stableNinjaShardIndex(moduleShardIdentity(second), ninjaShardCount) {
		t.Fatalf("stable module identity moved between Ninja shards: %d != %d", firstShard, stableNinjaShardIndex(moduleShardIdentity(second), ninjaShardCount))
	}
	if first.moduleCacheKey() == newModule("device/bar/Android.bp", "generated0xc001").moduleCacheKey() {
		t.Fatal("modules from distinct Blueprint files shared an action-cache key")
	}
}

func TestShardPhonyBuildDefsByStableOutputHasStableOrder(t *testing.T) {
	defs := []*buildDef{
		{OutputStrings: []string{"dedup-z"}},
		{OutputStrings: []string{"dedup-a"}},
		{OutputStrings: []string{"dedup-m"}},
		{OutputStrings: []string{"dedup-b"}},
	}

	first := shardPhonyBuildDefsByStableOutput(defs, 17)
	second := shardPhonyBuildDefsByStableOutput([]*buildDef{defs[3], defs[1], defs[0], defs[2]}, 17)
	firstOutputs := make([][]string, len(first))
	secondOutputs := make([][]string, len(second))
	for shard := range first {
		for _, def := range first[shard] {
			firstOutputs[shard] = append(firstOutputs[shard], strings.Join(def.OutputStrings, "\x00"))
		}
		for _, def := range second[shard] {
			secondOutputs[shard] = append(secondOutputs[shard], strings.Join(def.OutputStrings, "\x00"))
		}
	}
	if !reflect.DeepEqual(firstOutputs, secondOutputs) {
		t.Fatalf("phony shard order depends on input order:\nfirst:  %q\nsecond: %q", firstOutputs, secondOutputs)
	}
}

func TestIncrementalNinjaShardCacheReuse(t *testing.T) {
	ctx := NewContext()
	ctx.SetSrcDir(t.TempDir())
	shardFile := filepath.Join(ctx.SrcDir(), "build.ninja.0")
	if err := os.WriteFile(shardFile, []byte("build target: phony\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fingerprint := "test-fingerprint"
	if err := ctx.writeNinjaShardCacheRecord(shardFile, fingerprint); err != nil {
		t.Fatal(err)
	}
	if !ctx.ninjaShardCacheMatches(shardFile, fingerprint) {
		t.Fatal("matching shard cache record was rejected")
	}
	if ctx.ninjaShardCacheMatches(shardFile, "different-fingerprint") {
		t.Fatal("shard cache record matched a different graph fingerprint")
	}
	if err := os.WriteFile(shardFile, []byte("build target: phony\nchanged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if ctx.ninjaShardCacheMatches(shardFile, fingerprint) {
		t.Fatal("shard cache record survived a changed shard file")
	}
}

func TestModuleNinjaShardWriterTouchesOnlyChangedContent(t *testing.T) {
	ctx := incrementalSetup(t)
	if _, errs := ctx.PrepareBuildActions(nil); len(errs) > 0 {
		t.Fatalf("unexpected errors preparing build actions: %v", errs)
	}

	countingFs := &countingOpenFileSystem{FileSystem: ctx.fs}
	ctx.fs = countingFs
	writeShards := func() {
		t.Helper()
		if err := ctx.writeAllModuleActions(newNinjaWriter(bytes.NewBuffer(nil)), true, "test.ninja"); err != nil {
			t.Fatalf("failed to write module Ninja shards: %v", err)
		}
	}

	writeShards()
	if got := countingFs.openWriteCount(); got == 0 {
		t.Fatal("initial generation did not create any module Ninja shards")
	}

	countingFs.resetOpenWrites()
	writeShards()
	if got := countingFs.openWriteCount(); got != 0 {
		t.Fatalf("identical module shard generation opened %d files for writing, want 0", got)
	}

	module := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()
	if len(module.actionDefs.buildDefs) == 0 {
		t.Fatal("MyBarModule has no build definition to mutate")
	}
	module.actionDefs.buildDefs[0].Comment = "content changed test"

	countingFs.resetOpenWrites()
	writeShards()
	if got := countingFs.openWriteCount(); got != 1 {
		t.Fatalf("one changed module action opened %d files for writing, want exactly its one shard", got)
	}
}

func TestModuleNinjaShardCacheCreatedByFreshBuildActionsIsReusedAfterRestore(t *testing.T) {
	first := incrementalSetup(t)
	first.SetIncrementalEnabled(true)
	first.SetIncrementalAnalysis(false)
	if _, errs := first.PrepareBuildActions(nil); len(errs) > 0 {
		t.Fatalf("fresh PrepareBuildActions failed: %v", errs)
	}
	if err := first.writeAllModuleActions(newNinjaWriter(bytes.NewBuffer(nil)), true, "test.ninja"); err != nil {
		t.Fatalf("write fresh module shards: %v", err)
	}
	first.keyValueStoreCache.flush()

	second := incrementalSetup(t)
	second.keyValueStoreCache = first.keyValueStoreCache
	second.fs = first.fs
	second.orderOnlyStringsCache = first.orderOnlyStringsCache
	second.SetIncrementalEnabled(true)
	second.SetIncrementalAnalysis(true)
	if _, errs := second.PrepareBuildActions(nil); len(errs) > 0 {
		t.Fatalf("incremental PrepareBuildActions failed: %v", errs)
	}
	globOwner := second.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	if !globOwner.incrementalRestored || len(globOwner.globCache) == 0 {
		t.Fatalf("glob owner did not restore its cached glob results: restored=%t globs=%v", globOwner.incrementalRestored, globOwner.globCache)
	}

	countingFs := &countingOpenFileSystem{FileSystem: second.fs}
	second.fs = countingFs
	if err := second.writeAllModuleActions(newNinjaWriter(bytes.NewBuffer(nil)), true, "test.ninja"); err != nil {
		t.Fatalf("write restored module shards: %v", err)
	}
	if got := countingFs.openWriteCount(); got != 0 {
		t.Fatalf("unchanged restored graph rewrote %d shard files after a fresh baseline", got)
	}
}

func bpSetup(t *testing.T, bp string) *Context {
	ctx := NewContext()
	fileSystem := map[string][]byte{
		"Android.bp": []byte(bp),
		"file1.cc":   {},
		"file1.cpp":  {},
		"file2.cc":   {},
		"file2.cpp":  {},
	}
	ctx.MockFileSystem(fileSystem)
	ctx.RegisterBottomUpMutator("deps", depsMutator)
	ctx.RegisterModuleType("incremental_module", newIncrementalModule)
	ctx.RegisterModuleType("incremental_transitive_module", newIncrementalTransitiveModule)
	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)
	ctx.RegisterModuleType("glob_owner_module", newGlobOwnerModule)

	_, errs := ctx.ParseBlueprintsFiles("Android.bp", nil)
	if len(errs) > 0 {
		t.Errorf("unexpected parse errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	_, errs = ctx.ResolveDependencies(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected dep errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	return ctx
}

type globOwnerModule struct {
	baseTestModule
}

func newGlobOwnerModule() (Module, []interface{}) {
	m := &globOwnerModule{}
	return m, []interface{}{&m.baseTestModule.properties, &m.SimpleName.Properties}
}

func (m *globOwnerModule) GenerateBuildActions(ctx ModuleContext) {
	m.GenerateBuildActionsCalled = true
	sources, err := ctx.GlobWithDeps(m.properties.Srcs[0], m.properties.Exclude_srcs)
	if err != nil {
		ctx.ModuleErrorf("glob failed: %s", err)
		return
	}
	SetProvider(ctx, IncrementalTestProviderKey, IncrementalTestInfo{Value: strings.Join(sources, ",")})
	ctx.Build(pctx, BuildParams{Rule: Phony, Outputs: m.properties.Outputs})
}

type incrementalModule struct {
	baseTestModule
}

type moduleActionCacheOptOutTestModule struct {
	*incrementalModule
}

func (*moduleActionCacheOptOutTestModule) DisableModuleActionCache() {}

const incrementalModuleNinja string = `# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #
# Module:  MyIncrementalModule
# Variant:
# Type:    incremental_module
# Factory: github.com/google/blueprint.newIncrementalModule
# Defined: Android.bp:2:4

build MyIncrementalModule_phony_output: phony || dedup-d479e9a8133ff998
    tags = module_name=MyIncrementalModule;module_type=incremental_module;rule_name=phony
`

func newIncrementalModule() (Module, []interface{}) {
	m := &incrementalModule{}
	return m, []interface{}{&m.baseTestModule.properties, &m.SimpleName.Properties}
}

type incrementalTransitiveModule struct {
	ModuleBase
	SimpleName
	ModuleUsesIncrementalWalkDeps
	properties struct {
		Deps []string
	}

	visited []string

	generateBuildActionsCalled bool
}

func newIncrementalTransitiveModule() (Module, []interface{}) {
	m := &incrementalTransitiveModule{}
	return m, []interface{}{&m.SimpleName.Properties, &m.properties}
}

func (t *incrementalTransitiveModule) GenerateBuildActions(ctx ModuleContext) {
	ctx.WalkDepsProxy(func(child ModuleProxy, parent ModuleProxy) bool {
		t.visited = append(t.visited, ctx.OtherModuleName(child))
		return true
	})
	t.generateBuildActionsCalled = true
}

func (t *incrementalTransitiveModule) Deps() []string {
	return t.properties.Deps
}

func (t *incrementalTransitiveModule) IgnoreDeps() []string {
	return nil
}

func incrementalSetup(t *testing.T) *Context {
	bp := `
			incremental_module {
					name: "MyIncrementalModule",
					deps: ["MyBarModule"],
					outputs: ["MyIncrementalModule_phony_output"],
					order_only: ["test.lib"],
					srcs: [
							"*.cc",
							"*.cpp",
					],
					exclude_srcs: [
							"file1.cc",
							"file1.cpp",
					],
			}
			bar_module {
					name: "MyBarModule",
					outputs: ["MyBarModule_phony_output"],
					order_only: ["test.lib"],
			}
			foo_module {
					name: "MyFooModule",
					outputs: ["MyFooModule_phony_output"],
					order_only: ["test.lib"],
					deps: ["MyIncrementalModule"],
			}
		`

	ctx := bpSetup(t, bp)

	cache := &KeyValueStoreCache{}
	err := cache.openForTests()
	if err != nil {
		t.Fatalf("failed to open cache: %s", err)
	}
	ctx.keyValueStoreCache = cache

	return ctx
}

func incrementalSetupForRestore(ctx *Context, orderOnlyStrings []string) any {
	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	barInfo := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()

	providerHashes := make([]proptools.Hash, len(providerRegistry))
	// Use fixed value since SetProvider hasn't been called yet, so we can't go
	// through the providers of the module.
	for k, v := range map[providerKey]any{
		IncrementalTestProviderKey.providerKey: IncrementalTestInfo{
			Value: barInfo.Name(),
		},
	} {
		hash, err := proptools.CalculateHashReflection(v)
		if err != nil {
			panic("Can't hash value of providers")
		}
		providerHashes[k.id] = hash
	}
	hash, err := proptools.CalculateHash(hashList(providerHashes))
	if err != nil {
		panic(err)
	}

	cacheKey, hash := calculateHashKey(ctx, incInfo, []proptools.Hash{hash})
	var providerValue any = IncrementalTestInfo{Value: "MyIncrementalModule"}
	providerHash, _ := proptools.CalculateHashReflection(providerValue)
	ctx.keyValueStoreCache.writeModuleBuildAction(ctx.EncContext, &cacheKey, &ModuleActionCachedData{
		InputHash: hash,
		ProviderHashes: []ProviderHash{{
			Id:   &IncrementalTestProviderKey.providerKey,
			Hash: providerHash,
		}},
		OrderOnlyStrings: orderOnlyStrings,
		GlobCache:        calculateGlobCache(),
	})
	ctx.keyValueStoreCache.writeProvider(ctx.EncContext, providerHash, CachedProvider{
		Id:    &IncrementalTestProviderKey.providerKey,
		Value: providerValue,
	})
	ctx.keyValueStoreCache.writeNinjaStatements(&cacheKey, []byte(incrementalModuleNinja))

	ctx.keyValueStoreCache.flush()

	ctx.SetIncrementalEnabled(true)
	ctx.SetIncrementalAnalysis(true)

	return providerValue
}

func calculateHashKey(ctx *Context, m *moduleInfo, providerHashes []proptools.Hash) (DataCacheKey, proptools.Hash) {
	hash, err := proptools.CalculateHashReflection(m.properties)
	if err != nil {
		panic(newPanicErrorf(err, "failed to calculate properties hash"))
	}
	cacheInput := new(ModuleBuildActionCacheInput)
	cacheInput.PropertiesHash = hash
	cacheInput.DepProviderHashes = providerHashes
	hash, err = proptools.CalculateHash(cacheInput)
	if err != nil {
		panic(newPanicErrorf(err, "failed to calculate cache input hash"))
	}
	m.cachedUniqueName = ctx.nameInterface.UniqueName(newNamespaceContext(m), m.group.name)
	return DataCacheKey{
		Id: m.moduleCacheKey(),
	}, hash
}

func calculateGlobCache() []globResultCache {
	globHash1, _ := proptools.CalculateHash(stringList{"file2.cc"})
	globHash2, _ := proptools.CalculateHash(stringList{"file2.cpp"})

	return []globResultCache{
		{
			Pattern:  "*.cc",
			Excludes: []string{"file1.cc", "file1.cpp"},
			Result:   globHash1,
		},
		{
			Pattern:  "*.cpp",
			Excludes: []string{"file1.cc", "file1.cpp"},
			Result:   globHash2,
		},
	}
}

func TestCacheBuildActions(t *testing.T) {
	ctx := incrementalSetup(t)
	ctx.SetIncrementalEnabled(true)

	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")

	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	barInfo := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()

	ctx.keyValueStoreCache.flush()

	cacheKey, hash := calculateHashKey(ctx, incInfo, []proptools.Hash{barInfo.providersHash})
	cache, err := ctx.keyValueStoreCache.readModuleBuildAction(ctx.EncContext, &cacheKey)
	if err != nil {
		t.Fatalf("read failed with an error: %s", err)
	}
	if cache == nil {
		t.Errorf("failed to find cached build actions for the incremental module")
	}
	var providerValue any = IncrementalTestInfo{Value: "MyIncrementalModule"}
	providerHash, _ := proptools.CalculateHashReflection(providerValue)
	expectedCache := ModuleActionCachedData{
		InputHash: hash,
		ProviderHashes: []ProviderHash{{
			Id:   &IncrementalTestProviderKey.providerKey,
			Hash: providerHash,
		}},
		OrderOnlyStrings: []string{"dedup-d479e9a8133ff998"},
		GlobCache:        calculateGlobCache(),
	}
	if !reflect.DeepEqual(expectedCache, *cache) {
		t.Errorf("expected: %v actual %v", expectedCache, *cache)
	}

	provider, err := ctx.keyValueStoreCache.readProvider(ctx.EncContext, providerHash, &IncrementalTestProviderKey.providerKey)
	if err != nil {
		t.Fatalf("read failed with an error: %s", err)
	}
	if *provider.Id != IncrementalTestProviderKey.providerKey {
		t.Errorf("expected restored id: %v actual %v", IncrementalTestProviderKey.providerKey, *provider.Id)
	}
	if !reflect.DeepEqual(provider.Value, providerValue) {
		t.Errorf("expected: %v actual %v", providerValue, provider.Value)
	}

	ninja, err := ctx.keyValueStoreCache.readNinjaStatements(&cacheKey)
	if err != nil {
		t.Fatalf("read failed with an error: %s", err)
	}
	ninjaStr := string(ninja)
	if !strings.Contains(ninjaStr, incrementalModuleNinja) {
		t.Errorf("expected: %v actual %v", incrementalModuleNinja, ninjaStr)
	}
}

func TestRestoreBuildActions(t *testing.T) {
	ctx := incrementalSetup(t)
	providerValue := incrementalSetupForRestore(ctx, nil)
	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	barInfo := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	// Verify that the GenerateBuildActions was skipped for the incremental module
	incRerun := incInfo.logicModule.(*incrementalModule).GenerateBuildActionsCalled
	barRerun := barInfo.logicModule.(*barModule).GenerateBuildActionsCalled
	if incRerun || !barRerun {
		t.Errorf("failed to skip/rerun GenerateBuildActions: %t %t", incRerun, barRerun)
	}
	// Verify that the provider is set correctly for the incremental module
	if !reflect.DeepEqual(incInfo.providers[IncrementalTestProviderKey.id], providerValue) {
		t.Errorf("provider is not set correctly when restoring from cache")
	}
}

func TestModuleActionCacheOptOutRegeneratesBuildActions(t *testing.T) {
	ctx := incrementalSetup(t)
	incrementalSetupForRestore(ctx, nil)
	ctx.SkipCloneModulesAfterMutators = true
	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	incModule := incInfo.logicModule.(*incrementalModule)
	incInfo.logicModule = &moduleActionCacheOptOutTestModule{incrementalModule: incModule}

	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors calling generateModuleBuildActions: %v", errs)
	}

	if incInfo.incrementalRestored {
		t.Fatal("module with action-cache opt-out was restored")
	}
	if incInfo.buildActionCacheKey != nil {
		t.Fatal("module with action-cache opt-out got a cache key")
	}
	if !incInfo.logicModule.(*moduleActionCacheOptOutTestModule).GenerateBuildActionsCalled {
		t.Fatal("module with action-cache opt-out did not regenerate build actions")
	}
}

func TestGlobChangeRestoreBuildActions(t *testing.T) {
	ctx := incrementalSetup(t)
	incrementalSetupForRestore(ctx, nil)
	// Now change the file system to make the old glob result invalid.
	fileSystem := map[string][]byte{
		"Android.bp": {},
		"file1.cc":   {},
		"file1.cpp":  {},
		"file3.cc":   {},
		"file4.cpp":  {},
	}
	ctx.MockFileSystem(fileSystem)
	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	// Verify that the GenerateBuildActions was rerun for the incremental module
	incRerun := incInfo.logicModule.(*incrementalModule).GenerateBuildActionsCalled
	if !incRerun {
		t.Errorf("failed to rerun GenerateBuildActions when glob result changed: %t", incRerun)
	}
}

func TestIncrementalGlobChangeRestoresOnlyDependencyClosure(t *testing.T) {
	bp := `
		glob_owner_module {
			name: "GlobOwner",
			srcs: ["src/*.java"],
			outputs: ["glob_owner"],
		}
		foo_module {
			name: "Dependent",
			deps: ["GlobOwner"],
			outputs: ["dependent"],
		}
		foo_module {
			name: "Unrelated",
			outputs: ["unrelated"],
		}
	`
	fileSystem := func(sources ...string) map[string][]byte {
		files := map[string][]byte{"Android.bp": []byte(bp)}
		for _, source := range sources {
			files[source] = []byte{}
		}
		return files
	}

	cache := &KeyValueStoreCache{}
	if err := cache.openForTests(); err != nil {
		t.Fatalf("failed to open cache: %v", err)
	}
	first := bpSetup(t, bp)
	first.MockFileSystem(fileSystem("src/A.java"))
	first.keyValueStoreCache = cache
	first.SetIncrementalEnabled(true)
	if _, errs := first.PrepareBuildActions(nil); len(errs) != 0 {
		t.Fatalf("first PrepareBuildActions failed: %v", errs)
	}
	firstOwner := first.moduleGroupFromName("GlobOwner", nil).modules.firstModule()
	baselineFingerprint, ok := first.incrementalNinjaShardFingerprint([]*moduleInfo{firstOwner}, nil)
	if !ok {
		t.Fatal("fresh glob owner did not produce a shard fingerprint")
	}
	if err := first.writeAllModuleActions(newNinjaWriter(bytes.NewBuffer(nil)), true, "test.ninja"); err != nil {
		t.Fatalf("write first build actions: %v", err)
	}
	cache.flush()

	second := bpSetup(t, bp)
	second.MockFileSystem(fileSystem("src/A.java", "src/B.java"))
	second.keyValueStoreCache = cache
	second.SetIncrementalEnabled(true)
	second.SetIncrementalAnalysis(true)
	debugFile := filepath.Join(t.TempDir(), "incremental-misses.json")
	second.SetIncrementalDebugFile(debugFile)
	second.SetIncrementalDebugMissesOnly(true)
	if _, errs := second.PrepareBuildActions(nil); len(errs) != 0 {
		t.Fatalf("incremental PrepareBuildActions failed: %v", errs)
	}
	secondOwner := second.moduleGroupFromName("GlobOwner", nil).modules.firstModule()
	changedFingerprint, ok := second.incrementalNinjaShardFingerprint([]*moduleInfo{secondOwner}, nil)
	if !ok {
		t.Fatal("changed glob owner did not produce a shard fingerprint")
	}
	if changedFingerprint == baselineFingerprint {
		t.Fatal("glob result change did not invalidate its Ninja shard fingerprint")
	}

	for _, test := range []struct {
		name string
		want bool
	}{
		{name: "GlobOwner", want: false},
		{name: "Dependent", want: false},
		{name: "Unrelated", want: true},
	} {
		module := second.moduleGroupFromName(test.name, nil).modules.firstModule()
		if module.incrementalRestored != test.want {
			t.Errorf("module %s restored = %t, want %t", test.name, module.incrementalRestored, test.want)
		}
	}
	if err := second.writeAllModuleActions(newNinjaWriter(bytes.NewBuffer(nil)), true, "test.ninja"); err != nil {
		t.Fatalf("write incremental build actions: %v", err)
	}
	debugJSON, err := os.ReadFile(debugFile)
	if err != nil {
		t.Fatalf("read incremental miss report: %v", err)
	}
	var report struct {
		Modules []struct {
			Name          string `json:"name"`
			RestoreReason string `json:"restore_reason"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(debugJSON, &report); err != nil {
		t.Fatalf("decode incremental miss report: %v", err)
	}
	if len(report.Modules) != 2 {
		t.Fatalf("incremental miss report has %d modules, want only the 2-module dependency closure: %s", len(report.Modules), debugJSON)
	}
	misses := make(map[string]string, len(report.Modules))
	for _, module := range report.Modules {
		misses[module.Name] = module.RestoreReason
	}
	if misses["GlobOwner"] != "glob_result_changed: src/*.java" || misses["Dependent"] != "module_or_dependency_inputs_changed" {
		t.Fatalf("unexpected incremental misses: %+v", misses)
	}
}

func TestSkipNinjaForCacheHit(t *testing.T) {
	ctx := incrementalSetup(t)
	incrementalSetupForRestore(ctx, nil)
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")
	// Verify that soong updated the ninja file for the bar module and skipped the
	// Ninja file writing of the incremental module. Stable sharding means the
	// modules are no longer tied to fixed numeric shard indexes.
	content := readNinjaShardContents(t, ctx, "test.ninja")
	if !strings.Contains(content, "build MyBarModule_phony_output: phony") {
		t.Errorf("ninja shards don't have build statements for MyBarModule")
	}
	if !strings.Contains(content, incrementalModuleNinja) {
		t.Errorf("ninja shards don't have restored build statements for MyIncrementalModule")
	}
}

func TestNotSkipNinjaForCacheMiss(t *testing.T) {
	ctx := incrementalSetup(t)
	ctx.SetIncrementalEnabled(true)
	ctx.SetIncrementalAnalysis(true)
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")
	// Verify that soong updated the ninja files for both the bar module and the
	// incremental module
	content := readNinjaShardContents(t, ctx, "test.ninja")
	if !strings.Contains(content, "build MyBarModule_phony_output: phony") {
		t.Errorf("ninja shards don't have build statements for MyBarModule")
	}
	if !strings.Contains(content, "build MyIncrementalModule_phony_output: phony") {
		t.Errorf("ninja shards don't have build statements for MyIncrementalModule")
	}
}

func readNinjaShardContents(t *testing.T, ctx *Context, ninjaFile string) string {
	t.Helper()
	var content strings.Builder
	for _, file := range GetNinjaShardFiles(ninjaFile) {
		shard, err := ctx.fs.Open(file)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(shard)
		closeErr := shard.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		content.Write(data)
	}
	return content.String()
}

func TestOrderOnlyStringsCaching(t *testing.T) {
	phony := "dedup-d479e9a8133ff998"
	ctx := incrementalSetup(t)
	ctx.SetIncrementalEnabled(true)
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}
	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	barInfo := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")

	ctx.keyValueStoreCache.flush()

	verifyOrderOnlyStringsCache(t, ctx, incInfo, barInfo)

	// Verify the shared dedup phony is written exactly once across Ninja shards.
	expected := strings.Join([]string{"build", phony + ":", "phony", "test.lib"}, " ")
	shardContent := readNinjaShardContents(t, ctx, "test.ninja")
	if strings.Count(shardContent, expected) != 1 {
		t.Errorf("only one phony target should be found: %s", shardContent)
	}
}

func TestOrderOnlyStringsRestoring(t *testing.T) {
	phony := "dedup-d479e9a8133ff998"
	orderOnlyStrings := []string{phony}
	ctx := incrementalSetup(t)
	incrementalSetupForRestore(ctx, orderOnlyStrings)
	ctx.orderOnlyStringsCache = make(OrderOnlyStringsCache)
	ctx.orderOnlyStringsCache[phony] = []string{"test.lib"}
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	barInfo := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")

	ctx.keyValueStoreCache.flush()

	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	verifyOrderOnlyStringsCache(t, ctx, incInfo, barInfo)

	verifyBuildDefsShouldContain(t, barInfo, phony)
	// Verify dedup-d479e9a8133ff998 is written to a Ninja shard.
	expected := strings.Join([]string{"build", phony + ":", "phony", "test.lib"}, " ")
	shardContent := readNinjaShardContents(t, ctx, "test.ninja")
	if strings.Count(shardContent, expected) != 1 {
		t.Errorf("only one phony target should be found: %s", shardContent)
	}

	if len(ctx.orderOnlyStringsCache) != 1 {
		t.Errorf("Phony target should be cached: %s", buf.String())
	}
}

func TestOrderOnlyStringsValidWhenOnlyRestoredModuleUseIt(t *testing.T) {
	phony := "dedup-d479e9a8133ff998"
	orderOnlyStrings := []string{phony}
	bp := `
			incremental_module {
					name: "MyIncrementalModule",
					deps: ["MyBarModule"],
					outputs: ["MyIncrementalModule_phony_output"],
					order_only: ["test.lib"],
			}
			bar_module {
					name: "MyBarModule",
					outputs: ["MyBarModule_phony_output"],
			}
		`

	ctx := bpSetup(t, bp)
	cache := &KeyValueStoreCache{}
	err := cache.openForTests()
	if err != nil {
		t.Fatalf("failed to open cache: %s", err)
	}
	ctx.keyValueStoreCache = cache
	incrementalSetupForRestore(ctx, orderOnlyStrings)
	ctx.orderOnlyStringsCache = make(OrderOnlyStringsCache)
	ctx.orderOnlyStringsCache[phony] = []string{"test.lib"}
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	barInfo := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")

	ctx.keyValueStoreCache.flush()

	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	verifyOrderOnlyStringsCache(t, ctx, incInfo, barInfo)

	// Verify dedup-d479e9a8133ff998 is still written to a Ninja shard even
	// though MyBarModule no longer uses it.
	expected := strings.Join([]string{"build", phony + ":", "phony", "test.lib"}, " ")
	shardContent := readNinjaShardContents(t, ctx, "test.ninja")
	if strings.Count(shardContent, expected) != 1 {
		t.Errorf("only one phony target should be found: %s", shardContent)
	}

	if len(ctx.orderOnlyStringsCache) != 1 {
		t.Errorf("Phony target should be cached: %s", buf.String())
	}
}

func TestCachedModuleRemoved(t *testing.T) {
	phony := "dedup-d479e9a8133ff998"
	orderOnlyStrings := []string{phony}
	ctx := incrementalSetup(t)
	incrementalSetupForRestore(ctx, orderOnlyStrings)
	bp := `
			bar_module {
					name: "MyBarModule",
					outputs: ["MyBarModule_phony_output"],
					order_only: ["test.lib"],
			}
		`
	ctx = bpSetup(t, bp)
	ctx.orderOnlyStringsCache = make(OrderOnlyStringsCache)
	ctx.orderOnlyStringsCache[phony] = []string{"test.lib"}
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")

	// Verify dedup-d479e9a8133ff998 is no longer written to any Ninja shard
	// because MyIncrementalModule was removed so only MyBarModule still use it.
	expected := strings.Join([]string{"build", phony + ":", "phony", "test.lib"}, " ")
	shardContent := readNinjaShardContents(t, ctx, "test.ninja")
	if strings.Count(shardContent, expected) != 0 {
		t.Errorf("Phony target should not be present in any Ninja shard: %s", shardContent)
	}
}

// This tests the scenario where one restored module and two non-restored modules
// share the same set of order only strings. The two non-restored modules will
// contribute a dedup phony target in this case, and the restored module shouldn't
// add a duplicate one.
func TestSharedOrderOnlyStringsRestoringNoDuplicates(t *testing.T) {
	phony := "dedup-d479e9a8133ff998"
	orderOnlyStrings := []string{phony}
	ctx := incrementalSetup(t)
	incrementalSetupForRestore(ctx, orderOnlyStrings)
	ctx.orderOnlyStringsCache = make(OrderOnlyStringsCache)
	ctx.orderOnlyStringsCache[phony] = []string{"test.lib"}

	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}
	incInfo := ctx.moduleGroupFromName("MyIncrementalModule", nil).modules.firstModule()
	fooInfo := ctx.moduleGroupFromName("MyFooModule", nil).modules.firstModule()
	barInfo := ctx.moduleGroupFromName("MyBarModule", nil).modules.firstModule()

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")

	ctx.keyValueStoreCache.flush()

	verifyOrderOnlyStringsCache(t, ctx, incInfo, barInfo)
	verifyBuildDefsShouldContain(t, fooInfo, phony)
	verifyBuildDefsShouldContain(t, barInfo, phony)

	// Verify dedup-d479e9a8133ff998 is written to a Ninja shard.
	expected := strings.Join([]string{"build", phony + ":", "phony", "test.lib"}, " ")
	shardContent := readNinjaShardContents(t, ctx, "test.ninja")
	if strings.Count(shardContent, expected) != 1 {
		t.Errorf("only one phony target should be found: %s", shardContent)
	}

	if len(ctx.orderOnlyStringsCache) != 1 {
		t.Errorf("Phony target should be cached: %s", buf.String())
	}
}

func verifyBuildDefsShouldContain(t *testing.T, module *moduleInfo, expected string) {
	found := false
	for _, def := range module.actionDefs.buildDefs {
		found = listContainsValue(def.OrderOnlyStrings.ToSlice(), expected)
		if found {
			break
		}
	}
	if !found {
		t.Errorf("%s should have dedup phony target: %v", module.Name(), module.actionDefs.buildDefs)
	}
}

func verifyOrderOnlyStringsCache(t *testing.T, ctx *Context, incInfo, barInfo *moduleInfo) {
	// Verify that soong cache all the order only strings that are used by the
	// incremental modules
	ok, key := mapContainsValue(ctx.orderOnlyStringsCache, "test.lib")
	if !ok {
		t.Errorf("no order only strings used by incremetnal modules cached: %v", ctx.orderOnlyStringsCache)
	}

	// Verify that the dedup-* order only strings used by MyIncrementalModule is
	// cached along with its other cached values
	cacheKey, _ := calculateHashKey(ctx, incInfo, []proptools.Hash{barInfo.providersHash})
	cache, err := ctx.keyValueStoreCache.readModuleBuildAction(ctx.EncContext, &cacheKey)
	if err != nil {
		t.Fatalf("read failed with an error: %s", err)
	}
	if cache == nil {
		t.Errorf("failed to find cached build actions for the incremental module")
	}
	if !listContainsValue(cache.OrderOnlyStrings, key) {
		t.Errorf("no order only strings cached for MyIncrementalModule: %v", cache.OrderOnlyStrings)
	}
}

func listContainsValue[K comparable](l []K, target K) bool {
	for _, value := range l {
		if value == target {
			return true
		}
	}
	return false
}

func mapContainsValue[K comparable, V comparable](m map[K][]V, target V) (bool, K) {
	for k, v := range m {
		if listContainsValue(v, target) {
			return true, k
		}
	}
	var key K
	return false, key
}

var singletonTestInfoProvider = NewSingletonProvider[IncrementalTestInfo]()

const targetedSingletonName = "targeted_singleton"

type targetedSingleton struct {
	GenerateBuildActionsCalled int
}

func (s *targetedSingleton) GenerateBuildActions(ctx SingletonContext) {
	s.GenerateBuildActionsCalled++
	ctx.VisitAllModuleProxies(func(module ModuleProxy) {
		if ctx.ModuleName(module) == "MyFooModule" {
			ctx.ModuleProvider(module, IncrementalTestProviderKey)
		}
	})
	ctx.Build(pctx, BuildParams{Rule: Phony, Outputs: []string{targetedSingletonName}})
}

func (*targetedSingleton) IncrementalSupported() bool { return true }

func targetedSingletonFactory() Singleton { return &targetedSingleton{} }

type sequentialSingleton struct {
	GenerateBuildActionsCalled int
}

func (s *sequentialSingleton) GenerateBuildActions(ctx SingletonContext) {
	s.GenerateBuildActionsCalled++
	ctx.Build(pctx, BuildParams{
		Rule:    Phony,
		Outputs: []string{sequentialSingletonName},
	})
	ctx.VisitAllSingletons(func(singleton SingletonProxy) {
		ctx.OtherSingletonProvider(singleton, singletonTestInfoProvider)
	})
	ctx.VisitAllModules(func(module Module) {
		ctx.ModuleProvider(module, IncrementalTestProviderKey)
	})
	ctx.SetSingletonProvider(singletonTestInfoProvider, IncrementalTestInfo{Value: sequentialSingletonName})
}

func (s *sequentialSingleton) IncrementalSupported() bool {
	return true
}

func sequentialSingletonFactory() Singleton {
	return &sequentialSingleton{}
}

type parallelSingleton struct {
	GenerateBuildActionsCalled int
}

func (s *parallelSingleton) GenerateBuildActions(ctx SingletonContext) {
	s.GenerateBuildActionsCalled++
	var values []string
	ctx.VisitAllModuleProxies(func(module ModuleProxy) {
		if info, ok := ctx.ModuleProvider(module, IncrementalTestProviderKey); ok {
			values = append(values, info.(IncrementalTestInfo).Value)
		}
	})
	ctx.SetSingletonProvider(singletonTestInfoProvider, IncrementalTestInfo{Value: strings.Join(values, ",")})
}

func parallelSingletonFactory() Singleton {
	return &parallelSingleton{}
}

func (s *parallelSingleton) IncrementalSupported() bool {
	return true
}

type noProviderParallelSingleton struct {
	GenerateBuildActionsCalled int
}

func (s *noProviderParallelSingleton) GenerateBuildActions(ctx SingletonContext) {
	s.GenerateBuildActionsCalled++
}

func noProviderParallelSingletonFactory() Singleton {
	return &noProviderParallelSingleton{}
}

func (s *noProviderParallelSingleton) IncrementalSupported() bool {
	return true
}

const noProviderParallelSingletonName = "no_provider_parallel_singleton"
const parallelSingletonName = "parallel_singleton"
const sequentialSingletonName = "sequential_singleton"

func singletonCacheSetup(t *testing.T, modifiers ...func(bp string) string) *Context {
	bp := `
			foo_module {
					name: "MyFooModule",
					outputs: ["MyFooModule_phony_output"],
			}
		`

	for _, m := range modifiers {
		bp = m(bp)
	}

	ctx := bpSetup(t, bp)
	ctx.RegisterSingletonType(parallelSingletonName, parallelSingletonFactory, true)
	ctx.RegisterSingletonType(noProviderParallelSingletonName, noProviderParallelSingletonFactory, true)
	ctx.RegisterSingletonType(sequentialSingletonName, sequentialSingletonFactory, false)
	ctx.RegisterSingletonType(targetedSingletonName, targetedSingletonFactory, true)

	cache := &KeyValueStoreCache{}
	if err := cache.openForTests(); err != nil {
		t.Fatalf("failed to open cache: %s", err)
	}
	ctx.keyValueStoreCache = cache

	ctx.SetIncrementalEnabled(true)
	ctx.SetIncrementalAnalysis(true)
	return ctx
}

func TestSingletonCache(t *testing.T) {
	ctx := singletonCacheSetup(t)

	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	seqSingleton := ctx.singletonByName(sequentialSingletonName).singleton.(*sequentialSingleton)

	// 1. Verify GenerateBuildActions was called
	if seqSingleton.GenerateBuildActionsCalled != 1 {
		t.Errorf("expected GenerateBuildActions to be called once, got %d", sequentialSingleton{}.GenerateBuildActionsCalled)
	}

	ctx.keyValueStoreCache.flush()

	// 2. Verify cache entry was written
	seqCacheKey := &DataCacheKey{Id: sequentialSingletonName}
	data, err := ctx.keyValueStoreCache.readSingletonBuildAction(ctx.EncContext, seqCacheKey)
	if err != nil {
		t.Fatalf("failed to read cache: %v", err)
	}
	if data == nil || data.CacheVersion != singletonActionCacheVersion || len(data.ModuleDependencyBloom) == 0 || len(data.ModuleProviderDependencyBitsets) == 0 || len(data.SingletonProviderDependencies) == 0 {
		t.Errorf("expected cache entry to contain tracked module and singleton dependencies, got %#v", data)
	}

	// 3. Verify providers were cached
	seqSingletonProviderHash := ctx.singletonByName(sequentialSingletonName).providerInitialValueHashes[singletonTestInfoProvider.providerKey.id]

	provider, err := ctx.keyValueStoreCache.readProvider(ctx.EncContext, seqSingletonProviderHash, &singletonTestInfoProvider.providerKey)
	if err != nil {
		t.Fatalf("read failed with an error: %s", err)
	}
	if *provider.Id != singletonTestInfoProvider.providerKey {
		t.Errorf("expected restored id: %v actual %v", IncrementalTestProviderKey.providerKey, *provider.Id)
	}
	var providerValue any = IncrementalTestInfo{Value: sequentialSingletonName}
	if !reflect.DeepEqual(provider.Value, providerValue) {
		t.Errorf("expected: %v actual %v", providerValue, provider.Value)
	}

	// 4. Verify ninja statement was cached
	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	if err := ctx.writeAllSingletonActions(w); err != nil {
		t.Fatalf("failed to write all singleton actions: %v", err)
	}
	ninja, err := ctx.keyValueStoreCache.readNinjaStatements(seqCacheKey)
	if err != nil {
		t.Fatalf("failed to read cache: %v", err)
	}
	if !strings.Contains(string(ninja), "build sequential_singleton: phony") {
		t.Errorf("expected ninja statement to be cached")
	}

	// 5. Verify ninja file was written
	if !strings.Contains(buf.String(), "build sequential_singleton: phony") {
		t.Errorf("ninja file doesn't have build statements for singleton: %s", buf.String())
	}
}

func TestSingletonActionsUseSubninjaShards(t *testing.T) {
	ctx := singletonCacheSetup(t)
	if _, errs := ctx.PrepareBuildActions(nil); len(errs) > 0 {
		t.Fatalf("unexpected errors preparing build actions: %v", errs)
	}

	root := bytes.NewBuffer(nil)
	if err := ctx.writeAllSingletonActionsToShards(newNinjaWriter(root), "test.ninja"); err != nil {
		t.Fatalf("failed to write singleton subninja shards: %v", err)
	}
	if strings.Contains(root.String(), "build "+targetedSingletonName+": phony") {
		t.Fatal("singleton build actions were written to the root Ninja manifest")
	}
	if !strings.Contains(root.String(), "subninja test.singleton.") {
		t.Fatalf("root Ninja manifest does not include singleton shards: %s", root.String())
	}

	files := GetNinjaSingletonShardFiles("test.ninja")
	targetedCount := 0
	sequentialCount := 0
	var shardSummaries []string
	for _, file := range files {
		shard, err := ctx.fs.Open(filepath.FromSlash(file))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("failed to read singleton shard %q: %v", file, err)
		}
		data, readErr := io.ReadAll(shard)
		closeErr := shard.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("failed to read singleton shard %q: %v", file, errors.Join(readErr, closeErr))
		}
		shardSummaries = append(shardSummaries, fmt.Sprintf("%s=%d", file, len(data)))
		targetedCount += strings.Count(string(data), "build "+targetedSingletonName+": phony")
		sequentialCount += strings.Count(string(data), "build "+sequentialSingletonName+": phony")
	}
	if targetedCount != 1 || sequentialCount != 1 {
		t.Fatalf("singleton actions in shards: targeted=%d sequential=%d, want one each; root=%q shards=%v", targetedCount, sequentialCount, root.String(), shardSummaries)
	}
}

func TestSingletonRestore(t *testing.T) {
	ctx := singletonCacheSetup(t)
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	if err := ctx.writeAllModuleActions(w, true, "test.ninja"); err != nil {
		t.Fatalf("failed to write module actions: %v", err)
	}
	if err := ctx.writeAllSingletonActions(w); err != nil {
		t.Fatalf("failed to write all singleton actions: %v", err)
	}

	ctx.keyValueStoreCache.flush()

	// Now simulate an incremental build
	oldCache := ctx.keyValueStoreCache
	ctx = singletonCacheSetup(t)
	ctx.keyValueStoreCache = oldCache

	_, errs = ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	seqSingletonInfo := ctx.singletonByName(sequentialSingletonName)
	seqSingleton := seqSingletonInfo.singleton.(*sequentialSingleton)

	parallelSingletonInfo := ctx.singletonByName(parallelSingletonName)
	parallelSingleton := parallelSingletonInfo.singleton.(*parallelSingleton)

	noProviderSingletonInfo := ctx.singletonByName(noProviderParallelSingletonName)
	noProviderSingleton := noProviderSingletonInfo.singleton.(*noProviderParallelSingleton)

	// 1. Verify GenerateBuildActions was not called
	if seqSingleton.GenerateBuildActionsCalled != 0 {
		t.Errorf("expected sequentialSingleton GenerateBuildActions to be not called, got %d", seqSingleton.GenerateBuildActionsCalled)
	}

	if parallelSingleton.GenerateBuildActionsCalled != 0 {
		t.Errorf("expected parallelSingleton GenerateBuildActions to be not called, got %d", parallelSingleton.GenerateBuildActionsCalled)
	}

	if noProviderSingleton.GenerateBuildActionsCalled != 0 {
		t.Errorf("expected noProviderParallelSingleton GenerateBuildActions to be not called, got %d", noProviderSingleton.GenerateBuildActionsCalled)
	}

	// 2. Verify that the provider is set correctly for the singleton
	expected := IncrementalTestInfo{Value: sequentialSingletonName}
	actual, found := ctx.singletonProvider(seqSingletonInfo, singletonTestInfoProvider.provider())
	if !found || !reflect.DeepEqual(expected, actual) {
		t.Errorf("expected: %v actual %v", expected, actual)
	}

	// 3. Verify ninja statement was restored
	buf.Reset()
	if err := ctx.writeAllSingletonActions(w); err != nil {
		t.Fatalf("failed to write all singleton actions: %v", err)
	}
	if !strings.Contains(buf.String(), "build sequential_singleton: phony") {
		t.Errorf("ninja file doesn't have build statements for singleton: %s", buf.String())
	}
}

func TestSingletonCacheOnlyInvalidatesForAccessedModule(t *testing.T) {
	withUnrelatedModule := func(bp string) string {
		return bp + `
			foo_module {
				name: "UnrelatedModule",
				outputs: ["UnrelatedModule_old"],
			}
		`
	}
	changeUnrelatedOutput := func(bp string) string {
		return strings.Replace(bp, "UnrelatedModule_old", "UnrelatedModule_new", 1)
	}
	changeTargetOutput := func(bp string) string {
		return strings.Replace(bp, "MyFooModule_phony_output", "MyFooModule_changed_output", 1)
	}
	seed := func(modifiers ...func(string) string) *Context {
		ctx := singletonCacheSetup(t, modifiers...)
		if _, errs := ctx.PrepareBuildActions(nil); len(errs) > 0 {
			t.Fatalf("unexpected errors preparing initial build: %v", errs)
		}
		if err := ctx.writeAllModuleActions(newNinjaWriter(bytes.NewBuffer(nil)), true, "test.ninja"); err != nil {
			t.Fatalf("failed to write module actions: %v", err)
		}
		if err := ctx.writeAllSingletonActions(newNinjaWriter(bytes.NewBuffer(nil))); err != nil {
			t.Fatalf("failed to write singleton actions: %v", err)
		}
		ctx.keyValueStoreCache.flush()
		return ctx
	}
	t.Run("unrelated module provider change keeps cache hit", func(t *testing.T) {
		first := seed(withUnrelatedModule)
		oldCache := first.keyValueStoreCache
		ctx := singletonCacheSetup(t, withUnrelatedModule, changeUnrelatedOutput)
		ctx.keyValueStoreCache = oldCache
		if _, errs := ctx.PrepareBuildActions(nil); len(errs) > 0 {
			t.Fatalf("unexpected errors preparing incremental build: %v", errs)
		}
		if got := ctx.singletonByName(targetedSingletonName).singleton.(*targetedSingleton).GenerateBuildActionsCalled; got != 0 {
			t.Fatalf("targeted singleton reran after an unrelated module changed, calls=%d", got)
		}
	})
	t.Run("accessed module action change leaves unchanged provider cacheable", func(t *testing.T) {
		first := seed()
		oldCache := first.keyValueStoreCache
		ctx := singletonCacheSetup(t, changeTargetOutput)
		ctx.keyValueStoreCache = oldCache
		if _, errs := ctx.PrepareBuildActions(nil); len(errs) > 0 {
			t.Fatalf("unexpected errors preparing incremental build: %v", errs)
		}
		if got := ctx.singletonByName(targetedSingletonName).singleton.(*targetedSingleton).GenerateBuildActionsCalled; got != 0 {
			t.Fatalf("targeted singleton reran even though its provider value was unchanged, calls=%d", got)
		}
	})
}

func changeModuleName(from, to string) func(string) string {
	return func(bp string) string {
		m, err := bpmodify.NewBlueprint("Android.bp", []byte(bp))
		if err != nil {
			panic(err)
		}
		name, err := m.ModulesByName(from).GetProperty("name")
		if err != nil {
			panic(err)
		}
		err = name.SetString(to)
		if err != nil {
			panic(err)
		}
		return m.String()
	}
}

func TestSingletonNotRestoreForSingletonChange(t *testing.T) {
	ctx := singletonCacheSetup(t)
	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	if err := ctx.writeAllSingletonActions(w); err != nil {
		t.Fatalf("failed to write all singleton actions: %v", err)
	}

	ctx.keyValueStoreCache.flush()

	// Now simulate an incremental build
	oldCache := ctx.keyValueStoreCache
	ctx = singletonCacheSetup(t, changeModuleName("MyFooModule", "changed"))
	ctx.keyValueStoreCache = oldCache

	_, errs = ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	seqSingletonInfo := ctx.singletonByName(sequentialSingletonName)
	seqSingleton := seqSingletonInfo.singleton.(*sequentialSingleton)

	parallelSingletonInfo := ctx.singletonByName(parallelSingletonName)
	parallelSingleton := parallelSingletonInfo.singleton.(*parallelSingleton)

	noProviderSingletonInfo := ctx.singletonByName(noProviderParallelSingletonName)
	noProviderSingleton := noProviderSingletonInfo.singleton.(*noProviderParallelSingleton)

	// 1. Verify GenerateBuildActions was called
	if seqSingleton.GenerateBuildActionsCalled != 1 {
		t.Errorf("expected sequentialSingleton GenerateBuildActions to be called, got %d", seqSingleton.GenerateBuildActionsCalled)
	}

	if parallelSingleton.GenerateBuildActionsCalled != 1 {
		t.Errorf("expected parallelSingleton GenerateBuildActions to be called, got %d", parallelSingleton.GenerateBuildActionsCalled)
	}

	if noProviderSingleton.GenerateBuildActionsCalled != 0 {
		t.Errorf("expected noProviderParallelSingleton GenerateBuildActions to be not called, got %d", noProviderSingleton.GenerateBuildActionsCalled)
	}
}

func TestIncrementalTransitiveDependencies(t *testing.T) {
	incrementalSetup(t)
	bp := `
		incremental_transitive_module {
			name: "top",
			deps: ["a"],
		}

		foo_module {
			name: "a",
			outputs: ["a"],
			deps: ["b"],
		}

		foo_module {
			name: "b",
			outputs: ["b"],
		}

		foo_module {
			name: "c",
			outputs: ["c"],
		}
	`

	ctx := bpSetup(t, bp)

	cache := &KeyValueStoreCache{}
	err := cache.openForTests()
	if err != nil {
		t.Fatalf("failed to open cache: %s", err)
	}
	ctx.keyValueStoreCache = cache

	ctx.SetIncrementalEnabled(true)

	_, errs := ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	buf := bytes.NewBuffer(nil)
	w := newNinjaWriter(buf)
	ctx.writeAllModuleActions(w, true, "test.ninja")

	top := ctx.moduleGroupFromName("top", nil).modules.firstModule().logicModule.(*incrementalTransitiveModule)

	if !top.generateBuildActionsCalled {
		t.Fatalf("expected GenerateBuildActions called on top in first pass")
	}

	if g, w := top.visited, []string{"a", "b"}; !slices.Equal(g, w) {
		t.Fatalf("unexpected visited on first pass, expected %q got %q", w, g)
	}

	cache.flush()

	bp = addDepToModule(t, bp, "b", "c")

	ctx = bpSetup(t, bp)
	ctx.SetIncrementalEnabled(true)
	ctx.SetIncrementalAnalysis(true)
	ctx.keyValueStoreCache = cache

	_, errs = ctx.PrepareBuildActions(nil)
	if len(errs) > 0 {
		t.Errorf("unexpected errors calling generateModuleBuildActions:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	top = ctx.moduleGroupFromName("top", nil).modules.firstModule().logicModule.(*incrementalTransitiveModule)

	if !top.generateBuildActionsCalled {
		t.Fatalf("expected GenerateBuildActions called on top in second pass")
	}

	if g, w := top.visited, []string{"a", "b", "c"}; !slices.Equal(g, w) {
		t.Fatalf("unexpected visited on second pass, expected %q got %q", w, g)
	}
}

func addDepToModule(t *testing.T, bp, module, dep string) string {
	t.Helper()

	bpm, err := bpmodify.NewBlueprint("Android.bp", []byte(bp))
	if err != nil {
		t.Fatal(err)
	}
	p, err := bpm.ModulesByName(module).GetOrCreateProperty(bpmodify.List, "deps")
	if err != nil {
		t.Fatal(err)
	}

	err = p.AddStringToList(dep)
	if err != nil {
		t.Fatal(err)
	}

	return bpm.String()
}
