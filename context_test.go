// Copyright 2014 Google Inc. All rights reserved.
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
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/blueprint/gobtools"
	"github.com/google/blueprint/parser"
	"github.com/google/blueprint/proptools"
	"github.com/google/blueprint/uniquelist"
)

type Walker interface {
	Walk() bool
}

func walkDependencyGraph(ctx *Context, topModule *moduleInfo, allowDuplicates bool) (string, string) {
	var outputDown string
	var outputUp string
	ctx.walkDeps(topModule, allowDuplicates,
		func(dep depInfo, parent *moduleInfo) bool {
			outputDown += ctx.ModuleName(dep.module.logicModule)
			if tag, ok := dep.tag.(walkerDepsTag); ok {
				if !tag.follow {
					return false
				}
			}
			if dep.module.logicModule.(Walker).Walk() {
				return true
			}

			return false
		},
		func(dep depInfo, parent *moduleInfo) {
			outputUp += ctx.ModuleName(dep.module.logicModule)
		})
	return outputDown, outputUp
}

type depsProvider interface {
	Deps() []string
	IgnoreDeps() []string
}

type IncrementalTestInfo struct {
	Value string
}

var IncrementalTestProviderKey = NewProvider[IncrementalTestInfo]()

func init() {
	IncrementalTestProviderGobRegId = gobtools.RegisterType(func() gobtools.CustomDec { return new(IncrementalTestInfo) })
}

func (r IncrementalTestInfo) Encode(ctx gobtools.EncContext, buf *bytes.Buffer) error {
	var err error

	if err = gobtools.EncodeString(buf, r.Value); err != nil {
		return err
	}
	return err
}

func (r *IncrementalTestInfo) Decode(ctx gobtools.EncContext, buf *bytes.Reader) error {
	var err error

	err = gobtools.DecodeString(buf, &r.Value)
	if err != nil {
		return err
	}

	return err
}

func (r IncrementalTestInfo) CustomHash(hasher *proptools.Hasher) error {
	hasher.HashType(reflect.TypeOf(r))
	hasher.WriteString(r.Value)
	return nil
}

var IncrementalTestProviderGobRegId int16

func (r IncrementalTestInfo) GetTypeId() int16 {
	return IncrementalTestProviderGobRegId
}

type baseTestModule struct {
	ModuleBase
	SimpleName
	properties struct {
		Deps             []string
		Ignored_deps     []string
		Outputs          []string
		Order_only       []string
		Extra_outputs    []string
		Extra_order_only []string
		Srcs             []string
		Exclude_srcs     []string
	}
	GenerateBuildActionsCalled bool
}

func (b *baseTestModule) Deps() []string {
	return b.properties.Deps
}

func (b *baseTestModule) IgnoreDeps() []string {
	return b.properties.Ignored_deps
}

var pctx PackageContext

func init() {
	pctx = NewPackageContext("android/blueprint")
}
func (b *baseTestModule) GenerateBuildActions(ctx ModuleContext) {
	b.GenerateBuildActionsCalled = true
	ctx.Build(pctx, BuildParams{
		Rule:      Phony,
		Outputs:   b.properties.Outputs,
		OrderOnly: b.properties.Order_only,
	})
	if len(b.properties.Extra_outputs) > 0 {
		ctx.Build(pctx, BuildParams{
			Rule:      Phony,
			Outputs:   b.properties.Extra_outputs,
			OrderOnly: b.properties.Extra_order_only,
		})
	}
	for _, src := range b.properties.Srcs {
		ctx.GlobWithDeps(src, b.properties.Exclude_srcs)
	}
	ctx.VisitDirectDeps(func(module Module) {
		OtherModuleProvider(ctx, module, IncrementalTestProviderKey)
	})
	SetProvider(ctx, IncrementalTestProviderKey, IncrementalTestInfo{
		Value: ctx.ModuleName(),
	})
}

type fooModule struct {
	baseTestModule
}

func newFooModule() (Module, []interface{}) {
	m := &fooModule{}
	return m, []interface{}{&m.baseTestModule.properties, &m.SimpleName.Properties}
}

func (f *fooModule) Walk() bool {
	return true
}

type barModule struct {
	baseTestModule
}

func newBarModule() (Module, []interface{}) {
	m := &barModule{}
	return m, []interface{}{&m.baseTestModule.properties, &m.SimpleName.Properties}
}

func (b *barModule) Walk() bool {
	return false
}

type walkerDepsTag struct {
	BaseDependencyTag
	// True if the dependency should be followed, false otherwise.
	follow bool
}

func depsMutator(mctx BottomUpMutatorContext) {
	if m, ok := mctx.Module().(depsProvider); ok {
		mctx.AddDependency(mctx.Module(), walkerDepsTag{follow: false}, m.IgnoreDeps()...)
		mctx.AddDependency(mctx.Module(), walkerDepsTag{follow: true}, m.Deps()...)
	}
}

func TestContextParse(t *testing.T) {
	ctx := NewContext()
	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)

	r := bytes.NewBufferString(`
		foo_module {
	        name: "MyFooModule",
			deps: ["MyBarModule"],
		}

		bar_module {
	        name: "MyBarModule",
		}
	`)

	_, _, errs := ctx.parseOne(".", "Blueprint", r, parser.NewScope(nil), nil)
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
}

// > |===B---D       - represents a non-walkable edge
// > A               = represents a walkable edge
// > |===C===E---G
// >     |       |   A should not be visited because it's the root node.
// >     |===F===|   B, D and E should not be walked.
func TestWalkDeps(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			foo_module {
			    name: "A",
			    deps: ["B", "C"],
			}

			bar_module {
			    name: "B",
			    deps: ["D"],
			}

			foo_module {
			    name: "C",
			    deps: ["E", "F"],
			}

			foo_module {
			    name: "D",
			}

			bar_module {
			    name: "E",
			    deps: ["G"],
			}

			foo_module {
			    name: "F",
			    deps: ["G"],
			}

			foo_module {
			    name: "G",
			}
		`),
	})

	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)
	ctx.RegisterBottomUpMutator("deps", depsMutator)
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

	topModule := ctx.moduleGroupFromName("A", nil).modules.firstModule()
	outputDown, outputUp := walkDependencyGraph(ctx, topModule, false)
	if outputDown != "BCEFG" {
		t.Errorf("unexpected walkDeps behaviour: %s\ndown should be: BCEFG", outputDown)
	}
	if outputUp != "BEGFC" {
		t.Errorf("unexpected walkDeps behaviour: %s\nup should be: BEGFC", outputUp)
	}
}

// > |===B---D           - represents a non-walkable edge
// > A                   = represents a walkable edge
// > |===C===E===\       A should not be visited because it's the root node.
// >     |       |       B, D should not be walked.
// >     |===F===G===H   G should be visited multiple times
// >         \===/       H should only be visited once
func TestWalkDepsDuplicates(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			foo_module {
			    name: "A",
			    deps: ["B", "C"],
			}

			bar_module {
			    name: "B",
			    deps: ["D"],
			}

			foo_module {
			    name: "C",
			    deps: ["E", "F"],
			}

			foo_module {
			    name: "D",
			}

			foo_module {
			    name: "E",
			    deps: ["G"],
			}

			foo_module {
			    name: "F",
			    deps: ["G", "G"],
			}

			foo_module {
			    name: "G",
				deps: ["H"],
			}

			foo_module {
			    name: "H",
			}
		`),
	})

	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)
	ctx.RegisterBottomUpMutator("deps", depsMutator)
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

	topModule := ctx.moduleGroupFromName("A", nil).modules.firstModule()
	outputDown, outputUp := walkDependencyGraph(ctx, topModule, true)
	if outputDown != "BCEGHFGG" {
		t.Errorf("unexpected walkDeps behaviour: %s\ndown should be: BCEGHFGG", outputDown)
	}
	if outputUp != "BHGEGGFC" {
		t.Errorf("unexpected walkDeps behaviour: %s\nup should be: BHGEGGFC", outputUp)
	}
}

// >                     - represents a non-walkable edge
// > A                   = represents a walkable edge
// > |===B-------\       A should not be visited because it's the root node.
// >     |       |       B -> D should not be walked.
// >     |===C===D===E   B -> C -> D -> E should be walked
func TestWalkDepsDuplicates_IgnoreFirstPath(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			foo_module {
			    name: "A",
			    deps: ["B"],
			}

			foo_module {
			    name: "B",
			    deps: ["C"],
			    ignored_deps: ["D"],
			}

			foo_module {
			    name: "C",
			    deps: ["D"],
			}

			foo_module {
			    name: "D",
			    deps: ["E"],
			}

			foo_module {
			    name: "E",
			}
		`),
	})

	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)
	ctx.RegisterBottomUpMutator("deps", depsMutator)
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

	topModule := ctx.moduleGroupFromName("A", nil).modules.firstModule()
	outputDown, outputUp := walkDependencyGraph(ctx, topModule, true)
	expectedDown := "BDCDE"
	if outputDown != expectedDown {
		t.Errorf("unexpected walkDeps behaviour: %s\ndown should be: %s", outputDown, expectedDown)
	}
	expectedUp := "DEDCB"
	if outputUp != expectedUp {
		t.Errorf("unexpected walkDeps behaviour: %s\nup should be: %s", outputUp, expectedUp)
	}
}

func TestCreateModule(t *testing.T) {
	ctx := newContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			foo_module {
			    name: "A",
			    deps: ["B", "C"],
			}
		`),
	})

	ctx.RegisterBottomUpMutator("create", createTestMutator).UsesCreateModule()
	ctx.RegisterBottomUpMutator("deps", depsMutator)

	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)
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

	a := ctx.moduleGroupFromName("A", nil).modules.firstModule().logicModule.(*fooModule)
	creator := ctx.moduleGroupFromName("A", nil).modules.firstModule()
	b := ctx.moduleGroupFromName("B", nil).modules.firstModule().logicModule.(*barModule)
	c := ctx.moduleGroupFromName("C", nil).modules.firstModule().logicModule.(*barModule)
	d := ctx.moduleGroupFromName("D", nil).modules.firstModule().logicModule.(*fooModule)

	checkDeps := func(m Module, expected string) {
		var deps []string
		ctx.VisitDirectDeps(m, func(m Module) {
			deps = append(deps, ctx.ModuleName(m))
		})
		got := strings.Join(deps, ",")
		if got != expected {
			t.Errorf("unexpected %q dependencies, got %q expected %q",
				ctx.ModuleName(m), got, expected)
		}
	}

	checkDeps(a, "B,C")
	checkDeps(b, "D")
	checkDeps(c, "D")
	checkDeps(d, "")
	if len(ctx.createdModuleGroups[creator]) != 3 {
		t.Errorf("createdModuleGroups[%p] = %d groups, want 3", creator, len(ctx.createdModuleGroups[creator]))
	}
}

func createTestMutator(ctx BottomUpMutatorContext) {
	type props struct {
		Name string
		Deps []string
	}

	ctx.CreateModule(newBarModule, "new_bar", &props{
		Name: "B",
		Deps: []string{"D"},
	})

	ctx.CreateModule(newBarModule, "new_bar", &props{
		Name: "C",
		Deps: []string{"D"},
	})

	ctx.CreateModule(newFooModule, "new_foo", &props{
		Name: "D",
	})
}

func TestWalkFileOrder(t *testing.T) {
	// Run the test once to see how long it normally takes
	start := time.Now()
	doTestWalkFileOrder(t, time.Duration(0))
	duration := time.Since(start)

	// Run the test again, but put enough of a sleep into each visitor to detect ordering
	// problems if they exist
	doTestWalkFileOrder(t, duration)
}

// test that WalkBlueprintsFiles calls asyncVisitor in the right order
func doTestWalkFileOrder(t *testing.T, sleepDuration time.Duration) {
	// setup mock context
	ctx := newContext()
	mockFiles := map[string][]byte{
		"Android.bp": []byte(`
			sample_module {
			    name: "a",
			}
		`),
		"dir1/Android.bp": []byte(`
			sample_module {
			    name: "b",
			}
		`),
		"dir1/dir2/Android.bp": []byte(`
			sample_module {
			    name: "c",
			}
		`),
	}
	ctx.MockFileSystem(mockFiles)

	// prepare to monitor the visit order
	visitOrder := []string{}
	visitLock := sync.Mutex{}
	correctVisitOrder := []string{"Android.bp", "dir1/Android.bp", "dir1/dir2/Android.bp"}

	// sleep longer when processing the earlier files
	chooseSleepDuration := func(fileName string) (duration time.Duration) {
		duration = time.Duration(0)
		for i := len(correctVisitOrder) - 1; i >= 0; i-- {
			if fileName == correctVisitOrder[i] {
				return duration
			}
			duration = duration + sleepDuration
		}
		panic("unrecognized file name " + fileName)
	}

	visitor := func(file *parser.File) {
		time.Sleep(chooseSleepDuration(file.Name))
		visitLock.Lock()
		defer visitLock.Unlock()
		visitOrder = append(visitOrder, file.Name)
	}
	keys := []string{"Android.bp", "dir1/Android.bp", "dir1/dir2/Android.bp"}

	// visit the blueprints files
	ctx.WalkBlueprintsFiles(".", keys, visitor)

	// check the order
	if !reflect.DeepEqual(visitOrder, correctVisitOrder) {
		t.Errorf("Incorrect visit order; expected %v, got %v", correctVisitOrder, visitOrder)
	}
}

// test that WalkBlueprintsFiles reports syntax errors
func TestWalkingWithSyntaxError(t *testing.T) {
	// setup mock context
	ctx := newContext()
	mockFiles := map[string][]byte{
		"Android.bp": []byte(`
			sample_module {
			    name: "a" "b",
			}
		`),
		"dir1/Android.bp": []byte(`
			sample_module {
			    name: "b",
		`),
		"dir1/dir2/Android.bp": []byte(`
			sample_module {
			    name: "c",
			}
		`),
	}
	ctx.MockFileSystem(mockFiles)

	keys := []string{"Android.bp", "dir1/Android.bp", "dir1/dir2/Android.bp"}

	// visit the blueprints files
	_, errs := ctx.WalkBlueprintsFiles(".", keys, func(file *parser.File) {})

	expectedErrs := []error{
		errors.New(`Android.bp:3:18: expected "}", found String`),
		errors.New(`dir1/Android.bp:4:3: expected "}", found EOF`),
	}
	if fmt.Sprintf("%s", expectedErrs) != fmt.Sprintf("%s", errs) {
		t.Errorf("Incorrect errors; expected:\n%s\ngot:\n%s", expectedErrs, errs)
	}

}

func TestParseFailsForModuleWithoutName(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			foo_module {
			    name: "A",
			}

			bar_module {
			    deps: ["A"],
			}
		`),
	})
	ctx.RegisterModuleType("foo_module", newFooModule)
	ctx.RegisterModuleType("bar_module", newBarModule)

	_, errs := ctx.ParseBlueprintsFiles("Android.bp", nil)

	expectedErrs := []error{
		errors.New(`Android.bp:6:4: property 'name' is missing from a module`),
	}
	if fmt.Sprintf("%s", expectedErrs) != fmt.Sprintf("%s", errs) {
		t.Errorf("Incorrect errors; expected:\n%s\ngot:\n%s", expectedErrs, errs)
	}
}

func Test_variationMapEqualMatching(t *testing.T) {
	tests := []struct {
		name     string
		left     variationMap
		right    variationMap
		mutators []string
		want     bool
	}{
		{
			name:     "equal projection ignores other mutators",
			left:     variationMap{variations: map[string]string{"arch": "arm64", "link": "shared"}},
			right:    variationMap{variations: map[string]string{"arch": "arm64", "link": "static"}},
			mutators: []string{"arch"},
			want:     true,
		},
		{
			name:     "different projected values",
			left:     variationMap{variations: map[string]string{"arch": "arm64"}},
			right:    variationMap{variations: map[string]string{"arch": "arm"}},
			mutators: []string{"arch"},
			want:     false,
		},
		{
			name:     "missing projected value",
			left:     variationMap{variations: map[string]string{}},
			right:    variationMap{variations: map[string]string{"arch": "arm64"}},
			mutators: []string{"arch"},
			want:     false,
		},
		{
			name:     "empty projection",
			left:     variationMap{variations: map[string]string{"arch": "arm64"}},
			right:    variationMap{variations: map[string]string{"arch": "arm"}},
			mutators: nil,
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.left.equalMatching(tt.right, tt.mutators); got != tt.want {
				t.Fatalf("equalMatching() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestUpdateDependenciesForModulesMatchesFullRebuild(t *testing.T) {
	groupA := &moduleGroup{name: "a", index: 0}
	groupB := &moduleGroup{name: "b", index: 1}
	a := &moduleInfo{group: groupA}
	b := &moduleInfo{group: groupA}
	c := &moduleInfo{group: groupA}
	x := &moduleInfo{group: groupB}
	y := &moduleInfo{group: groupB}
	groupA.modules = moduleList{a, b, c}
	groupB.modules = moduleList{x, y}
	a.directDeps = []depInfo{{module: x}}
	b.directDeps = []depInfo{{module: a}}
	c.directDeps = []depInfo{{module: y}}

	ctx := NewContext()
	ctx.moduleGroups = []*moduleGroup{groupA, groupB}
	if errs := ctx.updateDependencies(); len(errs) != 0 {
		t.Fatalf("initial updateDependencies() errors = %v", errs)
	}

	// Model a transition split that inserts a variant and changes one
	// dependency, then a dependency replacement on another module.
	newVariant := &moduleInfo{group: groupA, directDeps: slices.Clone(a.directDeps)}
	groupA.modules = slices.Insert(groupA.modules, 1, newVariant)
	c.directDeps[0].module = x
	ctx.updateDependenciesForModules([]*moduleInfo{a, newVariant, b, c})

	modules := []*moduleInfo{a, newVariant, b, c, x, y}
	forwardAfterLocal := make(map[*moduleInfo][]*moduleInfo, len(modules))
	reverseAfterLocal := make(map[*moduleInfo][]*moduleInfo, len(modules))
	for _, module := range modules {
		forwardAfterLocal[module] = slices.Clone(module.forwardDeps)
		reverseAfterLocal[module] = slices.Clone(module.reverseDeps)
	}
	if errs := ctx.updateDependencies(); len(errs) != 0 {
		t.Fatalf("reference updateDependencies() errors = %v", errs)
	}
	for _, module := range modules {
		if !slices.Equal(module.forwardDeps, forwardAfterLocal[module]) {
			t.Errorf("module %p forward dependencies differ: local %v, full %v", module, forwardAfterLocal[module], module.forwardDeps)
		}
		if !slices.Equal(module.reverseDeps, reverseAfterLocal[module]) {
			t.Errorf("module %p reverse dependencies differ: local %v, full %v", module, reverseAfterLocal[module], module.reverseDeps)
		}
	}
}

func TestUpdateReverseDepsForNewDirectDepsPreservesGraphOrder(t *testing.T) {
	groupA := &moduleGroup{index: 0}
	groupB := &moduleGroup{index: 1}
	target := &moduleInfo{group: &moduleGroup{index: 2}}
	a1 := &moduleInfo{group: groupA, newDirectDeps: []*moduleInfo{target}}
	a2 := &moduleInfo{group: groupA, newDirectDeps: []*moduleInfo{target}}
	b := &moduleInfo{group: groupB, newDirectDeps: []*moduleInfo{target}}
	groupA.modules = moduleList{a1, a2}
	groupB.modules = moduleList{b}

	updateReverseDepsForNewDirectDeps([]*moduleInfo{b, a2, a1})

	if !slices.Equal(target.reverseDeps, []*moduleInfo{a1, a2, b}) {
		t.Fatalf("reverseDeps = %v, want modules in graph order [a1 a2 b]", target.reverseDeps)
	}
	for _, module := range []*moduleInfo{a1, a2, b} {
		if len(module.newDirectDeps) != 0 {
			t.Errorf("module %p newDirectDeps = %v, want cleared", module, module.newDirectDeps)
		}
	}
}

func TestUpdateSplitVariantReferencesRepairsOnlyAffectedModules(t *testing.T) {
	sourceGroup := &moduleGroup{index: 0}
	dependentGroup := &moduleGroup{index: 1}
	newDepGroup := &moduleGroup{index: 2}
	createdGroup := &moduleGroup{index: 3}
	targetGroup := &moduleGroup{index: 4}
	source := &moduleInfo{group: sourceGroup}
	dependent := &moduleInfo{group: dependentGroup, directDeps: []depInfo{{module: source}}}
	newDependent := &moduleInfo{group: newDepGroup}
	created := &moduleInfo{group: createdGroup, createdBy: source}
	target := &moduleInfo{group: targetGroup}
	sourceGroup.modules = moduleList{source}
	dependentGroup.modules = moduleList{dependent}
	newDepGroup.modules = moduleList{newDependent}
	createdGroup.modules = moduleList{created}
	targetGroup.modules = moduleList{target}
	source.directDeps = []depInfo{{module: target}}

	ctx := NewContext()
	ctx.moduleGroups = []*moduleGroup{sourceGroup, dependentGroup, newDepGroup, createdGroup, targetGroup}
	ctx.createdModuleGroups[source] = []*moduleGroup{createdGroup}
	if errs := ctx.updateDependencies(); len(errs) != 0 {
		t.Fatalf("initial updateDependencies() errors = %v", errs)
	}
	newDependent.directDeps = []depInfo{{module: source}}

	first := &moduleInfo{group: sourceGroup, directDeps: slices.Clone(source.directDeps)}
	second := &moduleInfo{group: sourceGroup, directDeps: slices.Clone(source.directDeps)}
	source.obsoletedByNewVariants = true
	source.splitModules = moduleList{first, second}
	changedDependents := ctx.updateSplitVariantReferences([]*moduleInfo{source}, []*moduleInfo{newDependent})

	if !slices.Equal(sourceGroup.modules, moduleList{first, second}) {
		t.Fatalf("source variants = %v, want [first second]", sourceGroup.modules)
	}
	if dependent.directDeps[0].module != first {
		t.Errorf("existing dependency = %p, want first variant %p", dependent.directDeps[0].module, first)
	}
	if newDependent.directDeps[0].module != first {
		t.Errorf("new dependency = %p, want first variant %p", newDependent.directDeps[0].module, first)
	}
	if created.createdBy != first {
		t.Errorf("createdBy = %p, want first variant %p", created.createdBy, first)
	}
	if !slices.Equal(changedDependents, []*moduleInfo{dependent, newDependent}) {
		t.Errorf("changed dependents = %v, want [existing dependent, new dependent]", changedDependents)
	}
	if !slices.Contains(ctx.createdModuleGroups[first], createdGroup) {
		t.Errorf("created child group was not reindexed under the first variant")
	}
	if slices.Contains(target.reverseDeps, source) {
		t.Errorf("removed split source remains in dependency reverseDeps")
	}
	if len(source.forwardDeps) != 0 || len(source.reverseDeps) != 0 {
		t.Errorf("removed split source retains graph edges: forward=%v reverse=%v", source.forwardDeps, source.reverseDeps)
	}
	ctx.updateDependenciesForModules(append(sourceGroup.modules, changedDependents...))
	liveModules := []*moduleInfo{first, second, dependent, newDependent, created, target}
	forwardAfterLocal := make(map[*moduleInfo][]*moduleInfo, len(liveModules))
	reverseAfterLocal := make(map[*moduleInfo][]*moduleInfo, len(liveModules))
	for _, module := range liveModules {
		forwardAfterLocal[module] = slices.Clone(module.forwardDeps)
		reverseAfterLocal[module] = slices.Clone(module.reverseDeps)
	}
	if errs := ctx.updateDependencies(); len(errs) != 0 {
		t.Fatalf("reference updateDependencies() errors = %v", errs)
	}
	for _, module := range liveModules {
		if !slices.Equal(module.forwardDeps, forwardAfterLocal[module]) {
			t.Errorf("module %p forwardDeps = %v, want locally refreshed %v", module, module.forwardDeps, forwardAfterLocal[module])
		}
		if !slices.Equal(module.reverseDeps, reverseAfterLocal[module]) {
			t.Errorf("module %p reverseDeps = %v, want locally refreshed %v", module, module.reverseDeps, reverseAfterLocal[module])
		}
	}

	if errs := ctx.updateDependencies(); len(errs) != 0 {
		t.Fatalf("updateDependencies() before a second split errors = %v", errs)
	}
	firstA := &moduleInfo{group: sourceGroup}
	firstB := &moduleInfo{group: sourceGroup}
	first.obsoletedByNewVariants = true
	first.splitModules = moduleList{firstA, firstB}
	ctx.updateSplitVariantReferences([]*moduleInfo{first}, nil)
	if created.createdBy != firstA {
		t.Errorf("createdBy after creator split again = %p, want first variant %p", created.createdBy, firstA)
	}
}

func BenchmarkUpdateDependenciesForModules(b *testing.B) {
	const (
		groupCount       = 2048
		modulesPerGroup  = 4
		totalModuleCount = groupCount * modulesPerGroup
	)

	ctx := NewContext()
	modules := make([]*moduleInfo, totalModuleCount)
	for groupIndex := 0; groupIndex < groupCount; groupIndex++ {
		group := &moduleGroup{name: strconv.Itoa(groupIndex), index: groupIndex}
		group.modules = make(moduleList, modulesPerGroup)
		for moduleIndex := range group.modules {
			index := groupIndex*modulesPerGroup + moduleIndex
			module := &moduleInfo{group: group}
			group.modules[moduleIndex] = module
			modules[index] = module
		}
		ctx.moduleGroups = append(ctx.moduleGroups, group)
	}
	for index, module := range modules {
		module.directDeps = []depInfo{{module: modules[(index+modulesPerGroup+1)%totalModuleCount]}}
	}
	if errs := ctx.updateDependencies(); len(errs) != 0 {
		b.Fatalf("initial updateDependencies() errors = %v", errs)
	}
	affected := slices.Clone(ctx.moduleGroups[groupCount/2].modules)

	b.Run("full-graph", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ctx.updateDependencies()
		}
	})
	b.Run("one-module-group", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ctx.updateDependenciesForModules(affected)
		}
	})
}

func Test_findVariant(t *testing.T) {
	module := &moduleInfo{
		variant: variant{
			name: "normal_local",
			variations: variationMap{
				map[string]string{
					"normal": "normal",
				},
			},
		},
	}

	makeDependencyGroup := func(in ...*moduleInfo) *moduleGroup {
		group := &moduleGroup{
			name: "dep",
		}
		for _, m := range in {
			m.group = group
			group.modules = append(group.modules, m)
		}
		return group
	}

	tests := []struct {
		name         string
		possibleDeps *moduleGroup
		variations   []Variation
		far          bool
		reverse      bool
		want         string
	}{
		{
			name: "AddVariationDependencies(nil)",
			// A dependency that matches the non-local variations of the module
			possibleDeps: makeDependencyGroup(
				&moduleInfo{
					variant: variant{
						name: "normal",
						variations: variationMap{
							map[string]string{
								"normal": "normal",
							},
						},
					},
				},
			),
			variations: nil,
			far:        false,
			reverse:    false,
			want:       "normal",
		},
		{
			name: "AddVariationDependencies(a)",
			// A dependency with local variations
			possibleDeps: makeDependencyGroup(
				&moduleInfo{
					variant: variant{
						name: "normal_a",
						variations: variationMap{
							map[string]string{
								"normal": "normal",
								"a":      "a",
							},
						},
					},
				},
			),
			variations: []Variation{{"a", "a"}},
			far:        false,
			reverse:    false,
			want:       "normal_a",
		},
		{
			name: "AddFarVariationDependencies(far)",
			// A dependency with far variations
			possibleDeps: makeDependencyGroup(
				&moduleInfo{
					variant: variant{
						name:       "",
						variations: variationMap{},
					},
				},
				&moduleInfo{
					variant: variant{
						name: "far",
						variations: variationMap{
							map[string]string{
								"far": "far",
							},
						},
					},
				},
			),
			variations: []Variation{{"far", "far"}},
			far:        true,
			reverse:    false,
			want:       "far",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := NewContext()
			got, _, errs := ctx.findVariant(nil, module, nil, tt.possibleDeps, tt.variations, tt.far, tt.reverse, -1)
			if errs != nil {
				t.Fatal(errs)
			}
			if g, w := got == nil, tt.want == "nil"; g != w {
				t.Fatalf("findVariant() got = %v, want %v", got, tt.want)
			}
			if got != nil {
				if g, w := got.String(), fmt.Sprintf("module %q variant %q", "dep", tt.want); g != w {
					t.Errorf("findVariant() got = %v, want %v", g, w)
				}
			}
		})
	}
}

func Test_findVariantCachesExactMatchesAndInvalidatesAfterVariantChanges(t *testing.T) {
	group := &moduleGroup{name: "dep"}
	for i := 0; i < 4; i++ {
		module := &moduleInfo{
			group: group,
			variant: variant{
				name:       fmt.Sprintf("v%d", i),
				variations: variationMap{variations: map[string]string{"axis": fmt.Sprintf("v%d", i)}},
			},
		}
		group.modules = append(group.modules, module)
	}

	source := &moduleInfo{variant: variant{variations: variationMap{}}}
	ctx := NewContext()
	find := func() *moduleInfo {
		t.Helper()
		module, _, errs := ctx.findVariant(nil, source, nil, group, []Variation{{Mutator: "axis", Variation: "v2"}}, false, false, -1)
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		return module
	}

	if got, want := find(), group.modules[2]; got != want {
		t.Fatalf("findVariant() = %p, want %p", got, want)
	}
	if len(group.exactVariantCache) != 1 {
		t.Fatalf("exact variant cache has %d entries, want 1", len(group.exactVariantCache))
	}

	replacement := &moduleInfo{
		group: group,
		variant: variant{
			name:       "v2-replacement",
			variations: variationMap{variations: map[string]string{"axis": "v2"}},
		},
	}
	group.modules[2] = replacement
	group.invalidateVariantLookupCaches()
	if got := find(); got != replacement {
		t.Fatalf("findVariant() after cache invalidation = %p, want replacement %p", got, replacement)
	}

	missing, _, errs := ctx.findVariant(nil, source, nil, group, []Variation{{Mutator: "axis", Variation: "new"}}, false, false, -1)
	if len(errs) > 0 || missing != nil {
		t.Fatalf("findVariant() for missing exact variation = %p, errors %v; want nil", missing, errs)
	}
	newVariation := variationMap{variations: map[string]string{"axis": "new"}}
	entries := group.exactVariantCache[newVariation.cacheHash()]
	if len(entries) != 1 || !entries[0].variations.equal(newVariation) || entries[0].module != nil {
		t.Fatal("missing exact variation was not cached")
	}
	added := &moduleInfo{
		group: group,
		variant: variant{
			name:       "new",
			variations: variationMap{variations: map[string]string{"axis": "new"}},
		},
	}
	group.modules = append(group.modules, added)
	group.invalidateVariantLookupCaches()
	got, _, errs := ctx.findVariant(nil, source, nil, group, []Variation{{Mutator: "axis", Variation: "new"}}, false, false, -1)
	if len(errs) > 0 || got != added {
		t.Fatalf("findVariant() after adding variant = %p, errors %v; want %p", got, errs, added)
	}
}

func TestModuleGroupProjectionAndFarLookupCachesInvalidate(t *testing.T) {
	group := &moduleGroup{name: "dep"}
	variants := []map[string]string{
		{"arch": "arm64", "link": "shared"},
		{"arch": "arm64", "link": "static"},
		{"arch": "arm64", "image": "vendor"},
		{"arch": "arm", "link": "shared"},
	}
	for i, variations := range variants {
		module := &moduleInfo{
			group: group,
			variant: variant{
				name:       fmt.Sprintf("v%d", i),
				variations: variationMap{variations: variations},
			},
		}
		group.modules = append(group.modules, module)
	}

	requested := variationMap{variations: map[string]string{"arch": "arm64"}}
	if got, want := group.firstVariantMatching(requested, []string{"arch"}), group.modules[0]; got != want {
		t.Fatalf("firstVariantMatching() = %p, want %p", got, want)
	}
	group.modules[0] = group.modules[3]
	group.invalidateVariantLookupCaches()
	if got, want := group.firstVariantMatching(requested, []string{"arch"}), group.modules[1]; got != want {
		t.Fatalf("firstVariantMatching() after invalidation = %p, want %p", got, want)
	}

	if got, want := group.farVariant(requested), group.modules[1]; got != want {
		t.Fatalf("farVariant() = %p, want %p", got, want)
	}
	added := &moduleInfo{
		group: group,
		variant: variant{
			name:       "arm64-common",
			variations: variationMap{variations: map[string]string{"arch": "arm64"}},
		},
	}
	group.modules = append(group.modules, added)
	group.invalidateVariantLookupCaches()
	if got := group.farVariant(requested); got != added {
		t.Fatalf("farVariant() after invalidation = %p, want %p", got, added)
	}
}

func TestModuleGroupVariantLookupCacheIsBounded(t *testing.T) {
	group := &moduleGroup{name: "dep"}
	for i := 0; i < 4; i++ {
		group.modules = append(group.modules, &moduleInfo{
			group: group,
			variant: variant{
				variations: variationMap{variations: map[string]string{"arch": fmt.Sprintf("arch%d", i)}},
			},
		})
	}
	for i := 0; i < moduleGroupVariantLookupCacheLimit*2; i++ {
		group.exactVariant(variationMap{variations: map[string]string{"arch": fmt.Sprintf("missing%d", i)}})
	}
	if group.variantLookupCacheSize != moduleGroupVariantLookupCacheLimit {
		t.Fatalf("variant lookup cache retained %d entries, want limit %d", group.variantLookupCacheSize, moduleGroupVariantLookupCacheLimit)
	}
}

func TestModuleGroupVariantLookupCachesSupportConcurrentReaders(t *testing.T) {
	group := &moduleGroup{name: "dep"}
	for i := 0; i < 8; i++ {
		group.modules = append(group.modules, &moduleInfo{
			group: group,
			variant: variant{
				name:       fmt.Sprintf("v%d", i),
				variations: variationMap{variations: map[string]string{"arch": fmt.Sprintf("arch%d", i), "link": "shared"}},
			},
		})
	}
	exactQuery := variationMap{variations: map[string]string{"arch": "arch7", "link": "shared"}}
	projectionQuery := variationMap{variations: map[string]string{"arch": "arch7"}}
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < 100; j++ {
				if group.exactVariant(exactQuery) != group.modules[7] {
					t.Errorf("exact lookup returned a different variant")
					return
				}
				if group.firstVariantMatching(projectionQuery, []string{"arch"}) != group.modules[7] {
					t.Errorf("projected lookup returned a different variant")
					return
				}
				if group.farVariant(projectionQuery) != group.modules[7] {
					t.Errorf("far lookup returned a different variant")
					return
				}
			}
		}()
	}
	wait.Wait()
}

var benchmarkVariantResult *moduleInfo

func BenchmarkModuleGroupVariantLookup(b *testing.B) {
	group := &moduleGroup{name: "dep"}
	for i := 0; i < 128; i++ {
		module := &moduleInfo{
			group: group,
			variant: variant{
				name:       fmt.Sprintf("v%d", i),
				variations: variationMap{variations: map[string]string{"arch": fmt.Sprintf("arch%d", i), "link": "shared"}},
			},
		}
		group.modules = append(group.modules, module)
	}
	query := variationMap{variations: map[string]string{"arch": "arch127", "link": "shared"}}

	b.Run("linear", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, module := range group.modules {
				if module.variant.variations.equal(query) {
					benchmarkVariantResult = module
					break
				}
			}
		}
	})
	b.Run("cached", func(b *testing.B) {
		group.exactVariant(query)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkVariantResult = group.exactVariant(query)
		}
	})
	b.Run("projected-linear", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, module := range group.modules {
				if module.variant.variations.equalMatching(query, []string{"arch"}) {
					benchmarkVariantResult = module
					break
				}
			}
		}
	})
	b.Run("projected-cached", func(b *testing.B) {
		group.firstVariantMatching(query, []string{"arch"})
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkVariantResult = group.firstVariantMatching(query, []string{"arch"})
		}
	})
	b.Run("far-linear", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var found *moduleInfo
			bestDivergence := math.MaxInt
			for _, module := range group.modules {
				if query.subsetOf(module.variant.variations) {
					divergence := module.variant.variations.differenceKeysCount(query)
					if divergence < bestDivergence {
						found = module
						bestDivergence = divergence
					}
				}
			}
			benchmarkVariantResult = found
		}
	})
	b.Run("far-cached", func(b *testing.B) {
		farQuery := variationMap{variations: map[string]string{"arch": "arch127"}}
		group.farVariant(farQuery)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkVariantResult = group.farVariant(farQuery)
		}
	})
}

func Test_parallelVisit(t *testing.T) {
	addDep := func(from, to *moduleInfo) {
		from.directDeps = append(from.directDeps, depInfo{to, nil})
		from.forwardDeps = append(from.forwardDeps, to)
		to.reverseDeps = append(to.reverseDeps, from)
	}

	create := func(name string) *moduleInfo {
		m := &moduleInfo{
			group: &moduleGroup{
				name: name,
			},
		}
		m.group.modules = moduleList{m}
		return m
	}
	moduleA := create("A")
	moduleB := create("B")
	moduleC := create("C")
	moduleD := create("D")
	moduleE := create("E")
	moduleF := create("F")
	moduleG := create("G")

	moduleH := create("H")
	moduleI := create("I")
	moduleJ := create("J")

	// A depends on B, B depends on C.
	addDep(moduleA, moduleB)
	addDep(moduleB, moduleC)

	// Nothing depends on D through G, and they don't depend on anything.

	// H depends on I, and I and J depend on each other.
	addDep(moduleH, moduleI)
	addDep(moduleI, moduleJ)
	addDep(moduleJ, moduleI)

	t.Run("no modules", func(t *testing.T) {
		errs := parallelVisit(slices.Values([]*moduleInfo(nil)), bottomUpVisitorImpl{}, 1,
			func(module *moduleInfo, pause pauseFunc) bool {
				panic("unexpected call to visitor")
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
	})
	t.Run("bottom up", func(t *testing.T) {
		order := ""
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC}), bottomUpVisitorImpl{}, 1,
			func(module *moduleInfo, pause pauseFunc) bool {
				order += module.group.name
				return false
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
		if g, w := order, "CBA"; g != w {
			t.Errorf("expected order %q, got %q", w, g)
		}
	})
	t.Run("subset visit keeps selected dependency ordering", func(t *testing.T) {
		moduleC.waitingCount.Store(77)
		for _, testCase := range []struct {
			name  string
			order visitOrderer
			want  string
		}{
			{name: "bottom up", order: bottomUpVisitorImpl{}, want: "BA"},
			{name: "top down", order: topDownVisitorImpl{}, want: "AB"},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				order := ""
				errs := parallelVisitSubset([]*moduleInfo{moduleA, moduleB}, testCase.order, 1,
					func(module *moduleInfo, _ pauseFunc) bool {
						order += module.group.name
						return false
					})
				if errs != nil {
					t.Fatalf("unexpected errors: %v", errs)
				}
				if order != testCase.want {
					t.Fatalf("visit order = %q, want %q", order, testCase.want)
				}
				if got := moduleC.waitingCount.Load(); got != 77 {
					t.Fatalf("excluded dependency waitingCount = %d, want unchanged 77", got)
				}
			})
		}
	})
	t.Run("pause", func(t *testing.T) {
		order := ""
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC, moduleD}), bottomUpVisitorImpl{}, 1,
			func(module *moduleInfo, pause pauseFunc) bool {
				if module == moduleC {
					// Pause module C on module D
					pause(moduleD)
				}
				order += module.group.name
				return false
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
		if g, w := order, "DCBA"; g != w {
			t.Errorf("expected order %q, got %q", w, g)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		order := ""
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC}), bottomUpVisitorImpl{}, 1,
			func(module *moduleInfo, pause pauseFunc) bool {
				order += module.group.name
				// Cancel in module B
				return module == moduleB
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
		if g, w := order, "CB"; g != w {
			t.Errorf("expected order %q, got %q", w, g)
		}
	})
	t.Run("pause and cancel", func(t *testing.T) {
		order := ""
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC, moduleD}), bottomUpVisitorImpl{}, 1,
			func(module *moduleInfo, pause pauseFunc) bool {
				if module == moduleC {
					// Pause module C on module D
					pause(moduleD)
				}
				order += module.group.name
				// Cancel in module D
				return module == moduleD
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
		if g, w := order, "D"; g != w {
			t.Errorf("expected order %q, got %q", w, g)
		}
	})
	t.Run("parallel", func(t *testing.T) {
		order := ""
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC}), bottomUpVisitorImpl{}, 3,
			func(module *moduleInfo, pause pauseFunc) bool {
				order += module.group.name
				return false
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
		if g, w := order, "CBA"; g != w {
			t.Errorf("expected order %q, got %q", w, g)
		}
	})
	t.Run("pause existing", func(t *testing.T) {
		order := ""
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC}), bottomUpVisitorImpl{}, 3,
			func(module *moduleInfo, pause pauseFunc) bool {
				if module == moduleA {
					// Pause module A on module B (an existing dependency)
					pause(moduleB)
				}
				order += module.group.name
				return false
			})
		if errs != nil {
			t.Errorf("expected no errors, got %q", errs)
		}
		if g, w := order, "CBA"; g != w {
			t.Errorf("expected order %q, got %q", w, g)
		}
	})
	t.Run("cycle", func(t *testing.T) {
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC}), bottomUpVisitorImpl{}, 3,
			func(module *moduleInfo, pause pauseFunc) bool {
				if module == moduleC {
					// Pause module C on module A (a dependency cycle)
					pause(moduleA)
				}
				return false
			})
		want := []string{
			`encountered dependency cycle`,
			`module "C" depends on module "A"`,
			`module "A" depends on module "B"`,
			`module "B" depends on module "C"`,
		}
		for i := range want {
			if len(errs) <= i {
				t.Errorf("missing error %s", want[i])
			} else if !strings.Contains(errs[i].Error(), want[i]) {
				t.Errorf("expected error %s, got %s", want[i], errs[i])
			}
		}
		if len(errs) > len(want) {
			for _, err := range errs[len(want):] {
				t.Errorf("unexpected error %s", err.Error())
			}
		}
	})
	t.Run("pause cycle", func(t *testing.T) {
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleA, moduleB, moduleC, moduleD}), bottomUpVisitorImpl{}, 3,
			func(module *moduleInfo, pause pauseFunc) bool {
				if module == moduleC {
					// Pause module C on module D
					pause(moduleD)
				}
				if module == moduleD {
					// Pause module D on module C (a pause cycle)
					pause(moduleC)
				}
				return false
			})
		want := []string{
			`encountered dependency cycle`,
			`module "D" depends on module "C"`,
			`module "C" depends on module "D"`,
		}
		for i := range want {
			if len(errs) <= i {
				t.Errorf("missing error %s", want[i])
			} else if !strings.Contains(errs[i].Error(), want[i]) {
				t.Errorf("expected error %s, got %s", want[i], errs[i])
			}
		}
		if len(errs) > len(want) {
			for _, err := range errs[len(want):] {
				t.Errorf("unexpected error %s", err.Error())
			}
		}
	})
	t.Run("pause cycle with deps", func(t *testing.T) {
		pauseDeps := map[*moduleInfo]*moduleInfo{
			// F and G form a pause cycle
			moduleF: moduleG,
			moduleG: moduleF,
			// D depends on E which depends on the pause cycle, making E the first alphabetical
			// entry in pauseMap, which is not part of the cycle.
			moduleD: moduleE,
			moduleE: moduleF,
		}
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleD, moduleE, moduleF, moduleG}), bottomUpVisitorImpl{}, 4,
			func(module *moduleInfo, pause pauseFunc) bool {
				if dep, ok := pauseDeps[module]; ok {
					pause(dep)
				}
				return false
			})
		want := []string{
			`encountered dependency cycle`,
			`module "G" depends on module "F"`,
			`module "F" depends on module "G"`,
		}
		for i := range want {
			if len(errs) <= i {
				t.Errorf("missing error %s", want[i])
			} else if !strings.Contains(errs[i].Error(), want[i]) {
				t.Errorf("expected error %s, got %s", want[i], errs[i])
			}
		}
		if len(errs) > len(want) {
			for _, err := range errs[len(want):] {
				t.Errorf("unexpected error %s", err.Error())
			}
		}
	})
	t.Run("existing cycle", func(t *testing.T) {
		errs := parallelVisit(slices.Values([]*moduleInfo{moduleG, moduleH, moduleI, moduleJ}), bottomUpVisitorImpl{}, 3,
			func(module *moduleInfo, pause pauseFunc) bool {
				if module == moduleG {
					// Pause module G on module I.  This verifies the fix for b/433694465, where
					// having a module paused on a cycle of existing dependencies could drop
					// the cycle error.
					pause(moduleI)
				}
				return false
			})
		want := []string{
			`encountered dependency cycle`,
			`module "J" depends on module "I"`,
			`module "I" depends on module "J"`,
		}
		for i := range want {
			if len(errs) <= i {
				t.Errorf("missing error %s", want[i])
			} else if !strings.Contains(errs[i].Error(), want[i]) {
				t.Errorf("expected error %s, got %s", want[i], errs[i])
			}
		}
		if len(errs) > len(want) {
			for _, err := range errs[len(want):] {
				t.Errorf("unexpected error %s", err.Error())
			}
		}
	})
}

func TestDeduplicateOrderOnlyDeps(t *testing.T) {
	b := func(output string, inputs []string, orderOnlyDeps []string) *buildDef {
		return &buildDef{
			OutputStrings:    []string{output},
			InputStrings:     inputs,
			OrderOnlyStrings: uniquelist.Make(orderOnlyDeps),
		}
	}

	type testcase struct {
		bp             string
		expectedPhonys []*buildDef
		conversions    map[string][]string
	}
	fnvHash := func(s string) string {
		hash := fnv.New64a()
		hash.Write([]byte(s))
		return strconv.FormatUint(hash.Sum64(), 16)
	}
	testCases := []testcase{{
		bp: `
			foo_module {
					name: "A",
					outputs: ["A"],
					order_only: ["d"],
			}
			foo_module {
					name: "B",
					outputs: ["B"],
					order_only: ["d"],
			}
		`,
		expectedPhonys: []*buildDef{
			b("dedup-"+fnvHash("d"), []string{"d"}, nil),
		},
		conversions: map[string][]string{
			"A": []string{"dedup-" + fnvHash("d")},
			"B": []string{"dedup-" + fnvHash("d")},
		},
	}, {
		bp: `
			foo_module {
					name: "A",
					outputs: ["A"],
					order_only: ["a"],
			}
			foo_module {
					name: "B",
					outputs: ["B"],
					order_only: ["b"],
			}
		`,
	}, {
		bp: `
			foo_module {
					name: "A",
					outputs: ["A"],
					order_only: ["a"],
			}
			foo_module {
					name: "B",
					outputs: ["B"],
					order_only: ["b"],
			}
			foo_module {
					name: "C",
					outputs: ["C"],
					order_only: ["a"],
			}
		`,
		expectedPhonys: []*buildDef{b("dedup-"+fnvHash("a"), []string{"a"}, nil)},
		conversions: map[string][]string{
			"A": []string{"dedup-" + fnvHash("a")},
			"B": []string{"b"},
			"C": []string{"dedup-" + fnvHash("a")},
		},
	}, {
		bp: `
			foo_module {
					name: "A",
					outputs: ["A"],
					order_only: ["a", "b"],
					extra_outputs: ["B"],
					extra_order_only: ["a", "b"],
			}
			foo_module {
					name: "C",
					outputs: ["C"],
					order_only: ["a", "c"],
					extra_outputs: ["D"],
					extra_order_only: ["a", "c"],
			}
		`,
		expectedPhonys: []*buildDef{
			b("dedup-"+fnvHash("ab"), []string{"a", "b"}, nil),
			b("dedup-"+fnvHash("ac"), []string{"a", "c"}, nil)},
		conversions: map[string][]string{
			"A": []string{"dedup-" + fnvHash("ab")},
			"B": []string{"dedup-" + fnvHash("ab")},
			"C": []string{"dedup-" + fnvHash("ac")},
			"D": []string{"dedup-" + fnvHash("ac")},
		},
	}}
	for index, tc := range testCases {
		t.Run(fmt.Sprintf("TestCase-%d", index), func(t *testing.T) {
			ctx := bpSetup(t, tc.bp)
			_, errs := ctx.PrepareBuildActions(nil)
			if len(errs) > 0 {
				t.Errorf("unexpected errors calling generateModuleBuildActions:")
				for _, err := range errs {
					t.Errorf("  %s", err)
				}
				t.FailNow()
			}
			var modules []*moduleInfo
			for module := range ctx.iterateAllVariants() {
				modules = append(modules, module)
			}
			actualPhonys := ctx.deduplicateOrderOnlyDeps(modules)
			if len(actualPhonys.variables) != 0 {
				t.Errorf("No variables expected but found %v", actualPhonys.variables)
			}
			if len(actualPhonys.rules) != 0 {
				t.Errorf("No rules expected but found %v", actualPhonys.rules)
			}
			if e, a := len(tc.expectedPhonys), len(actualPhonys.buildDefs); e != a {
				t.Errorf("Expected %d build statements but got %d", e, a)
			}
			for i := 0; i < len(tc.expectedPhonys); i++ {
				a := actualPhonys.buildDefs[i]
				e := tc.expectedPhonys[i]
				if !reflect.DeepEqual(e.Outputs, a.Outputs) {
					t.Errorf("phonys expected %v but actualPhonys %v", e.Outputs, a.Outputs)
				}
				if !reflect.DeepEqual(e.Inputs, a.Inputs) {
					t.Errorf("phonys expected %v but actualPhonys %v", e.Inputs, a.Inputs)
				}
			}
			find := func(k string) *buildDef {
				for _, m := range modules {
					for _, b := range m.actionDefs.buildDefs {
						if reflect.DeepEqual(b.OutputStrings, []string{k}) {
							return b
						}
					}
				}
				return nil
			}
			for k, conversion := range tc.conversions {
				actual := find(k)
				if actual == nil {
					t.Errorf("Couldn't find %s", k)
				}
				if !reflect.DeepEqual(actual.OrderOnlyStrings.ToSlice(), conversion) {
					t.Errorf("expected %s.OrderOnly = %v but got %v", k, conversion, actual.OrderOnly)
				}
			}
		})
	}
}

func TestSourceRootDirAllowed(t *testing.T) {
	type pathCase struct {
		path           string
		decidingPrefix string
		allowed        bool
	}
	testcases := []struct {
		desc      string
		rootDirs  []string
		pathCases []pathCase
	}{
		{
			desc: "simple case",
			rootDirs: []string{
				"a",
				"b/c/d",
				"-c",
				"-d/c/a",
				"c/some_single_file",
			},
			pathCases: []pathCase{
				{
					path:           "a",
					decidingPrefix: "a",
					allowed:        true,
				},
				{
					path:           "a/b/c",
					decidingPrefix: "a",
					allowed:        true,
				},
				{
					path:           "b",
					decidingPrefix: "",
					allowed:        true,
				},
				{
					path:           "b/c/d/a",
					decidingPrefix: "b/c/d",
					allowed:        true,
				},
				{
					path:           "c",
					decidingPrefix: "c",
					allowed:        false,
				},
				{
					path:           "c/a/b",
					decidingPrefix: "c",
					allowed:        false,
				},
				{
					path:           "c/some_single_file",
					decidingPrefix: "c/some_single_file",
					allowed:        true,
				},
				{
					path:           "d/c/a/abc",
					decidingPrefix: "d/c/a",
					allowed:        false,
				},
			},
		},
		{
			desc: "root directory order matters",
			rootDirs: []string{
				"-a",
				"a/c/some_allowed_file",
				"a/b/d/some_allowed_file",
				"a/b",
				"a/c",
				"-a/b/d",
			},
			pathCases: []pathCase{
				{
					path:           "a",
					decidingPrefix: "a",
					allowed:        false,
				},
				{
					path:           "a/some_disallowed_file",
					decidingPrefix: "a",
					allowed:        false,
				},
				{
					path:           "a/c/some_allowed_file",
					decidingPrefix: "a/c/some_allowed_file",
					allowed:        true,
				},
				{
					path:           "a/b/d/some_allowed_file",
					decidingPrefix: "a/b/d/some_allowed_file",
					allowed:        true,
				},
				{
					path:           "a/b/c",
					decidingPrefix: "a/b",
					allowed:        true,
				},
				{
					path:           "a/b/c/some_allowed_file",
					decidingPrefix: "a/b",
					allowed:        true,
				},
				{
					path:           "a/b/d",
					decidingPrefix: "a/b/d",
					allowed:        false,
				},
			},
		},
	}
	for _, tc := range testcases {
		dirs := SourceRootDirs{}
		dirs.Add(tc.rootDirs...)
		for _, pc := range tc.pathCases {
			t.Run(fmt.Sprintf("%s: %s", tc.desc, pc.path), func(t *testing.T) {
				allowed, decidingPrefix := dirs.SourceRootDirAllowed(pc.path)
				if allowed != pc.allowed {
					if pc.allowed {
						t.Errorf("expected path %q to be allowed, but was not; root allowlist: %q", pc.path, tc.rootDirs)
					} else {
						t.Errorf("path %q was allowed unexpectedly; root allowlist: %q", pc.path, tc.rootDirs)
					}
				}
				if decidingPrefix != pc.decidingPrefix {
					t.Errorf("expected decidingPrefix to be %q, but got %q", pc.decidingPrefix, decidingPrefix)
				}
			})
		}
	}
}

func TestSourceRootDirs(t *testing.T) {
	root_foo_bp := `
	foo_module {
		name: "foo",
		deps: ["foo_dir1", "foo_dir_ignored_special_case"],
	}
	`
	dir1_foo_bp := `
	foo_module {
		name: "foo_dir1",
		deps: ["foo_dir_ignored"],
	}
	`
	dir_ignored_foo_bp := `
	foo_module {
		name: "foo_dir_ignored",
	}
	`
	dir_ignored_special_case_foo_bp := `
	foo_module {
		name: "foo_dir_ignored_special_case",
	}
	`
	mockFs := map[string][]byte{
		"Android.bp":                          []byte(root_foo_bp),
		"dir1/Android.bp":                     []byte(dir1_foo_bp),
		"dir_ignored/Android.bp":              []byte(dir_ignored_foo_bp),
		"dir_ignored/special_case/Android.bp": []byte(dir_ignored_special_case_foo_bp),
	}
	fileList := []string{}
	for f := range mockFs {
		fileList = append(fileList, f)
	}
	testCases := []struct {
		sourceRootDirs       []string
		expectedModuleDefs   []string
		unexpectedModuleDefs []string
		expectedErrs         []string
	}{
		{
			sourceRootDirs: []string{},
			expectedModuleDefs: []string{
				"foo",
				"foo_dir1",
				"foo_dir_ignored",
				"foo_dir_ignored_special_case",
			},
		},
		{
			sourceRootDirs: []string{"-", ""},
			unexpectedModuleDefs: []string{
				"foo",
				"foo_dir1",
				"foo_dir_ignored",
				"foo_dir_ignored_special_case",
			},
		},
		{
			sourceRootDirs: []string{"-"},
			unexpectedModuleDefs: []string{
				"foo",
				"foo_dir1",
				"foo_dir_ignored",
				"foo_dir_ignored_special_case",
			},
		},
		{
			sourceRootDirs: []string{"dir1"},
			expectedModuleDefs: []string{
				"foo",
				"foo_dir1",
				"foo_dir_ignored",
				"foo_dir_ignored_special_case",
			},
		},
		{
			sourceRootDirs: []string{"-dir1"},
			expectedModuleDefs: []string{
				"foo",
				"foo_dir_ignored",
				"foo_dir_ignored_special_case",
			},
			unexpectedModuleDefs: []string{
				"foo_dir1",
			},
			expectedErrs: []string{
				`Android.bp:2:2: module "foo" depends on skipped module "foo_dir1"; "foo_dir1" was defined in files(s) [dir1/Android.bp], but was skipped for reason(s) ["dir1/Android.bp" is a descendant of "dir1", and that path prefix was not included in PRODUCT_SOURCE_ROOT_DIRS]`,
			},
		},
		{
			sourceRootDirs: []string{"-", "dir1"},
			expectedModuleDefs: []string{
				"foo_dir1",
			},
			unexpectedModuleDefs: []string{
				"foo",
				"foo_dir_ignored",
				"foo_dir_ignored_special_case",
			},
			expectedErrs: []string{
				`dir1/Android.bp:2:2: module "foo_dir1" depends on skipped module "foo_dir_ignored"; "foo_dir_ignored" was defined in files(s) [dir_ignored/Android.bp], but was skipped for reason(s) ["dir_ignored/Android.bp" is a descendant of "", and that path prefix was not included in PRODUCT_SOURCE_ROOT_DIRS]`,
			},
		},
		{
			sourceRootDirs: []string{"-", "dir1", "dir_ignored/special_case/Android.bp"},
			expectedModuleDefs: []string{
				"foo_dir1",
				"foo_dir_ignored_special_case",
			},
			unexpectedModuleDefs: []string{
				"foo",
				"foo_dir_ignored",
			},
			expectedErrs: []string{
				"dir1/Android.bp:2:2: module \"foo_dir1\" depends on skipped module \"foo_dir_ignored\"; \"foo_dir_ignored\" was defined in files(s) [dir_ignored/Android.bp], but was skipped for reason(s) [\"dir_ignored/Android.bp\" is a descendant of \"\", and that path prefix was not included in PRODUCT_SOURCE_ROOT_DIRS]",
			},
		},
	}
	for _, tc := range testCases {
		t.Run(fmt.Sprintf(`source root dirs are %q`, tc.sourceRootDirs), func(t *testing.T) {
			ctx := NewContext()
			ctx.MockFileSystem(mockFs)
			ctx.RegisterModuleType("foo_module", newFooModule)
			ctx.RegisterBottomUpMutator("deps", depsMutator)
			ctx.AddSourceRootDirs(tc.sourceRootDirs...)
			ctx.ParseFileList(".", fileList, nil)
			_, actualErrs := ctx.ResolveDependencies(nil)

			stringErrs := []string(nil)
			for _, err := range actualErrs {
				stringErrs = append(stringErrs, err.Error())
			}
			if !reflect.DeepEqual(tc.expectedErrs, stringErrs) {
				t.Errorf("expected to find errors %v; got %v", tc.expectedErrs, stringErrs)
			}
			for _, modName := range tc.expectedModuleDefs {
				allMods := ctx.moduleGroupFromName(modName, nil)
				if allMods == nil || len(allMods.modules) != 1 {
					mods := moduleList{}
					if allMods != nil {
						mods = allMods.modules
					}
					t.Errorf("expected to find one definition for module %q, but got %v", modName, mods)
				}
			}

			for _, modName := range tc.unexpectedModuleDefs {
				allMods := ctx.moduleGroupFromName(modName, nil)
				if allMods != nil {
					t.Errorf("expected to find no definitions for module %q, but got %v", modName, allMods.modules)
				}
			}
		})
	}
}

func TestDisallowedMutatorMethods(t *testing.T) {
	testCases := []struct {
		name              string
		mutatorHandleFunc func(MutatorHandle)
		mutatorFunc       func(BottomUpMutatorContext)
		expectedPanic     string
	}{
		{
			name:              "rename",
			mutatorHandleFunc: func(handle MutatorHandle) { handle.UsesRename() },
			mutatorFunc:       func(ctx BottomUpMutatorContext) { ctx.Rename("qux") },
			expectedPanic:     "method Rename called from mutator that was not marked UsesRename",
		},
		{
			name:              "replace_dependencies",
			mutatorHandleFunc: func(handle MutatorHandle) { handle.UsesReplaceDependencies() },
			mutatorFunc:       func(ctx BottomUpMutatorContext) { ctx.ReplaceDependencies("bar") },
			expectedPanic:     "method ReplaceDependenciesIf called from mutator that was not marked UsesReplaceDependencies",
		},
		{
			name:              "replace_dependencies_if",
			mutatorHandleFunc: func(handle MutatorHandle) { handle.UsesReplaceDependencies() },
			mutatorFunc: func(ctx BottomUpMutatorContext) {
				ctx.ReplaceDependenciesIf("bar", func(from Module, tag DependencyTag, to Module) bool { return false })
			},
			expectedPanic: "method ReplaceDependenciesIf called from mutator that was not marked UsesReplaceDependencies",
		},
		{
			name:              "reverse_dependencies",
			mutatorHandleFunc: func(handle MutatorHandle) { handle.UsesReverseDependencies() },
			mutatorFunc:       func(ctx BottomUpMutatorContext) { ctx.AddReverseDependency(ctx.Module(), nil, "baz") },
			expectedPanic:     "method AddReverseDependency called from mutator that was not marked UsesReverseDependencies",
		},
		{
			name:              "create_module",
			mutatorHandleFunc: func(handle MutatorHandle) { handle.UsesCreateModule() },
			mutatorFunc: func(ctx BottomUpMutatorContext) {
				ctx.CreateModule(newFooModule, "create_module",
					&struct{ Name string }{Name: "quz"})
			},
			expectedPanic: "method CreateModule called from mutator that was not marked UsesCreateModule",
		},
	}

	runTest := func(mutatorHandleFunc func(MutatorHandle), mutatorFunc func(ctx BottomUpMutatorContext), expectedPanic string) {
		ctx := NewContext()

		ctx.MockFileSystem(map[string][]byte{
			"Android.bp": []byte(`
			foo_module {
				name: "foo",
			}

			foo_module {
				name: "bar",
				deps: ["foo"],
			}

			foo_module {
				name: "baz",
			}
		`)})

		ctx.RegisterModuleType("foo_module", newFooModule)
		ctx.RegisterBottomUpMutator("deps", depsMutator)
		handle := ctx.RegisterBottomUpMutator("mutator", func(ctx BottomUpMutatorContext) {
			if ctx.ModuleName() == "foo" {
				mutatorFunc(ctx)
			}
		})
		mutatorHandleFunc(handle)

		_, errs := ctx.ParseBlueprintsFiles("Android.bp", nil)
		if len(errs) > 0 {
			t.Errorf("unexpected parse errors:")
			for _, err := range errs {
				t.Errorf("  %s", err)
			}
			t.FailNow()
		}

		_, errs = ctx.ResolveDependencies(nil)
		if expectedPanic != "" {
			if len(errs) == 0 {
				t.Errorf("missing expected error %q", expectedPanic)
			} else if !strings.Contains(errs[0].Error(), expectedPanic) {
				t.Errorf("missing expected error %q in %q", expectedPanic, errs[0].Error())
			}
		} else if len(errs) > 0 {
			t.Errorf("unexpected dep errors:")
			for _, err := range errs {
				t.Errorf("  %s", err)
			}
			t.FailNow()
		}
	}

	noopMutatorHandleFunc := func(MutatorHandle) {}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Run("allowed", func(t *testing.T) {
				// Test that the method doesn't panic when the handle function is called.
				runTest(testCase.mutatorHandleFunc, testCase.mutatorFunc, "")
			})
			t.Run("disallowed", func(t *testing.T) {
				// Test that the method does panic with the expected error when the
				// handle function is not called.
				runTest(noopMutatorHandleFunc, testCase.mutatorFunc, testCase.expectedPanic)
			})
		})
	}

}

func Benchmark_parallelVisit(b *testing.B) {
	b.ReportAllocs()
	create := func(name string) *moduleInfo {
		m := &moduleInfo{
			group: &moduleGroup{
				name: name,
			},
		}
		m.group.modules = moduleList{m}
		return m
	}

	addDep := func(from, to *moduleInfo) {
		from.directDeps = append(from.directDeps, depInfo{to, nil})
		from.forwardDeps = append(from.forwardDeps, to)
		to.reverseDeps = append(to.reverseDeps, from)
	}
	_ = addDep

	var modules []*moduleInfo

	for i := range b.N {
		modules = append(modules, create(strconv.Itoa(i)))
		if i != 0 {
			//addDep(modules[len(modules)-1], modules[len(modules)-2])
		}
	}

	b.ResetTimer()
	errs := parallelVisit(slices.Values(modules), bottomUpVisitorImpl{}, 1000,
		func(module *moduleInfo, pause pauseFunc) bool {
			//fmt.Println(module.group.name)
			return false
		})
	if errs != nil {
		b.Errorf("expected no errors, got %q", errs)
	}
}

func Benchmark_parallelVisitAllVsSubset(b *testing.B) {
	const moduleCount = 16384
	const subsetCount = 256

	modules := make([]*moduleInfo, moduleCount)
	for i := range modules {
		module := &moduleInfo{group: &moduleGroup{name: strconv.Itoa(i)}}
		module.group.modules = moduleList{module}
		modules[i] = module
	}
	subset := slices.Clone(modules[:subsetCount])
	visit := func(*moduleInfo, pauseFunc) bool { return false }

	b.Run("all", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if errs := parallelVisit(slices.Values(modules), bottomUpVisitorImpl{}, 64, visit); len(errs) > 0 {
				b.Fatal(errs)
			}
		}
	})
	b.Run("subset", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if errs := parallelVisitSubset(subset, bottomUpVisitorImpl{}, 64, visit); len(errs) > 0 {
				b.Fatal(errs)
			}
		}
	})
}
