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
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/ioutil"
	"iter"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/pprof"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/scanner"
	"text/template"
	"unsafe"

	"github.com/google/blueprint/gobtools"
	"github.com/google/blueprint/metrics"
	"github.com/google/blueprint/parser"
	"github.com/google/blueprint/pathtools"
	"github.com/google/blueprint/pool"
	"github.com/google/blueprint/proptools"
	"github.com/google/blueprint/syncmap"
	"github.com/google/blueprint/uniquelist"
)

//go:generate go run ./gobtools/codegen

var ErrBuildActionsNotReady = errors.New("build actions are not ready")

const maxErrors = 10
const MockModuleListFile = "bplist"

const OutFilePermissions = 0666

const BuildActionsCacheFile = "build_actions.gob"
const OrderOnlyStringsCacheFile = "order_only_strings.gob"

// sandboxConfig is an interface for config objects that can report if the
// build is action sandboxed.
type sandboxConfig interface {
	IsActionSandboxedBuild() bool
	ActionSandboxMetrics() *SandboxMetrics
}

type SandboxedAction struct {
	Name      string
	Sandboxed bool
}

// SandboxMetrics tracks the total number of rules and action sandboxing disabled
// (i.e. opted-out) rules in action sandboxed builds
type SandboxMetrics struct {
	deduper       sync.Map
	totalRules    int64
	disabledRules int64
	actions       []SandboxedAction
}

func (s *SandboxMetrics) updateSandboxMetrics(name string, source string, isSandboxDisabled bool) {
	if source != "" {
		if _, ok := s.deduper.LoadOrStore(source, true); ok {
			return
		}
		name = source
	}
	s.totalRules += 1
	if isSandboxDisabled {
		s.disabledRules += 1
	}
	s.actions = append(s.actions, SandboxedAction{
		Name:      name,
		Sandboxed: !isSandboxDisabled,
	})
}

func (s *SandboxMetrics) TotalRules() int64 {
	return s.totalRules
}

func (s *SandboxMetrics) DisabledRules() int64 {
	return s.disabledRules
}

func (s *SandboxMetrics) Actions() []SandboxedAction {
	return s.actions
}

// A Context contains all the state needed to parse a set of Blueprints files
// and generate a Ninja file.  The process of generating a Ninja file proceeds
// through a series of four phases.  Each phase corresponds with a some methods
// on the Context object
//
//	      Phase                            Methods
//	   ------------      -------------------------------------------
//	1. Registration         RegisterModuleType, RegisterSingletonType
//
//	2. Parse                    ParseBlueprintsFiles, Parse
//
//	3. Generate            ResolveDependencies, PrepareBuildActions
//
//	4. Write                           WriteBuildFile
//
// The registration phase prepares the context to process Blueprints files
// containing various types of modules.  The parse phase reads in one or more
// Blueprints files and validates their contents against the module types that
// have been registered.  The generate phase then analyzes the parsed Blueprints
// contents to create an internal representation for the build actions that must
// be performed.  This phase also performs validation of the module dependencies
// and property values defined in the parsed Blueprints files.  Finally, the
// write phase generates the Ninja manifest text based on the generated build
// actions.
type Context struct {
	context.Context

	// Used for metrics-related event logging.
	EventHandler *metrics.EventHandler

	BeforePrepareBuildActionsHook func() error

	moduleFactories     map[string]ModuleFactory
	nameInterface       NameInterface
	moduleGroups        []*moduleGroup
	singletonInfo       []*singletonInfo
	mutatorInfo         []*mutatorInfo
	variantMutatorNames []string

	completedTransitionMutators int
	transitionMutators          []*transitionMutatorImpl
	transitionMutatorNames      []string

	needsUpdateDependencies uint32 // positive if a mutator modified the dependencies

	dependenciesReady bool // set to true on a successful ResolveDependencies
	buildActionsReady bool // set to true on a successful PrepareBuildActions

	// set by SetIgnoreUnknownModuleTypes
	ignoreUnknownModuleTypes bool

	// set by SetAllowMissingDependencies
	allowMissingDependencies bool

	// set during PrepareBuildActions
	nameTracker     *nameTracker
	liveGlobals     *liveTracker
	globalVariables map[Variable]*ninjaString
	globalPools     map[Pool]*poolDef
	globalRules     map[Rule]*ruleDef

	// set during PrepareBuildActions
	outDir             *ninjaString // The builddir special Ninja variable
	requiredNinjaMajor int          // For the ninja_required_version variable
	requiredNinjaMinor int          // For the ninja_required_version variable
	requiredNinjaMicro int          // For the ninja_required_version variable

	subninjas []string

	// set lazily by sortedModuleGroups
	cachedSortedModuleGroups []*moduleGroup
	// cache deps modified to determine whether cachedSortedModuleGroups needs to be recalculated
	cachedDepsModified bool

	globs syncmap.SyncMap[globKey, globsMapEntry]

	restoredGlobsFromCache map[globKey]pathtools.GlobResult
	restoredGlobMetrics    pathtools.RestoredGlobsMetrics

	srcDir           string
	incrementalDBDir string
	fs               pathtools.FileSystem
	moduleListFile   string

	// Mutators indexed by the ID of the provider associated with them.  Not all mutators will
	// have providers, and not all providers will have a mutator, or if they do the mutator may
	// not be registered in this Context.
	providerMutators []*mutatorInfo

	// True for the index of any mutators that have already run over all modules
	finishedMutators []bool

	// If true, RunBlueprint will skip cloning modules at the end of RunBlueprint.
	// Cloning modules intentionally invalidates some Module values after
	// mutators run (to ensure that mutators don't set such Module values in a way
	// which ruins the integrity of the graph). However, keeping Module values
	// changed by mutators may be a desirable outcome (such as for tooling or tests).
	SkipCloneModulesAfterMutators bool

	// String values that can be used to gate build graph traversal
	includeTags *IncludeTags

	sourceRootDirs *SourceRootDirs

	// True if an incremental analysis can be attempted, i.e., there is no Soong
	// code changes, no environmental variable changes and no product config
	// variable changes.
	incrementalAnalysis bool

	// True if the flag --incremental-build-actions is set, in which case Soong
	// will try to do a incremental build. Mainly two tasks will involve here:
	// caching the providers of all the participating modules, and restoring the
	// providers and skip the build action generations if there is a cache hit.
	// Enabling this flag will only guarantee the former task to be performed, the
	// latter will depend on the flag above.
	incrementalEnabled bool

	incrementalProviderTest bool

	keyValueStoreCache      *KeyValueStoreCache
	buildActionsToCacheLock sync.Mutex
	orderOnlyStringsCache   OrderOnlyStringsCache
	orderOnlyStrings        syncmap.SyncMap[uniquelist.UniqueList[string], *orderOnlyStringsInfo]
	incrementalDebugFile    string
	EncContext              gobtools.EncContext
	providerValueHashes     []proptools.Hash

	moduleDebugDataChannel chan []byte

	captureBuildParams bool

	// index of the last mutator which uses `CreateModule`.
	mutatorIndexAfterLastCreateModule int

	// If splitAllVariants is true, all variants will be created upfront rather than on-demand.
	splitAllVariants bool

	// index of the first mutator that supports partial analysis.
	mutatorIndexPartialAnalysis int
	partialAnalysisTargets      []string
}

type orderOnlyStringsInfo struct {
	dedup       bool
	incremental bool
	dedupName   string
}

// A container for String keys. The keys can be used to gate build graph traversal
type SourceRootDirs struct {
	dirs []string
}

func (dirs *SourceRootDirs) Add(names ...string) {
	dirs.dirs = append(dirs.dirs, names...)
}

func (dirs *SourceRootDirs) SourceRootDirAllowed(path string) (bool, string) {
	sort.Slice(dirs.dirs, func(i, j int) bool {
		return len(dirs.dirs[i]) < len(dirs.dirs[j])
	})
	last := len(dirs.dirs)
	for i := range dirs.dirs {
		// iterate from longest paths (most specific)
		prefix := dirs.dirs[last-i-1]
		disallowedPrefix := false
		if len(prefix) >= 1 && prefix[0] == '-' {
			prefix = prefix[1:]
			disallowedPrefix = true
		}
		if strings.HasPrefix(path, prefix) {
			if disallowedPrefix {
				return false, prefix
			} else {
				return true, prefix
			}
		}
	}
	return true, ""
}

func (c *Context) AddSourceRootDirs(dirs ...string) {
	c.sourceRootDirs.Add(dirs...)
}

// A container for String keys. The keys can be used to gate build graph traversal
type IncludeTags map[string]bool

func (tags *IncludeTags) Add(names ...string) {
	for _, name := range names {
		(*tags)[name] = true
	}
}

func (tags *IncludeTags) Contains(tag string) bool {
	_, exists := (*tags)[tag]
	return exists
}

func (c *Context) AddIncludeTags(names ...string) {
	c.includeTags.Add(names...)
}

func (c *Context) ContainsIncludeTag(name string) bool {
	return c.includeTags.Contains(name)
}

// iterateAllVariants returns an iter.Seq that iterates over every variant of every module.
func (c *Context) iterateAllVariants() iter.Seq[*moduleInfo] {
	return func(yield func(*moduleInfo) bool) {
		for _, group := range c.moduleGroups {
			for _, module := range group.modules {
				if !yield(module) {
					return
				}
			}
		}
	}
}

// An Error describes a problem that was encountered that is related to a
// particular location in a Blueprints file.
type BlueprintError struct {
	Err error            // the error that occurred
	Pos scanner.Position // the relevant Blueprints file location
}

// A ModuleError describes a problem that was encountered that is related to a
// particular module in a Blueprints file
type ModuleError struct {
	BlueprintError
	module *moduleInfo
}

// A PropertyError describes a problem that was encountered that is related to a
// particular property in a Blueprints file
type PropertyError struct {
	ModuleError
	property string
}

func (e *BlueprintError) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Err)
}

func (e *ModuleError) Error() string {
	return fmt.Sprintf("%s: %s: %s", e.Pos, e.module, e.Err)
}

func (e *PropertyError) Error() string {
	return fmt.Sprintf("%s: %s: %s: %s", e.Pos, e.module, e.property, e.Err)
}

type localBuildActions struct {
	variables []*localVariable
	rules     []*localRule
	buildDefs []*buildDef
}

type moduleList []*moduleInfo

func (l moduleList) firstModule() *moduleInfo {
	if len(l) > 0 {
		return l[0]
	}
	panic(fmt.Errorf("no first module!"))
}

func (l moduleList) lastModule() *moduleInfo {
	if len(l) > 0 {
		return l[len(l)-1]
	}
	panic(fmt.Errorf("no last module!"))
}

type moduleGroup struct {
	name string

	modules moduleList

	namespace Namespace

	// A partial copy of moduleInfo at the end of defaults mutator.
	// common across all variants.
	coreModuleInfo moduleInfo

	// used for creating on demand variants.
	variantOnDemandLock sync.Mutex

	// Additional variations supported by this module group that were not
	// created by Split.
	supportedVariantsOnDemand map[string]TransitionInfos

	// Map of requested variation map to on-demand variants.
	// Set to empty at the end of mutator.
	cachedVariantsOnDemand map[string]*moduleInfo

	// Indicates the module group is not yet in the build graph.
	passive bool
}

func (group *moduleGroup) moduleByVariantName(name string) *moduleInfo {
	for _, module := range group.modules {
		if module.variant.name == name {
			return module
		}
	}
	return nil
}

// registerSupportedVariants registers the supported variants, but does not create them
func (group *moduleGroup) registerSupportedVariants(mutatorName string, infos TransitionInfos) {
	if len(infos) == 0 {
		return
	}
	group.variantOnDemandLock.Lock()
	defer group.variantOnDemandLock.Unlock()
	if group.supportedVariantsOnDemand == nil {
		group.supportedVariantsOnDemand = map[string]TransitionInfos{}
	}
	if _, exists := group.supportedVariantsOnDemand[mutatorName]; exists {
		return
	}
	group.supportedVariantsOnDemand[mutatorName] = append(group.supportedVariantsOnDemand[mutatorName], infos...)
}

// searchOnDemandVariant returns true if an on-demand variant creation should be attempted.
// It uses the following heuristics.
// 1. SplitOnDemand is non-nil for any mutator that precedes the Add.*Dependency request.
// 2. No transition has occurred yet
//
// Feasibility of on-demand variant request will be determined subsequently by using `applyTransitions`
// across a sliding window on the on-demand variant.
func (group *moduleGroup) searchOnDemandVariant(onDemandVariants variationMap, far bool, atMutatorIndex int, transitionMutators []*transitionMutatorImpl) bool {
	for _, mutator := range transitionMutators {
		if atMutatorIndex >= mutator.mutatorIndex {
			if _, exists := group.supportedVariantsOnDemand[mutator.name]; exists {
				return true
			}
		}
	}

	if len(transitionMutators) > 0 && atMutatorIndex < transitionMutators[0].mutatorIndex {
		return true
	}
	return false
}

// loadOrCreateVariantOnDemand returns an on-demand variant (if supported) or nil.
// It reruns the completed transitions, applying Outgoing/Incoming transition to resolve the requested variant to a final variant.
//
// To reduce duplicate reruns, it stores the results in a map.
func (c *Context) loadOrCreateVariantOnDemand(config any, module *moduleInfo, depTag DependencyTag, possibleDeps *moduleGroup, variant variationMap,
	requestedVariations []Variation, far bool, moduleOutgoingTransitionInfos []TransitionInfo) (*moduleInfo, []error) {
	createVariantOnDemand := func() (*moduleInfo, []error) {
		var variantOnDemand *moduleInfo
		var errs []error
		onDemandVariantForIncomingTransition := c.createVariantOnDemand(possibleDeps, variant)
		_, _, errs = c.applyTransitions(config, module, depTag, possibleDeps, variant, requestedVariations, far, onDemandVariantForIncomingTransition)
		if len(errs) > 0 || onDemandVariantForIncomingTransition.createdOnDemandIncompatible {
			variantOnDemand = nil
		} else {
			variantOnDemand = c.createVariantOnDemand(possibleDeps, onDemandVariantForIncomingTransition.requestedOnDemandVariant.clone())
		}

		return variantOnDemand, errs

	}
	if module.group == possibleDeps {
		// inter-variant dep can cause a deadlock, so always compute.
		return createVariantOnDemand()
	}

	var variantSb strings.Builder
	for _, mutator := range slices.Sorted(maps.Keys(variant.variations)) {
		variantSb.WriteString("-")
		variantSb.WriteString(variant.variations[mutator])
	}
	for _, mutator := range slices.Sorted(maps.Keys(module.requestedOnDemandVariant.variations)) {
		variantSb.WriteString("-")
		variantSb.WriteString(variant.variations[mutator])
	}
	for _, oti := range moduleOutgoingTransitionInfos {
		if oti != nil && oti.Variation() != "" {
			variantSb.WriteString("-")
			variantSb.WriteString(oti.Variation())
		}
	}

	possibleDeps.variantOnDemandLock.Lock()
	defer possibleDeps.variantOnDemandLock.Unlock()
	if possibleDeps.cachedVariantsOnDemand == nil {
		possibleDeps.cachedVariantsOnDemand = make(map[string]*moduleInfo)
	}
	// Match found
	if cached, exists := possibleDeps.cachedVariantsOnDemand[variantSb.String()]; exists {
		return cached, nil
	}
	// No match found
	variantOnDemand, errs := createVariantOnDemand()
	possibleDeps.cachedVariantsOnDemand[variantSb.String()] = variantOnDemand
	return variantOnDemand, errs
}

// createVariantOnDemand uses group.coreModuleInfo.factory() to create an "empty" variant on demand.
// `runMutator` will be responsible for populating this variant using cloneLogicModule
// and creating the dependency edges to this on demand variant.
func (c *Context) createVariantOnDemand(group *moduleGroup, onDemandVariants variationMap) *moduleInfo {

	newlogicmodule, newproperties := group.coreModuleInfo.factory()
	newmodule := moduleInfo{
		logicModule:              newlogicmodule,
		properties:               newproperties,
		factory:                  group.coreModuleInfo.factory,
		group:                    group,
		createdOnDemand:          true,
		requestedOnDemandVariant: onDemandVariants,
		relBlueprintsFile:        group.coreModuleInfo.relBlueprintsFile,
		pos:                      group.coreModuleInfo.pos,
	}
	newmodule.createdOnDemandReplaceWith = &newmodule
	newlogicmodule.setInfo(&newmodule)

	return &newmodule
}

// Initialize the properties of the on demand variant and
// re-run the completed mutators on this newly created module.
func (c *Context) rerunMutatorsOnVariantOnDemand(newmodule *moduleInfo, fromMutatorIndex, tillMutatorIndex int, p pauseFunc, config interface{}) {
	pause := func(dep *moduleInfo) {
		if dep.createdOnDemand {
			// Transitive variant on demand.
			// Set `requestedOnDemandVariant` on this transitive dep.
			// This will be used in the main coordinator goroutine to create the correct transition for this variant.
			for _, transitionMutator := range c.transitionMutators[:c.completedTransitionMutators] {
				if transitionMutator.mutatorIndex >=
					dep.finishedMutator+3 { // TODO (b/448182248): Revisit partially transitioned on-demand variants.
					// Copy requestedOnDemandVariant from rdep to dep on-demand modules.
					// This will be done only for the transition mutators that have not yet
					// been completed in rerunMutator.
					// variation map for earlier transition mutators will be resolved by `findVariant`.
					//
					// TODO (spandandas): Does this handle transition mutators between
					// newmodule.finishedMutator and c.completedTransitionMutators?
					dep.requestedOnDemandVariant.set(transitionMutator.name, newmodule.requestedOnDemandVariant.get(transitionMutator.name))
				}
			}
		}
		if p != nil {
			p(dep)
		}
	}

	// initialize the properties from coreModuleInfo.
	if fromMutatorIndex == c.mutatorIndexAfterLastCreateModule {
		newlogicmodule, newproperties := c.cloneLogicModule(&newmodule.group.coreModuleInfo)
		newlogicmodule.setInfo(newmodule)
		newmodule.logicModule = newlogicmodule
		newmodule.properties = newproperties
	}

	for index, mi := range c.mutatorInfo {
		if index < fromMutatorIndex {
			continue
		}
		newmodule.startedMutator = index
		mctx := &mutatorContext{
			baseModuleContext: baseModuleContext{
				context: c,
				config:  config,
				module:  newmodule,
			},
			mutator:   mi,
			pauseFunc: pause,
		}
		if mi.transitionPropagateMutator != nil {
			mi.transitionPropagateMutator(mctx)
		} else {
			mctx.mutator = mi
			mi.bottomUpMutator(mctx)
		}
		newmodule.finishedMutator = index
		if len(mctx.reverseDeps) > 0 || len(mctx.replace) > 0 || len(mctx.rename) > 0 {
			panic("TODO (b/448182009): Add support for AddReverseDependency, ReplaceDependency, Rename")
		}

		// The module has been mutated till the current mutator.
		// Do not mutate further.
		if index == tillMutatorIndex {
			break
		}

		// Err
		if newmodule.createdOnDemandIncompatible {
			break
		}
	}
}

type moduleInfo struct {
	// set during Parse
	typeName          string
	factory           ModuleFactory
	relBlueprintsFile string
	pos               scanner.Position
	propertyPos       map[string]scanner.Position
	createdBy         *moduleInfo

	variant variant

	logicModule Module
	group       *moduleGroup
	properties  []interface{}

	// set during ResolveDependencies
	missingDeps            []string
	newDirectDeps          []*moduleInfo
	newOnDemandReverseDeps []reverseDep

	// set during updateDependencies
	reverseDeps []*moduleInfo
	forwardDeps []*moduleInfo
	directDeps  []depInfo

	// used by parallelVisit
	waitingCount atomic.Int32

	// set during each runMutator
	splitModules           moduleList
	obsoletedByNewVariants bool

	// Used by TransitionMutator implementations

	// incomingTransitionInfos stores the map from variation to TransitionInfo object for transitions that were
	// requested by reverse dependencies.  It is updated by reverse dependencies and protected by
	// incomingTransitionInfosLock.  It is invalid after the TransitionMutator top down mutator has run on
	// this module.
	incomingTransitionInfos     map[string]TransitionInfo
	incomingTransitionInfosLock sync.Mutex
	// splitTransitionInfos and splitTransitionVariations stores the list of TransitionInfo objects, and their
	// corresponding variations, returned by Split or requested by reverse dependencies.  They are valid after the
	// TransitionMutator top down mutator has run on this module, and invalid after the bottom up mutator has run.
	splitTransitionInfos      []TransitionInfo
	splitTransitionVariations []string
	currentTransitionMutator  string

	// transitionInfos stores the final TransitionInfo for this module indexed by transitionMutatorImpl.index
	transitionInfos []TransitionInfo

	// outgoingTransitionCache stores the final variation for each dependency, indexed by the source variation
	// index in splitTransitionInfos and then by the index of the dependency in directDeps
	outgoingTransitionCache [][]string

	// set during PrepareBuildActions
	actionDefs  localBuildActions
	buildParams *[]BuildParams

	startedMutator  int
	finishedMutator int

	startedGenerateBuildActions  bool
	finishedGenerateBuildActions bool

	// freeAfterGenerateBuildActions is set if the module called ModuleContext.FreeModuleAfterGenerateBuildActions,
	// allowing the Module to be freed after GenerateBuildActions complete, and requiring all future accesses
	// to go through ModuleProxy instead of the Module.
	freeAfterGenerateBuildActions bool
	// cachedName stores the result of Module.Name() after the end of GenerateBuildActions for use in ModuleProxy.Name()
	cachedName string
	// cachedString stores the result of Module.String() after the end of GenerateBuildActions for use in ModuleProxy.String().
	cachedString string
	// cachedUniqueName stores the result of UniqueName after the end of GenerateBuildActions for use when sorting
	// modules.
	cachedUniqueName string

	moduleIncrementalInfo

	createdOnDemand            bool
	createdOnDemandReplaceWith *moduleInfo
	requestedOnDemandVariant   variationMap
	// Properties used to determine whether a requested on-demand variant can be created.
	createdOnDemandIncompatible    bool
	createdOnDemandSupportedSplits []TransitionInfo
	// Index used for sorting primary and on-demand variants. Special-cased to android's image mutator for now.
	sortIndex int
}

type providerInfo struct {
	providers                  []interface{}
	providerInitialValueHashes []proptools.Hash
}

// @auto-generate: gob
type globResultCache struct {
	Pattern  string
	Excludes []string
	Result   proptools.Hash
}

func (g *globResultCache) equal(other globResultCache) bool {
	return g.Result == other.Result && g.Pattern == other.Pattern && slices.Equal(g.Excludes, other.Excludes)
}

type moduleIncrementalInfo struct {
	commonIncrementalInfo
	buildActionInputHash proptools.Hash
	orderOnlyStrings     []string
	incrementalDebugInfo []byte

	// providersHash is the hash of the providers set by this module
	providersHash proptools.Hash
	// transitiveProvidersHash is the hash of the providers set by
	// this module and all transitive dependencies of this module.
	transitiveProvidersHash proptools.Hash
}

type commonIncrementalInfo struct {
	incrementalRestored bool
	// hasUnrestoredProvider is true when the module has been restored from the cache and the provider
	// (indexed in the same order as providerRegistry) exists in the cache but has not yet been
	// restored.
	hasUnrestoredProvider []bool
	providerRestoreLock   sync.Mutex
	buildActionCacheKey   *DataCacheKey
	globCache             []globResultCache
	providerInfo
}

type variant struct {
	name       string
	variations variationMap
}

type depInfo struct {
	module *moduleInfo
	tag    DependencyTag
}

func (module *moduleInfo) Name() string {
	// If this is called from a LoadHook (which is run before the module has been registered)
	// then group will not be set and so the name is retrieved from logicModule.Name().
	// Usually, using that method is not safe as it does not track renames (group.name does).
	// However, when called from LoadHook it is safe as there is no way to rename a module
	// until after the LoadHook has run and the module has been registered.
	if module.group != nil {
		return module.group.name
	} else {
		return module.logicModule.Name()
	}
}

func (module *moduleInfo) String() string {
	s := fmt.Sprintf("module %q", module.Name())
	if module.variant.name != "" {
		s += fmt.Sprintf(" variant %q", module.variant.name)
	}
	if module.createdBy != nil {
		s += fmt.Sprintf(" (created by %s)", module.createdBy)
	}

	return s
}

func (module *moduleInfo) namespace() Namespace {
	return module.group.namespace
}

func (module *moduleInfo) moduleCacheKey() string {
	variant := module.variant.name
	if variant == "" {
		variant = "none"
	}
	return calculateFileNameHash(fmt.Sprintf("%s-%s-%s-%s",
		filepath.Dir(module.relBlueprintsFile), module.cachedUniqueName, variant, module.typeName))
}

// @auto-generate: gob
type stringHash struct {
	string
}

func calculateFileNameHash(name string) string {
	hash, err := proptools.CalculateHash(stringHash{name})
	if err != nil {
		panic(newPanicErrorf(err, "failed to calculate hash for file name: %s", name))
	}
	return hash.FormatUint(16)
}

func (c *Context) setModuleTransitionInfo(module *moduleInfo, t *transitionMutatorImpl, info TransitionInfo) {
	if len(module.transitionInfos) == 0 {
		module.transitionInfos = make([]TransitionInfo, len(c.transitionMutators))
	}
	module.transitionInfos[t.index] = info
}

// A Variation is a way that a variant of a module differs from other variants of the same module.
// For example, two variants of the same module might have Variation{"arch","arm"} and
// Variation{"arch","arm64"}
// @auto-generate: gob
type Variation struct {
	// Mutator is the axis on which this variation applies, i.e. "arch" or "link"
	Mutator string
	// Variation is the name of the variation on the axis, i.e. "arm" or "arm64" for arch, or
	// "shared" or "static" for link.
	Variation string
}

// A variationMap stores a map of Mutator to Variation to specify a variant of a module.
type variationMap struct {
	variations map[string]string
}

func (vm variationMap) clone() variationMap {
	return variationMap{
		variations: maps.Clone(vm.variations),
	}
}

func (vm variationMap) cloneMatching(mutators []string) variationMap {
	newVariations := make(map[string]string)
	for _, mutator := range mutators {
		if variation, ok := vm.variations[mutator]; ok {
			newVariations[mutator] = variation
		}
	}
	return variationMap{
		variations: newVariations,
	}
}

// Compare this variationMap to another one.  Returns true if the every entry in this map
// exists and has the same value in the other map.
func (vm variationMap) subsetOf(other variationMap) bool {
	for k, v1 := range vm.variations {
		if v2, ok := other.variations[k]; !ok || v1 != v2 {
			return false
		}
	}
	return true
}

func (vm variationMap) equal(other variationMap) bool {
	return maps.Equal(vm.variations, other.variations)
}

func (vm *variationMap) set(mutator, variation string) {
	if variation == "" {
		if vm.variations != nil {
			delete(vm.variations, mutator)
		}
	} else {
		if vm.variations == nil {
			vm.variations = make(map[string]string)
		}
		vm.variations[mutator] = variation
	}
}

func (vm variationMap) get(mutator string) string {
	return vm.variations[mutator]
}

func (vm variationMap) delete(mutator string) {
	delete(vm.variations, mutator)
}

func (vm variationMap) empty() bool {
	return len(vm.variations) == 0
}

// differenceKeysCount returns the count of keys that exist in this variationMap that don't exist in the argument.  It
// ignores the values.
func (vm variationMap) differenceKeysCount(other variationMap) int {
	divergence := 0
	for mutator, _ := range vm.variations {
		if _, exists := other.variations[mutator]; !exists {
			divergence += 1
		}
	}
	return divergence
}

type singletonInfo struct {
	// set during RegisterSingletonType
	factory   SingletonFactory
	singleton Singleton
	name      string
	parallel  bool

	// set during PrepareBuildActions
	actionDefs                   localBuildActions
	subninjas                    []string
	startedGenerateBuildActions  bool
	finishedGenerateBuildActions bool
	commonIncrementalInfo
	// Whether this singleton supports incremental build.
	incrementalSupported bool
	// The provider hashes of all the singletons that this singleton might depend on.
	// These values are calculated before calling the GenerateBuildAction of the current
	// singleton, and combined with the hashes of all the module providers that this
	// singleton might depend on, we can decide if the input of the GenerateBuildAction
	// has any change, and skip the execution of it if there is no change.
	providerValueHashes []proptools.Hash
}

type mutatorInfo struct {
	// set during RegisterMutator
	transitionPropagateMutator func(BaseModuleContext)
	bottomUpMutator            BottomUpMutator
	name                       string
	index                      int
	transitionMutator          *transitionMutatorImpl

	usesRename              bool
	usesReverseDependencies bool
	usesReplaceDependencies bool
	usesCreateModule        bool
	mutatesDependencies     bool
	mutatesGlobalState      bool
	prePartial              bool
}

func newContext() *Context {
	eventHandler := metrics.EventHandler{}
	return &Context{
		Context:               context.Background(),
		EventHandler:          &eventHandler,
		moduleFactories:       make(map[string]ModuleFactory),
		nameInterface:         NewSimpleNameInterface(),
		fs:                    pathtools.OsFs,
		includeTags:           &IncludeTags{},
		sourceRootDirs:        &SourceRootDirs{},
		outDir:                nil,
		requiredNinjaMajor:    1,
		requiredNinjaMinor:    7,
		requiredNinjaMicro:    0,
		orderOnlyStringsCache: make(OrderOnlyStringsCache),
		orderOnlyStrings:      syncmap.SyncMap[uniquelist.UniqueList[string], *orderOnlyStringsInfo]{},
	}
}

// NewContext creates a new Context object.  The created context initially has
// no module or singleton factories registered, so the RegisterModuleFactory and
// RegisterSingletonFactory methods must be called before it can do anything
// useful.
func NewContext() *Context {
	ctx := newContext()

	ctx.RegisterBottomUpMutator("blueprint_deps", blueprintDepsMutator)

	return ctx
}

// A ModuleFactory function creates a new Module object.  See the
// Context.RegisterModuleType method for details about how a registered
// ModuleFactory is used by a Context.
type ModuleFactory func() (m Module, propertyStructs []interface{})

// RegisterModuleType associates a module type name (which can appear in a
// Blueprints file) with a Module factory function.  When the given module type
// name is encountered in a Blueprints file during parsing, the Module factory
// is invoked to instantiate a new Module object to handle the build action
// generation for the module.  If a Mutator splits a module into multiple variants,
// the factory is invoked again to create a new Module for each variant.
//
// The module type names given here must be unique for the context.  The factory
// function should be a named function so that its package and name can be
// included in the generated Ninja file for debugging purposes.
//
// The factory function returns two values.  The first is the newly created
// Module object.  The second is a slice of pointers to that Module object's
// properties structs.  Each properties struct is examined when parsing a module
// definition of this type in a Blueprints file.  Exported fields of the
// properties structs are automatically set to the property values specified in
// the Blueprints file.  The properties struct field names determine the name of
// the Blueprints file properties that are used - the Blueprints property name
// matches that of the properties struct field name with the first letter
// converted to lower-case.
//
// The fields of the properties struct must be either []string, a string, or
// bool. The Context will panic if a Module gets instantiated with a properties
// struct containing a field that is not one these supported types.
//
// Any properties that appear in the Blueprints files that are not built-in
// module properties (such as "name" and "deps") and do not have a corresponding
// field in the returned module properties struct result in an error during the
// Context's parse phase.
//
// As an example, the follow code:
//
//	type myModule struct {
//	    properties struct {
//	        Foo string
//	        Bar []string
//	    }
//	}
//
//	func NewMyModule() (blueprint.Module, []interface{}) {
//	    module := new(myModule)
//	    properties := &module.properties
//	    return module, []interface{}{properties}
//	}
//
//	func main() {
//	    ctx := blueprint.NewContext()
//	    ctx.RegisterModuleType("my_module", NewMyModule)
//	    // ...
//	}
//
// would support parsing a module defined in a Blueprints file as follows:
//
//	my_module {
//	    name: "myName",
//	    foo:  "my foo string",
//	    bar:  ["my", "bar", "strings"],
//	}
//
// The factory function may be called from multiple goroutines.  Any accesses
// to global variables must be synchronized.
func (c *Context) RegisterModuleType(name string, factory ModuleFactory) {
	if _, present := c.moduleFactories[name]; present {
		panic(fmt.Errorf("module type %q is already registered", name))
	}
	c.moduleFactories[name] = factory
}

// A SingletonFactory function creates a new Singleton object.  See the
// Context.RegisterSingletonType method for details about how a registered
// SingletonFactory is used by a Context.
type SingletonFactory func() Singleton

// RegisterSingletonType registers a singleton type that will be invoked to
// generate build actions.  Each registered singleton type is instantiated
// and invoked exactly once as part of the generate phase.
//
// Those singletons registered with parallel=true are run in parallel, after
// which the other registered singletons are run in registration order.
//
// The singleton type names given here must be unique for the context.  The
// factory function should be a named function so that its package and name can
// be included in the generated Ninja file for debugging purposes.
func (c *Context) RegisterSingletonType(name string, factory SingletonFactory, parallel bool) {
	for _, s := range c.singletonInfo {
		if s.name == name {
			panic(fmt.Errorf("singleton %q is already registered", name))
		}
	}

	c.singletonInfo = append(c.singletonInfo, &singletonInfo{
		factory:   factory,
		singleton: factory(),
		name:      name,
		parallel:  parallel,
	})
}

func (c *Context) SetNameInterface(i NameInterface) {
	c.nameInterface = i
}

func (c *Context) SetIncrementalAnalysis(incremental bool) {
	c.incrementalAnalysis = incremental
}

func (c *Context) GetIncrementalAnalysis() bool {
	return c.incrementalAnalysis
}

func (c *Context) SetIncrementalEnabled(incremental bool) {
	c.incrementalEnabled = incremental
}

func (c *Context) SetIncrementalProviderTest(test bool) {
	c.incrementalProviderTest = test
}

func (c *Context) GetIncrementalEnabled() bool {
	return c.incrementalEnabled
}

func (c *Context) SetIncrementalDebugFile(file string) {
	c.incrementalDebugFile = file
}

func (c *Context) SetPartialAnalysisTargets(targets string) {
	rawSlice := strings.Split(strings.TrimSpace(targets), ",")
	for _, item := range rawSlice {
		cleanItem := strings.TrimSpace(item)
		if cleanItem != "" {
			c.partialAnalysisTargets = append(c.partialAnalysisTargets, cleanItem)
		}
	}
}

func (c *Context) GetPartialAnalysisTargets() []string {
	return c.partialAnalysisTargets
}

func (c *Context) GetSplitAllVariants() bool {
	return c.splitAllVariants
}

func (c *Context) SetSplitAllVariants(s bool) {
	c.splitAllVariants = s
}

func (c *Context) CacheAllBuildActions(soongOutDir string) (err error) {
	if err := cacheEncData(c, soongOutDir, OrderOnlyStringsCacheFile, &c.orderOnlyStringsCache); err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, c.keyValueStoreCache.close())
	}()
	err = c.EncContext.EncodeReferences()
	return err
}

func cacheEncData(ctx *Context, soongOutDir string, fileName string, data gobtools.CustomEnc) error {
	buf := new(bytes.Buffer)
	if err := data.Encode(ctx.EncContext, buf); err != nil {
		return err
	}
	return writeToCache(ctx, soongOutDir, fileName, buf)
}

func writeToCache(ctx *Context, soongOutDir string, fileName string, buf *bytes.Buffer) error {
	file, err := pathtools.OpenWithTruncateOnClose(ctx.fs, filepath.Join(ctx.SrcDir(), soongOutDir, fileName))
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.Write(buf.Bytes())
	return err
}

func (c *Context) RestoreAllBuildActions(soongOutDir string) error {
	return restoreEncData(c, soongOutDir, OrderOnlyStringsCacheFile, &c.orderOnlyStringsCache)
}

func restoreEncData(ctx *Context, soongOutDir string, fileName string, data gobtools.CustomDec) error {
	if stream, err := restoreFromCache(ctx, soongOutDir, fileName); err == nil && stream != nil {
		return data.Decode(ctx.EncContext, bytes.NewReader(stream))
	} else {
		return err
	}
}

func restoreFromCache(ctx *Context, soongOutDir string, fileName string) ([]byte, error) {
	file := filepath.Join(ctx.SrcDir(), soongOutDir, fileName)
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return nil, nil
	}

	return os.ReadFile(file)
}

func (c *Context) SetSrcDir(path string) {
	c.srcDir = path
	c.fs = pathtools.NewOsFs(path)
}

func (c *Context) SrcDir() string {
	return c.srcDir
}

func (c *Context) SetIncrementalDBDir(path string) {
	c.incrementalDBDir = path
}

func (c *Context) IncrementalDBDir() string {
	return c.incrementalDBDir
}

func singletonPkgPath(singleton Singleton) string {
	typ := reflect.TypeOf(singleton)
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return typ.PkgPath()
}

func singletonTypeName(singleton Singleton) string {
	typ := reflect.TypeOf(singleton)
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return typ.PkgPath() + "." + typ.Name()
}

// registerTransitionPropagateMutator registers a mutator that will be invoked to propagate transition mutator
// configuration info top-down between Modules.
func (c *Context) registerTransitionPropagateMutator(name string, mutator func(mctx BaseModuleContext)) MutatorHandle {
	for _, m := range c.mutatorInfo {
		if m.name == name && m.transitionPropagateMutator != nil {
			panic(fmt.Errorf("mutator %q is already registered", name))
		}
	}

	info := &mutatorInfo{
		transitionPropagateMutator: mutator,

		name:  name,
		index: len(c.mutatorInfo),
	}

	c.mutatorInfo = append(c.mutatorInfo, info)

	return info
}

// RegisterBottomUpMutator registers a mutator that will be invoked to split Modules into variants.
// Each registered mutator is invoked in registration order once per Module, and will not be invoked on a
// module until the invocations on all of the modules dependencies have returned.
//
// The mutator type names given here must be unique to all bottom up or early
// mutators in the Context.
func (c *Context) RegisterBottomUpMutator(name string, mutator BottomUpMutator) MutatorHandle {
	for _, m := range c.variantMutatorNames {
		if m == name {
			panic(fmt.Errorf("mutator %q is already registered", name))
		}
	}

	info := &mutatorInfo{
		bottomUpMutator: mutator,
		name:            name,
		index:           len(c.mutatorInfo),
	}
	c.mutatorInfo = append(c.mutatorInfo, info)

	c.variantMutatorNames = append(c.variantMutatorNames, name)

	return info
}

// RegisterFirstBottomUpMutator registers a mutator that will be invoked to split Modules into variants.
// The registered mutator is placed at the front of the list.
//
// The mutator type names given here must be unique to all bottom up mutators in the Context.
func (c *Context) RegisterFirstBottomUpMutator(name string, mutator BottomUpMutator) MutatorHandle {
	for _, m := range c.variantMutatorNames {
		if m == name {
			panic(fmt.Errorf("mutator %q is already registered", name))
		}
	}

	info := &mutatorInfo{
		bottomUpMutator: mutator,
		name:            name,
		index:           0,
	}
	c.mutatorInfo = append([]*mutatorInfo{info}, c.mutatorInfo...)
	c.variantMutatorNames = append([]string{name}, c.variantMutatorNames...)
	for i := range c.mutatorInfo {
		c.mutatorInfo[i].index = i
	}

	return info
}

// HasMutatorFinished returns true if the given mutator has finished running.
// It will panic if given an invalid mutator name.
func (c *Context) HasMutatorFinished(mutatorName string) bool {
	for _, mutator := range c.mutatorInfo {
		if mutator.name == mutatorName {
			return len(c.finishedMutators) > mutator.index && c.finishedMutators[mutator.index]
		}
	}
	panic(fmt.Sprintf("unknown mutator %q", mutatorName))
}

type MutatorHandle interface {
	// UsesRename marks the mutator as using the BottomUpMutatorContext.Rename method, which prevents
	// coalescing adjacent mutators into a single mutator pass.
	UsesRename() MutatorHandle

	// UsesReverseDependencies marks the mutator as using the BottomUpMutatorContext.AddReverseDependency
	// method, which prevents coalescing adjacent mutators into a single mutator pass.
	UsesReverseDependencies() MutatorHandle

	// UsesReplaceDependencies marks the mutator as using the BottomUpMutatorContext.ReplaceDependencies
	// method, which prevents coalescing adjacent mutators into a single mutator pass.
	UsesReplaceDependencies() MutatorHandle

	// UsesCreateModule marks the mutator as using the BottomUpMutatorContext.CreateModule method,
	// which prevents coalescing adjacent mutators into a single mutator pass.
	UsesCreateModule() MutatorHandle

	// MutatesDependencies marks the mutator as modifying properties in dependencies, which prevents
	// coalescing adjacent mutators into a single mutator pass.
	MutatesDependencies() MutatorHandle

	// MutatesGlobalState marks the mutator as modifying global state, which prevents coalescing
	// adjacent mutators into a single mutator pass.
	MutatesGlobalState() MutatorHandle

	PrePartial() MutatorHandle

	setTransitionMutator(impl *transitionMutatorImpl) MutatorHandle
}

func (mutator *mutatorInfo) UsesRename() MutatorHandle {
	mutator.usesRename = true
	return mutator
}

func (mutator *mutatorInfo) UsesReverseDependencies() MutatorHandle {
	mutator.usesReverseDependencies = true
	return mutator
}

func (mutator *mutatorInfo) UsesReplaceDependencies() MutatorHandle {
	mutator.usesReplaceDependencies = true
	return mutator
}

func (mutator *mutatorInfo) UsesCreateModule() MutatorHandle {
	mutator.usesCreateModule = true
	return mutator
}

func (mutator *mutatorInfo) MutatesDependencies() MutatorHandle {
	mutator.mutatesDependencies = true
	return mutator
}

func (mutator *mutatorInfo) MutatesGlobalState() MutatorHandle {
	mutator.mutatesGlobalState = true
	return mutator
}

func (mutator *mutatorInfo) PrePartial() MutatorHandle {
	mutator.prePartial = true
	return mutator
}

func (mutator *mutatorInfo) setTransitionMutator(impl *transitionMutatorImpl) MutatorHandle {
	mutator.transitionMutator = impl
	return mutator
}

// SetIgnoreUnknownModuleTypes sets the behavior of the context in the case
// where it encounters an unknown module type while parsing Blueprints files. By
// default, the context will report unknown module types as an error.  If this
// method is called with ignoreUnknownModuleTypes set to true then the context
// will silently ignore unknown module types.
//
// This method should generally not be used.  It exists to facilitate the
// bootstrapping process.
func (c *Context) SetIgnoreUnknownModuleTypes(ignoreUnknownModuleTypes bool) {
	c.ignoreUnknownModuleTypes = ignoreUnknownModuleTypes
}

// SetAllowMissingDependencies changes the behavior of Blueprint to ignore
// unresolved dependencies.  If the module's GenerateBuildActions calls
// ModuleContext.GetMissingDependencies Blueprint will not emit any errors
// for missing dependencies.
func (c *Context) SetAllowMissingDependencies(allowMissingDependencies bool) {
	c.allowMissingDependencies = allowMissingDependencies
}

func (c *Context) SetModuleListFile(listFile string) {
	c.moduleListFile = listFile
}

func (c *Context) ListModulePaths(baseDir string) (paths []string, err error) {
	reader, err := c.fs.Open(c.moduleListFile)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	bytes, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	text := string(bytes)

	text = strings.Trim(text, "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = filepath.Join(baseDir, lines[i])
	}

	return lines, nil
}

// a fileParseContext tells the status of parsing a particular file
type fileParseContext struct {
	// name of file
	fileName string

	// scope to use when resolving variables
	Scope *parser.Scope

	// pointer to the one in the parent directory
	parent *fileParseContext

	// is closed once FileHandler has completed for this file
	doneVisiting chan struct{}
}

// ParseBlueprintsFiles parses a set of Blueprints files starting with the file
// at rootFile.  When it encounters a Blueprints file with a set of subdirs
// listed it recursively parses any Blueprints files found in those
// subdirectories.
//
// If no errors are encountered while parsing the files, the list of paths on
// which the future output will depend is returned.  This list will include both
// Blueprints file paths as well as directory paths for cases where wildcard
// subdirs are found.
func (c *Context) ParseBlueprintsFiles(rootFile string,
	config interface{}) (deps []string, errs []error) {

	baseDir := filepath.Dir(rootFile)
	pathsToParse, err := c.ListModulePaths(baseDir)
	if err != nil {
		return nil, []error{err}
	}
	return c.ParseFileList(baseDir, pathsToParse, config)
}

type shouldVisitFileInfo struct {
	shouldVisitFile bool
	skippedModules  []string
	reasonForSkip   string
	errs            []error
}

// Returns a boolean for whether this file should be analyzed
// Evaluates to true if the file either
// 1. does not contain a blueprint_package_includes
// 2. contains a blueprint_package_includes and all requested tags are set
// This should be processed before adding any modules to the build graph
func shouldVisitFile(c *Context, file *parser.File) shouldVisitFileInfo {
	skippedModules := []string{}
	for _, def := range file.Defs {
		switch def := def.(type) {
		case *parser.Module:
			skippedModules = append(skippedModules, def.Name())
		}
	}

	shouldVisit, invalidatingPrefix := c.sourceRootDirs.SourceRootDirAllowed(file.Name)
	if !shouldVisit {
		return shouldVisitFileInfo{
			shouldVisitFile: shouldVisit,
			skippedModules:  skippedModules,
			reasonForSkip: fmt.Sprintf(
				"%q is a descendant of %q, and that path prefix was not included in PRODUCT_SOURCE_ROOT_DIRS",
				file.Name,
				invalidatingPrefix,
			),
		}
	}
	return shouldVisitFileInfo{shouldVisitFile: true}
}

func (c *Context) ParseFileList(rootDir string, filePaths []string,
	config interface{}) (deps []string, errs []error) {

	if len(filePaths) < 1 {
		return nil, []error{fmt.Errorf("no paths provided to parse")}
	}

	c.dependenciesReady = false

	type newModuleInfo struct {
		*moduleInfo
		deps  []string
		added chan<- struct{}
	}

	type newSkipInfo struct {
		shouldVisitFileInfo
		file string
	}

	moduleCh := make(chan newModuleInfo)
	errsCh := make(chan []error)
	doneCh := make(chan struct{})
	skipCh := make(chan newSkipInfo)
	var numErrs uint32
	var numGoroutines int32

	// handler must be reentrant
	handleOneFile := func(file *parser.File) {
		if atomic.LoadUint32(&numErrs) > maxErrors {
			return
		}

		addedCh := make(chan struct{})

		var scopedModuleFactories map[string]ModuleFactory

		var addModule func(module *moduleInfo) []error
		addModule = func(module *moduleInfo) []error {
			// Run any load hooks immediately before it is sent to the moduleCh and is
			// registered by name. This allows load hooks to set and/or modify any aspect
			// of the module (including names) using information that is not available when
			// the module factory is called.
			newModules, newDeps, errs := runAndRemoveLoadHooks(c, config, module, &scopedModuleFactories)
			if len(errs) > 0 {
				return errs
			}

			moduleCh <- newModuleInfo{module, newDeps, addedCh}
			<-addedCh
			for _, n := range newModules {
				errs = addModule(n)
				if len(errs) > 0 {
					return errs
				}
			}
			return nil
		}
		shouldVisitInfo := shouldVisitFile(c, file)
		errs := shouldVisitInfo.errs
		if len(errs) > 0 {
			atomic.AddUint32(&numErrs, uint32(len(errs)))
			errsCh <- errs
		}
		if !shouldVisitInfo.shouldVisitFile {
			skipCh <- newSkipInfo{
				file:                file.Name,
				shouldVisitFileInfo: shouldVisitInfo,
			}
			// TODO: Write a file that lists the skipped bp files
			return
		}

		for _, def := range file.Defs {
			switch def := def.(type) {
			case *parser.Module:
				module, errs := processModuleDef(def, file.Name, c.moduleFactories, scopedModuleFactories, c.ignoreUnknownModuleTypes)
				if len(errs) == 0 && module != nil {
					errs = addModule(module)
				}

				if len(errs) > 0 {
					atomic.AddUint32(&numErrs, uint32(len(errs)))
					errsCh <- errs
				}

			case *parser.Assignment:
				// Already handled via Scope object
			default:
				panic("unknown definition type")
			}

		}
	}

	atomic.AddInt32(&numGoroutines, 1)
	go func() {
		var errs []error
		deps, errs = c.WalkBlueprintsFiles(rootDir, filePaths, handleOneFile)
		if len(errs) > 0 {
			errsCh <- errs
		}
		doneCh <- struct{}{}
	}()

	var hookDeps []string
loop:
	for {
		select {
		case newErrs := <-errsCh:
			errs = append(errs, newErrs...)
		case module := <-moduleCh:
			newErrs := c.addModule(module.moduleInfo)
			hookDeps = append(hookDeps, module.deps...)
			if module.added != nil {
				module.added <- struct{}{}
			}
			if len(newErrs) > 0 {
				errs = append(errs, newErrs...)
			}
		case <-doneCh:
			n := atomic.AddInt32(&numGoroutines, -1)
			if n == 0 {
				break loop
			}
		case skipped := <-skipCh:
			nctx := newNamespaceContextFromFilename(skipped.file)
			for _, name := range skipped.skippedModules {
				c.nameInterface.NewSkippedModule(nctx, name, SkippedModuleInfo{
					filename: skipped.file,
					reason:   skipped.reasonForSkip,
				})
			}
		}
	}

	sort.Strings(hookDeps)

	deps = append(deps, hookDeps...)
	return deps, errs
}

type FileHandler func(*parser.File)

// WalkBlueprintsFiles walks a set of Blueprints files starting with the given filepaths,
// calling the given file handler on each
//
// When WalkBlueprintsFiles encounters a Blueprints file with a set of subdirs listed,
// it recursively parses any Blueprints files found in those subdirectories.
//
// If any of the file paths is an ancestor directory of any other of file path, the ancestor
// will be parsed and visited first.
//
// the file handler will be called from a goroutine, so it must be reentrant.
//
// If no errors are encountered while parsing the files, the list of paths on
// which the future output will depend is returned.  This list will include both
// Blueprints file paths as well as directory paths for cases where wildcard
// subdirs are found.
//
// visitor will be called asynchronously, and will only be called once visitor for each
// ancestor directory has completed.
//
// WalkBlueprintsFiles will not return until all calls to visitor have returned.
func (c *Context) WalkBlueprintsFiles(rootDir string, filePaths []string,
	visitor FileHandler) (deps []string, errs []error) {

	// make a mapping from ancestors to their descendants to facilitate parsing ancestors first
	descendantsMap, err := findBlueprintDescendants(filePaths)
	if err != nil {
		panic(err.Error())
	}
	blueprintsSet := make(map[string]bool)

	// Channels to receive data back from openAndParse goroutines
	blueprintsCh := make(chan fileParseContext)
	errsCh := make(chan []error)
	depsCh := make(chan string)

	// Channel to notify main loop that a openAndParse goroutine has finished
	doneParsingCh := make(chan fileParseContext)

	// Number of outstanding goroutines to wait for
	activeCount := 0
	var pending []fileParseContext
	tooManyErrors := false

	// Limit concurrent calls to parseBlueprintFiles to 200
	// Darwin has a default limit of 256 open files
	maxActiveCount := 200

	// count the number of pending calls to visitor()
	visitorWaitGroup := sync.WaitGroup{}

	startParseBlueprintsFile := func(blueprint fileParseContext) {
		if blueprintsSet[blueprint.fileName] {
			return
		}
		blueprintsSet[blueprint.fileName] = true
		activeCount++
		deps = append(deps, blueprint.fileName)
		visitorWaitGroup.Add(1)
		go func() {
			file, blueprints, deps, errs := c.openAndParse(blueprint.fileName, blueprint.Scope, rootDir,
				&blueprint)
			if len(errs) > 0 {
				errsCh <- errs
			}
			for _, blueprint := range blueprints {
				blueprintsCh <- blueprint
			}
			for _, dep := range deps {
				depsCh <- dep
			}
			doneParsingCh <- blueprint

			if blueprint.parent != nil && blueprint.parent.doneVisiting != nil {
				// wait for visitor() of parent to complete
				<-blueprint.parent.doneVisiting
			}

			if len(errs) == 0 {
				// process this file
				visitor(file)
			}
			if blueprint.doneVisiting != nil {
				close(blueprint.doneVisiting)
			}
			visitorWaitGroup.Done()
		}()
	}

	foundParseableBlueprint := func(blueprint fileParseContext) {
		if activeCount >= maxActiveCount {
			pending = append(pending, blueprint)
		} else {
			startParseBlueprintsFile(blueprint)
		}
	}

	startParseDescendants := func(blueprint fileParseContext) {
		descendants, hasDescendants := descendantsMap[blueprint.fileName]
		if hasDescendants {
			for _, descendant := range descendants {
				foundParseableBlueprint(fileParseContext{descendant, parser.NewScope(blueprint.Scope), &blueprint, make(chan struct{})})
			}
		}
	}

	// begin parsing any files that have no ancestors
	startParseDescendants(fileParseContext{"", parser.NewScope(nil), nil, nil})

loop:
	for {
		if len(errs) > maxErrors {
			tooManyErrors = true
		}

		select {
		case newErrs := <-errsCh:
			errs = append(errs, newErrs...)
		case dep := <-depsCh:
			deps = append(deps, dep)
		case blueprint := <-blueprintsCh:
			if tooManyErrors {
				continue
			}
			foundParseableBlueprint(blueprint)
		case blueprint := <-doneParsingCh:
			activeCount--
			if !tooManyErrors {
				startParseDescendants(blueprint)
			}
			if activeCount < maxActiveCount && len(pending) > 0 {
				// start to process the next one from the queue
				next := pending[len(pending)-1]
				pending = pending[:len(pending)-1]
				startParseBlueprintsFile(next)
			}
			if activeCount == 0 {
				break loop
			}
		}
	}

	sort.Strings(deps)

	// wait for every visitor() to complete
	visitorWaitGroup.Wait()

	return
}

// MockFileSystem causes the Context to replace all reads with accesses to the provided map of
// filenames to contents stored as a byte slice.
func (c *Context) MockFileSystem(files map[string][]byte) {
	// look for a module list file
	_, ok := files[MockModuleListFile]
	if !ok {
		// no module list file specified; find every file named Blueprints
		pathsToParse := []string{}
		for candidate := range files {
			if filepath.Base(candidate) == "Android.bp" {
				pathsToParse = append(pathsToParse, candidate)
			}
		}
		if len(pathsToParse) < 1 {
			panic(fmt.Sprintf("No Blueprints files found in mock filesystem: %v\n", files))
		}
		// put the list of Blueprints files into a list file
		files[MockModuleListFile] = []byte(strings.Join(pathsToParse, "\n"))
	}
	c.SetModuleListFile(MockModuleListFile)

	// mock the filesystem
	c.fs = pathtools.MockFs(files)
}

func (c *Context) SetFs(fs pathtools.FileSystem) {
	c.fs = fs
}

// openAndParse opens and parses a single Blueprints file, and returns the results
func (c *Context) openAndParse(filename string, scope *parser.Scope, rootDir string,
	parent *fileParseContext) (file *parser.File,
	subBlueprints []fileParseContext, deps []string, errs []error) {

	f, err := c.fs.Open(filename)
	if err != nil {
		// couldn't open the file; see if we can provide a clearer error than "could not open file"
		stats, statErr := c.fs.Lstat(filename)
		if statErr == nil {
			isSymlink := stats.Mode()&os.ModeSymlink != 0
			if isSymlink {
				err = fmt.Errorf("could not open symlink %v : %v", filename, err)
				target, readlinkErr := os.Readlink(filename)
				if readlinkErr == nil {
					_, targetStatsErr := c.fs.Lstat(target)
					if targetStatsErr != nil {
						err = fmt.Errorf("could not open symlink %v; its target (%v) cannot be opened", filename, target)
					}
				}
			} else {
				err = fmt.Errorf("%v exists but could not be opened: %v", filename, err)
			}
		}
		return nil, nil, nil, []error{err}
	}

	func() {
		defer func() {
			err = f.Close()
			if err != nil {
				errs = append(errs, err)
			}
		}()
		file, subBlueprints, errs = c.parseOne(rootDir, filename, f, scope, parent)
	}()

	if len(errs) > 0 {
		return nil, nil, nil, errs
	}

	for _, b := range subBlueprints {
		deps = append(deps, b.fileName)
	}

	return file, subBlueprints, deps, nil
}

// parseOne parses a single Blueprints file from the given reader, creating Module
// objects for each of the module definitions encountered.  If the Blueprints
// file contains an assignment to the "subdirs" variable, then the
// subdirectories listed are searched for Blueprints files returned in the
// subBlueprints return value.  If the Blueprints file contains an assignment
// to the "build" variable, then the file listed are returned in the
// subBlueprints return value.
//
// rootDir specifies the path to the root directory of the source tree, while
// filename specifies the path to the Blueprints file.  These paths are used for
// error reporting and for determining the module's directory.
func (c *Context) parseOne(rootDir, filename string, reader io.Reader,
	scope *parser.Scope, parent *fileParseContext) (file *parser.File, subBlueprints []fileParseContext, errs []error) {

	relBlueprintsFile, err := filepath.Rel(rootDir, filename)
	if err != nil {
		return nil, nil, []error{err}
	}

	scope.DontInherit("subdirs")
	scope.DontInherit("optional_subdirs")
	scope.DontInherit("build")
	file, errs = parser.ParseAndEval(filename, reader, scope)
	if len(errs) > 0 {
		for i, err := range errs {
			if parseErr, ok := err.(*parser.ParseError); ok {
				err = &BlueprintError{
					Err: parseErr.Err,
					Pos: parseErr.Pos,
				}
				errs[i] = err
			}
		}

		// If there were any parse errors don't bother trying to interpret the
		// result.
		return nil, nil, errs
	}
	file.Name = relBlueprintsFile

	build, buildPos, err := getLocalStringListFromScope(scope, "build")
	if err != nil {
		errs = append(errs, err)
	}
	for _, buildEntry := range build {
		if strings.Contains(buildEntry, "/") {
			errs = append(errs, &BlueprintError{
				Err: fmt.Errorf("illegal value %v. The '/' character is not permitted", buildEntry),
				Pos: buildPos,
			})
		}
	}

	if err != nil {
		errs = append(errs, err)
	}

	var blueprints []string

	newBlueprints, newErrs := c.findBuildBlueprints(filepath.Dir(filename), build, buildPos)
	blueprints = append(blueprints, newBlueprints...)
	errs = append(errs, newErrs...)

	subBlueprintsAndScope := make([]fileParseContext, len(blueprints))
	for i, b := range blueprints {
		subBlueprintsAndScope[i] = fileParseContext{b, parser.NewScope(scope), parent, make(chan struct{})}
	}
	return file, subBlueprintsAndScope, errs
}

func (c *Context) findBuildBlueprints(dir string, build []string,
	buildPos scanner.Position) ([]string, []error) {

	var blueprints []string
	var errs []error

	for _, file := range build {
		pattern := filepath.Join(dir, file)
		var matches []string
		var err error

		matches, err = c.glob(pattern, nil)

		if err != nil {
			errs = append(errs, &BlueprintError{
				Err: fmt.Errorf("%q: %s", pattern, err.Error()),
				Pos: buildPos,
			})
			continue
		}

		if len(matches) == 0 {
			errs = append(errs, &BlueprintError{
				Err: fmt.Errorf("%q: not found", pattern),
				Pos: buildPos,
			})
		}

		for _, foundBlueprints := range matches {
			if strings.HasSuffix(foundBlueprints, "/") {
				errs = append(errs, &BlueprintError{
					Err: fmt.Errorf("%q: is a directory", foundBlueprints),
					Pos: buildPos,
				})
			}
			blueprints = append(blueprints, foundBlueprints)
		}
	}

	return blueprints, errs
}

func (c *Context) findSubdirBlueprints(dir string, subdirs []string, subdirsPos scanner.Position,
	subBlueprintsName string, optional bool) ([]string, []error) {

	var blueprints []string
	var errs []error

	for _, subdir := range subdirs {
		pattern := filepath.Join(dir, subdir, subBlueprintsName)
		var matches []string
		var err error

		matches, err = c.glob(pattern, nil)

		if err != nil {
			errs = append(errs, &BlueprintError{
				Err: fmt.Errorf("%q: %s", pattern, err.Error()),
				Pos: subdirsPos,
			})
			continue
		}

		if len(matches) == 0 && !optional {
			errs = append(errs, &BlueprintError{
				Err: fmt.Errorf("%q: not found", pattern),
				Pos: subdirsPos,
			})
		}

		for _, subBlueprints := range matches {
			if strings.HasSuffix(subBlueprints, "/") {
				errs = append(errs, &BlueprintError{
					Err: fmt.Errorf("%q: is a directory", subBlueprints),
					Pos: subdirsPos,
				})
			}
			blueprints = append(blueprints, subBlueprints)
		}
	}

	return blueprints, errs
}

func getLocalStringListFromScope(scope *parser.Scope, v string) ([]string, scanner.Position, error) {
	if assignment := scope.GetLocal(v); assignment == nil {
		return nil, scanner.Position{}, nil
	} else {
		switch value := assignment.Value.(type) {
		case *parser.List:
			ret := make([]string, 0, len(value.Values))

			for _, listValue := range value.Values {
				s, ok := listValue.(*parser.String)
				if !ok {
					// The parser should not produce this.
					panic("non-string value found in list")
				}

				ret = append(ret, s.Value)
			}

			return ret, assignment.EqualsPos, nil
		case *parser.Bool, *parser.String:
			return nil, scanner.Position{}, &BlueprintError{
				Err: fmt.Errorf("%q must be a list of strings", v),
				Pos: assignment.EqualsPos,
			}
		default:
			panic(fmt.Errorf("unknown value type: %d", assignment.Value.Type()))
		}
	}
}

// Clones a build logic module by calling the factory method for its module type, and then cloning
// property values.  Any values stored in the module object that are not stored in properties
// structs will be lost.
func (c *Context) cloneLogicModule(origModule *moduleInfo) (Module, []interface{}) {
	newLogicModule, newProperties := origModule.factory()

	if len(newProperties) != len(origModule.properties) {
		panic("mismatched properties array length in " + origModule.Name())
	}

	for i := range newProperties {
		dst := reflect.ValueOf(newProperties[i])
		src := reflect.ValueOf(origModule.properties[i])

		proptools.CopyProperties(dst, src)
	}

	return newLogicModule, newProperties
}

func newVariant(module *moduleInfo, mutatorName string, variationName string) variant {

	newVariantName := module.variant.name
	if variationName != "" {
		if newVariantName == "" {
			newVariantName = variationName
		} else {
			newVariantName += "_" + variationName
		}
	}

	newVariations := module.variant.variations.clone()
	newVariations.set(mutatorName, variationName)

	return variant{newVariantName, newVariations}
}

func (c *Context) createVariations(origModule *moduleInfo, mutator *mutatorInfo,
	depChooser depChooser, variationNames []string) (moduleList, []error) {

	if mutator.transitionMutator == nil {
		panic(fmt.Errorf("method createVariations called from mutator that was not a TransitionMutator"))
	}

	if len(variationNames) == 0 {
		panic(fmt.Errorf("mutator %q passed zero-length variation list for module %q",
			mutator.name, origModule.Name()))
	}

	var newModules moduleList

	var errs []error

	for i, variationName := range variationNames {
		var newLogicModule Module
		var newProperties []interface{}

		if i == 0 {
			// Reuse the existing module for the first new variant
			// This both saves creating a new module, and causes the insertion in c.moduleInfo below
			// with logicModule as the key to replace the original entry in c.moduleInfo
			newLogicModule, newProperties = origModule.logicModule, origModule.properties
		} else {
			newLogicModule, newProperties = c.cloneLogicModule(origModule)
		}

		m := *origModule
		newModule := &m
		newLogicModule.setInfo(newModule)
		newModule.directDeps = slices.Clone(origModule.directDeps)
		newModule.reverseDeps = nil
		newModule.forwardDeps = nil
		newModule.logicModule = newLogicModule
		newModule.variant = newVariant(origModule, mutator.name, variationName)
		newModule.properties = newProperties
		newModule.providers = slices.Clone(origModule.providers)
		newModule.providerInitialValueHashes = slices.Clone(origModule.providerInitialValueHashes)
		newModule.transitionInfos = slices.Clone(origModule.transitionInfos)

		newModules = append(newModules, newModule)

		newErrs := c.convertDepsToVariation(newModule, i, depChooser)
		if len(newErrs) > 0 {
			errs = append(errs, newErrs...)
		}
	}

	// Mark original variant as invalid.  Modules that depend on this module will still
	// depend on origModule, but we'll fix it when the mutator is called on them.
	origModule.obsoletedByNewVariants = true
	origModule.splitModules = newModules

	atomic.AddUint32(&c.needsUpdateDependencies, 1)

	return newModules, errs
}

type depChooser func(source *moduleInfo, variationIndex, depIndex int, dep depInfo) (*moduleInfo, string)

func chooseDep(candidates moduleList, mutatorName, variationName string, defaultVariationName *string) (*moduleInfo, string) {
	for _, m := range candidates {
		if m.variant.variations.get(mutatorName) == variationName {
			return m, ""
		}
	}

	if defaultVariationName != nil {
		// give it a second chance; match with defaultVariationName
		for _, m := range candidates {
			if m.variant.variations.get(mutatorName) == *defaultVariationName {
				return m, ""
			}
		}
	}

	return nil, variationName
}

func chooseDepByIndexes(mutatorName string, variations [][]string) depChooser {
	return func(source *moduleInfo, variationIndex, depIndex int, dep depInfo) (*moduleInfo, string) {
		desiredVariation := variations[variationIndex][depIndex]
		return chooseDep(dep.module.splitModules, mutatorName, desiredVariation, nil)
	}
}

func (c *Context) convertDepsToVariation(module *moduleInfo, variationIndex int, depChooser depChooser) (errs []error) {
	for i, dep := range module.directDeps {
		if dep.module.obsoletedByNewVariants {
			newDep, missingVariation := depChooser(module, variationIndex, i, dep)
			if newDep == nil {
				errs = append(errs, &BlueprintError{
					Err: fmt.Errorf("failed to find variation %q for module %q needed by %q",
						missingVariation, dep.module.Name(), module.Name()),
					Pos: module.pos,
				})
				continue
			}
			module.directDeps[i].module = newDep
		}
	}

	return errs
}

func (c *Context) prettyPrintVariant(variations variationMap) string {
	var names []string
	for _, m := range c.variantMutatorNames {
		if v := variations.get(m); v != "" {
			names = append(names, m+":"+v)
		}
	}
	if len(names) == 0 {
		return "<empty variant>"
	}

	return strings.Join(names, ",")
}

func (c *Context) prettyPrintGroupVariants(group *moduleGroup) string {
	var variants []string
	for _, module := range group.modules {
		variants = append(variants, c.prettyPrintVariant(module.variant.variations))
	}
	return strings.Join(variants, "\n  ")
}

func newModule(factory ModuleFactory) *moduleInfo {
	logicModule, properties := factory()

	moduleInfo := &moduleInfo{
		logicModule:     logicModule,
		factory:         factory,
		properties:      properties,
		startedMutator:  -1,
		finishedMutator: -1,
	}
	logicModule.setInfo(moduleInfo)
	return moduleInfo
}

func processModuleDef(moduleDef *parser.Module,
	relBlueprintsFile string, moduleFactories, scopedModuleFactories map[string]ModuleFactory, ignoreUnknownModuleTypes bool) (*moduleInfo, []error) {

	factory, ok := moduleFactories[moduleDef.Type]
	if !ok && scopedModuleFactories != nil {
		factory, ok = scopedModuleFactories[moduleDef.Type]
	}
	if !ok {
		if ignoreUnknownModuleTypes {
			return nil, nil
		}

		return nil, []error{
			&BlueprintError{
				Err: fmt.Errorf("unrecognized module type %q", moduleDef.Type),
				Pos: moduleDef.TypePos,
			},
		}
	}

	module := newModule(factory)
	module.typeName = moduleDef.Type

	module.relBlueprintsFile = relBlueprintsFile

	propertyMap, errs := proptools.UnpackProperties(moduleDef.Properties, module.properties...)
	if len(errs) > 0 {
		for i, err := range errs {
			if unpackErr, ok := err.(*proptools.UnpackError); ok {
				err = &BlueprintError{
					Err: unpackErr.Err,
					Pos: unpackErr.Pos,
				}
				errs[i] = err
			}
		}
		return nil, errs
	}

	module.pos = moduleDef.TypePos
	module.propertyPos = make(map[string]scanner.Position)
	for name, propertyDef := range propertyMap {
		module.propertyPos[name] = propertyDef.ColonPos
	}

	return module, nil
}

func (c *Context) addModule(module *moduleInfo) []error {
	name := module.logicModule.Name()
	if name == "" {
		return []error{
			&BlueprintError{
				Err: fmt.Errorf("property 'name' is missing from a module"),
				Pos: module.pos,
			},
		}
	}

	group := &moduleGroup{
		name:    name,
		modules: moduleList{module},
	}
	module.group = group
	namespace, errs := c.nameInterface.NewModule(
		newNamespaceContext(module),
		ModuleGroup{moduleGroup: group},
		module.logicModule)
	if len(errs) > 0 {
		for i := range errs {
			errs[i] = &BlueprintError{Err: errs[i], Pos: module.pos}
		}
		return errs
	}
	group.namespace = namespace

	c.moduleGroups = append(c.moduleGroups, group)

	return nil
}

// ResolveDependencies checks that the dependencies specified by all of the
// modules defined in the parsed Blueprints files are valid.  This means that
// the modules depended upon are defined and that no circular dependencies
// exist.
func (c *Context) ResolveDependencies(config interface{}) (deps []string, errs []error) {
	c.BeginEvent("resolve_deps")
	defer c.EndEvent("resolve_deps")
	return c.resolveDependencies(c.Context, config)
}

// coalesceMutators takes the list of mutators and returns a list of lists of mutators,
// where sublist is a compatible group of mutators that can be run with relaxed
// intra-mutator ordering.
func coalesceMutators(mutators []*mutatorInfo) [][]*mutatorInfo {
	var coalescedMutators [][]*mutatorInfo
	var last *mutatorInfo

	// Returns true if the mutator can be coalesced with other mutators that
	// also return true.
	coalescable := func(m *mutatorInfo) bool {
		return m.bottomUpMutator != nil &&
			m.transitionMutator == nil &&
			!m.usesCreateModule &&
			!m.usesReplaceDependencies &&
			!m.usesReverseDependencies &&
			!m.usesRename &&
			!m.mutatesGlobalState &&
			!m.mutatesDependencies &&
			// No mix of pre partial marker mutator with others.
			!m.prePartial
	}

	for _, mutator := range mutators {
		if last != nil && coalescable(last) && coalescable(mutator) {
			lastGroup := &coalescedMutators[len(coalescedMutators)-1]
			*lastGroup = append(*lastGroup, mutator)
		} else {
			coalescedMutators = append(coalescedMutators, []*mutatorInfo{mutator})
			last = mutator
		}
	}

	return coalescedMutators
}

func (c *Context) resolveDependencies(ctx context.Context, config interface{}) (deps []string, errs []error) {
	pprof.Do(ctx, pprof.Labels("blueprint", "ResolveDependencies"), func(ctx context.Context) {
		c.initProviders()

		errs = c.updateDependencies()
		if len(errs) > 0 {
			return
		}

		mutatorGroups := coalesceMutators(c.mutatorInfo)

		deps, errs = c.runMutators(ctx, config, mutatorGroups)
		if len(errs) > 0 {
			return
		}

		c.dependenciesReady = true
	})

	if len(errs) > 0 {
		return nil, errs
	}

	return deps, nil
}

// Default dependencies handling.  If the module implements the (deprecated)
// DynamicDependerModule interface then this set consists of the union of those
// module names returned by its DynamicDependencies method and those added by calling
// AddDependencies or AddVariationDependencies on DynamicDependencyModuleContext.
func blueprintDepsMutator(ctx BottomUpMutatorContext) {
	if dynamicDepender, ok := ctx.Module().(DynamicDependerModule); ok {
		func() {
			defer func() {
				if r := recover(); r != nil {
					ctx.error(newPanicErrorf(r, "DynamicDependencies for %s", ctx.moduleInfo()))
				}
			}()
			dynamicDeps := dynamicDepender.DynamicDependencies(ctx)

			if ctx.Failed() {
				return
			}

			ctx.AddDependency(ctx.Module(), nil, dynamicDeps...)
		}()
	}
}

// applyTransitions takes a variationMap being used to add a dependency on a module in a moduleGroup
// and applies the OutgoingTransition and IncomingTransition methods of each completed TransitionMutator to
// modify the requested variation.  It finds a variant that existed before the TransitionMutator ran that is
// a subset of the requested variant to use as the module context for IncomingTransition.
//
// for onDemand: true, a copy of the dependency will be created on demand and used as the context
// for IncomingTransition.
func (c *Context) applyTransitions(config any, module *moduleInfo, depTag DependencyTag, group *moduleGroup, variant variationMap,
	requestedVariations []Variation, far bool, onDemandMatchingVariant *moduleInfo) (variationMap, []TransitionInfo, []error) {
	// Initialize some variables that will be used to manage IncomingTransition context of on demand variants.
	onDemandFromMutatorIndex := c.mutatorIndexAfterLastCreateModule

	var moduleOutgoingTransitionInfos []TransitionInfo

	for _, transitionMutator := range c.transitionMutators[:c.completedTransitionMutators] {
		explicitlyRequested := slices.ContainsFunc(requestedVariations, func(variation Variation) bool {
			return variation.Mutator == transitionMutator.name
		})

		var outgoingTransitionInfo TransitionInfo
		if explicitlyRequested {
			sourceVariation := variant.get(transitionMutator.name)
			outgoingTransitionInfo = transitionMutator.mutator.TransitionInfoFromVariation(sourceVariation)
		} else {
			// Apply the outgoing transition if it was not explicitly requested.
			var srcTransitionInfo TransitionInfo
			if (!far || transitionMutator.neverFar) && len(module.transitionInfos) > transitionMutator.index {
				srcTransitionInfo = module.transitionInfos[transitionMutator.index]
			}
			ctx := outgoingTransitionContextPool.Get()
			*ctx = outgoingTransitionContextImpl{
				transitionContextImpl{context: c, source: module, dep: nil,
					depTag: depTag, postMutator: true, config: config},
			}
			outgoingTransitionInfo = transitionMutator.mutator.OutgoingTransition(ctx, srcTransitionInfo)
			errs := ctx.errs
			outgoingTransitionContextPool.Put(ctx)
			ctx = nil
			if len(errs) > 0 {
				return variationMap{}, moduleOutgoingTransitionInfos, errs
			}
			moduleOutgoingTransitionInfos = append(moduleOutgoingTransitionInfos, outgoingTransitionInfo)
		}

		earlierVariantCreatingMutators := c.transitionMutatorNames[:transitionMutator.index]
		filteredVariant := variant.cloneMatching(earlierVariantCreatingMutators)

		// Find an appropriate module to use as the context for the IncomingTransition.
		var matchingInputVariant *moduleInfo
		for _, module := range group.modules {
			filteredInputVariant := module.variant.variations.cloneMatching(earlierVariantCreatingMutators)
			if filteredInputVariant.equal(filteredVariant) {
				matchingInputVariant = module
				break
			}
		}

		if onDemandMatchingVariant != nil {
			// Mutate the onDemand variant through a moving window,
			// from the previous transition mutator to the current transition mutator.
			// This variant can then be used as the context for the IncomingTransition.
			matchingInputVariant = onDemandMatchingVariant
			if outgoingTransitionInfo != nil {
				onDemandMatchingVariant.requestedOnDemandVariant.variations[transitionMutator.name] = outgoingTransitionInfo.Variation()
			}
			// Rerun till propagate mutator handle of the Transition mutator.
			// transitionMutator.mutatorIndex corresponds to mutate.
			// transitionMutator.mutatorIndex-1 corresponds to bottomUp handle of the transition mutator
			// transitionMutator.mutatorIndex-2 (inclusive) corresponds to propagate mutator handle of the transition mutator
			c.rerunMutatorsOnVariantOnDemand(onDemandMatchingVariant, onDemandFromMutatorIndex, transitionMutator.mutatorIndex-2, nil, config)
			onDemandFromMutatorIndex = transitionMutator.mutatorIndex - 1
		}

		if matchingInputVariant != nil {
			// Apply the incoming transition.
			ctx := incomingTransitionContextPool.Get()
			*ctx = incomingTransitionContextImpl{
				transitionContextImpl{context: c, source: nil, dep: matchingInputVariant,
					depTag: depTag, postMutator: true, config: config},
			}

			finalTransitionInfo := transitionMutator.mutator.IncomingTransition(ctx, outgoingTransitionInfo)
			errs := ctx.errs
			incomingTransitionContextPool.Put(ctx)
			ctx = nil
			if len(errs) > 0 {
				return variationMap{}, moduleOutgoingTransitionInfos, errs
			}
			variation := ""
			if finalTransitionInfo != nil {
				variation = finalTransitionInfo.Variation()
			}
			variant.set(transitionMutator.name, variation)
			// For on-demand variants, check if requested variation can be supported by `SplitOnDemand`.
			if onDemandMatchingVariant != nil && variation == "" {
				onDemandMatchingVariant.requestedOnDemandVariant.delete(transitionMutator.name)
			} else if onDemandMatchingVariant != nil {
				// Rerun till Mutate of the Transition mutator
				onDemandMatchingVariant.requestedOnDemandVariant.variations[transitionMutator.name] = variation
				c.rerunMutatorsOnVariantOnDemand(onDemandMatchingVariant, onDemandFromMutatorIndex, transitionMutator.mutatorIndex, nil, config)
				onDemandFromMutatorIndex = transitionMutator.mutatorIndex + 1
				found := false
				for _, split := range onDemandMatchingVariant.createdOnDemandSupportedSplits {
					if split.Variation() == variation {
						found = true
					}
				}
				if !found {
					onDemandMatchingVariant.createdOnDemandIncompatible = true
					return variant, moduleOutgoingTransitionInfos, nil
				}
				onDemandMatchingVariant.createdOnDemandSupportedSplits = nil // reset for next transition mutator check.
			}
		}

		if (matchingInputVariant == nil && !explicitlyRequested) || variant.get(transitionMutator.name) == "" {
			// The transition mutator didn't apply anything to the target variant, remove the variation unless it
			// was explicitly requested when adding the dependency.
			variant.delete(transitionMutator.name)
		}
	}

	return variant, moduleOutgoingTransitionInfos, nil
}

func (c *Context) findVariant(config any, module *moduleInfo, depTag DependencyTag,
	possibleDeps *moduleGroup, requestedVariations []Variation, far bool, reverse bool, mutatorIndex int) (*moduleInfo, variationMap, []error) {

	// We can't just append variant.Variant to module.dependencyVariant.variantName and
	// compare the strings because the result won't be in mutator registration order.
	// Create a new map instead, and then deep compare the maps.
	var newVariant variationMap
	if !far {
		newVariant = module.variant.variations.clone()
	} else {
		for _, transitionMutator := range c.transitionMutators {
			if transitionMutator.neverFar {
				newVariant.set(transitionMutator.name, module.variant.variations.get(transitionMutator.name))
			}
		}
	}
	for _, v := range requestedVariations {
		newVariant.set(v.Mutator, v.Variation)
	}

	newVariantBeforeTransitions := newVariant.clone()
	var moduleOutgoingTransitionInfos []TransitionInfo

	if !reverse {
		var errs []error
		newVariant, moduleOutgoingTransitionInfos, errs = c.applyTransitions(config, module, depTag, possibleDeps, newVariant, requestedVariations, far, nil)
		if len(errs) > 0 {
			return nil, variationMap{}, errs
		}
	}

	// check returns a bool for whether the requested newVariant matches the given variant from possibleDeps, and a
	// divergence score.  A score of 0 is best match, and a positive integer is a worse match.
	// For a non-far search, the score is always 0 as the match must always be exact.  For a far search,
	// the score is the number of variants that are present in the given variant but not newVariant.
	check := func(variant variationMap) (bool, int) {
		if far {
			if newVariant.subsetOf(variant) {
				return true, variant.differenceKeysCount(newVariant)
			}
		} else {
			if variant.equal(newVariant) {
				return true, 0
			}
		}
		return false, math.MaxInt
	}

	var foundDep *moduleInfo
	bestDivergence := math.MaxInt
	for _, m := range possibleDeps.modules {
		if match, divergence := check(m.variant.variations); match && divergence < bestDivergence {
			foundDep = m
			bestDivergence = divergence
			if !far {
				// non-far dependencies use equality, so only the first match needs to be checked.
				break
			}
		}
	}

	if foundDep == nil &&
		!c.allowMissingDependencies && // TODO (b/448182009): Skip checking for possible on-demand variant if allowMissingDependencies is set.
		possibleDeps.searchOnDemandVariant(newVariantBeforeTransitions, far, mutatorIndex, c.transitionMutators) {
		if variantOnDemand, errs := c.loadOrCreateVariantOnDemand(config, module, depTag, possibleDeps, newVariantBeforeTransitions, requestedVariations, far, moduleOutgoingTransitionInfos); variantOnDemand != nil {
			foundDep = c.createVariantOnDemand(possibleDeps, variantOnDemand.requestedOnDemandVariant.clone())
			foundDep.finishedMutator = mutatorIndex
			newVariant = variantOnDemand.requestedOnDemandVariant.clone()
		} else {
			return nil, newVariant, errs
		}
	}

	return foundDep, newVariant, nil
}

func (c *Context) addVariationDependency(module *moduleInfo, mutator *mutatorInfo, config any, variations []Variation,
	tag DependencyTag, depName string, far bool) (*moduleInfo, []error) {
	if _, ok := tag.(BaseDependencyTag); ok {
		panic("BaseDependencyTag is not allowed to be used directly!")
	}

	possibleDeps := c.moduleGroupFromName(depName, module.namespace())
	if possibleDeps == nil {
		return nil, c.discoveredMissingDependencies(module, depName, variationMap{})
	}

	foundDep, newVariant, errs := c.findVariant(config, module, tag, possibleDeps, variations, far, false, mutator.index)
	if errs != nil {
		return nil, errs
	}

	if foundDep == nil {
		if c.allowMissingDependencies {
			// Allow missing variants.
			return nil, c.discoveredMissingDependencies(module, depName, newVariant)
		}
		return nil, []error{&BlueprintError{
			Err: fmt.Errorf("dependency %q of %q missing variant:\n  %s\navailable variants:\n  %s",
				depName, module.Name(),
				c.prettyPrintVariant(newVariant),
				c.prettyPrintGroupVariants(possibleDeps)),
			Pos: module.pos,
		}}
	}

	if module == foundDep {
		return nil, []error{&BlueprintError{
			Err: fmt.Errorf("%q depends on itself", depName),
			Pos: module.pos,
		}}
	}
	if foundDep.createdOnDemand {
		// `runMutator` will create the dependency edges, deduping on demand variants if necessary.
		// Also, defer the beforeInModuleList check to the end of runMutator.
		return foundDep, nil
	}
	// AddVariationDependency allows adding a dependency on itself, but only if
	// that module is earlier in the module list than this one, since we always
	// run GenerateBuildActions in order for the variants of a module
	if foundDep.group == module.group && beforeInModuleList(module, foundDep, module.group.modules) {
		return nil, []error{&BlueprintError{
			Err: fmt.Errorf("%q depends on later version of itself", depName),
			Pos: module.pos,
		}}
	}

	// The mutator will pause until the newly added dependency has finished running the current mutator,
	// so it is safe to add the new dependency directly to directDeps and forwardDeps where it will be visible
	// to future calls to VisitDirectDeps.  Set newDirectDeps so that at the end of the mutator the reverseDeps
	// of the dependencies can be updated to point to this module without running a full c.updateDependencies()
	module.directDeps = append(module.directDeps, depInfo{foundDep, tag})
	module.forwardDeps = append(module.forwardDeps, foundDep)
	module.newDirectDeps = append(module.newDirectDeps, foundDep)
	return foundDep, nil
}

// findBlueprintDescendants returns a map linking parent Blueprint files to child Blueprints files
// For example, if paths = []string{"a/b/c/Android.bp", "a/Android.bp"},
// then descendants = {"":[]string{"a/Android.bp"}, "a/Android.bp":[]string{"a/b/c/Android.bp"}}
func findBlueprintDescendants(paths []string) (descendants map[string][]string, err error) {
	// make mapping from dir path to file path
	filesByDir := make(map[string]string, len(paths))
	for _, path := range paths {
		dir := filepath.Dir(path)
		_, alreadyFound := filesByDir[dir]
		if alreadyFound {
			return nil, fmt.Errorf("Found two Blueprint files in directory %v : %v and %v", dir, filesByDir[dir], path)
		}
		filesByDir[dir] = path
	}

	findAncestor := func(childFile string) (ancestor string) {
		prevAncestorDir := filepath.Dir(childFile)
		for {
			ancestorDir := filepath.Dir(prevAncestorDir)
			if ancestorDir == prevAncestorDir {
				// reached the root dir without any matches; assign this as a descendant of ""
				return ""
			}

			ancestorFile, ancestorExists := filesByDir[ancestorDir]
			if ancestorExists {
				return ancestorFile
			}
			prevAncestorDir = ancestorDir
		}
	}
	// generate the descendants map
	descendants = make(map[string][]string, len(filesByDir))
	for _, childFile := range filesByDir {
		ancestorFile := findAncestor(childFile)
		descendants[ancestorFile] = append(descendants[ancestorFile], childFile)
	}
	return descendants, nil
}

type visitOrderer interface {
	// returns the number of modules that this module needs to wait for
	waitCount(module *moduleInfo) int
	// returns the list of modules that are waiting for this module
	propagate(module *moduleInfo) []*moduleInfo
}

type unorderedVisitorImpl struct{}

func (unorderedVisitorImpl) waitCount(module *moduleInfo) int {
	return 0
}

func (unorderedVisitorImpl) propagate(module *moduleInfo) []*moduleInfo {
	return nil
}

type bottomUpVisitorImpl struct{}

func (bottomUpVisitorImpl) waitCount(module *moduleInfo) int {
	return len(module.forwardDeps)
}

func (bottomUpVisitorImpl) propagate(module *moduleInfo) []*moduleInfo {
	return module.reverseDeps
}

type topDownVisitorImpl struct{}

func (topDownVisitorImpl) waitCount(module *moduleInfo) int {
	return len(module.reverseDeps)
}

func (topDownVisitorImpl) propagate(module *moduleInfo) []*moduleInfo {
	return module.forwardDeps
}

func (topDownVisitorImpl) visit(modules []*moduleInfo, visit func(*moduleInfo, chan<- pauseSpec) bool) {
	for i := 0; i < len(modules); i++ {
		module := modules[len(modules)-1-i]
		if visit(module, nil) {
			return
		}
	}
}

var (
	bottomUpVisitor bottomUpVisitorImpl
	topDownVisitor  topDownVisitorImpl
)

// unpause is a channel that will be closed when the paused module should resume.
type unpause chan struct{}

// pauseFunc is a function a visitor function can call to pause execution until the visitor
// function on the given module is completed.
type pauseFunc func(until *moduleInfo)

// parallelVisitWorker is an individual worker that will call visitor functions on behalf of
// a call to parallelVisit.  It's run method loops until the input queueCh is closed, receiving
// batches of modules on which to call visit method, and posting responses to the responseCh
// channel.
type parallelVisitWorker struct {
	// queue is the slice of modules that are currently being processed.
	queue []*moduleInfo
	// currentQueueIndex is the index into the queue of the module currently being visited.
	currentQueueIndex int

	// done is the slice of modules that have had the visit method called on them has returned.
	done []*moduleInfo
	// remaining is the slice of modules that have not had the visit method called on them.
	remaining []*moduleInfo
	// current is the module that is currently running the visit method.
	current *moduleInfo
	// unblocked is the slice of modules that are now runnable after visit returned on a module
	// in this batch.
	// TODO: can these modules just be handled in this worker?  Use some heuristic to decide
	//  whether to handle them or send them back to the orchestrator?
	unblocked []*moduleInfo

	// visit is the method that is called on each module.
	visit func(module *moduleInfo, pause pauseFunc) bool

	// order is the interface that describes which modules will be ready next once the current module
	// has had visit called on it.
	order visitOrderer

	// queueCh is the input channel that provides batches of modules to call visit on.  It is closed
	// when there is no more work to do.
	queueCh <-chan []*moduleInfo
	// responseCh is the output channel where a parallelVisitWorkerResponse is sent after processing
	// each batch.
	responseCh chan<- parallelVisitWorkerResponse
}

// pauseSpec describes a pause that a module needs to occur until another module has been visited,
// at which point the unpause channel will be closed.
type pauseSpec struct {
	paused  *moduleInfo
	until   *moduleInfo
	unpause unpause
}

// parallelVisitWorkerResponse is sent after a parallelVisitWorker processes a batch of modules.
type parallelVisitWorkerResponse struct {
	// done is the slice of modules that have had the visit method called on.
	done []*moduleInfo

	// returned is the slice of modules that the worker did not call the visit method on,
	// either because the visit method returned an error, or because the visit method called
	// the pause function, which may be waiting (directly or transitively) on a module that
	// is later in the batch.
	returned []*moduleInfo

	// unblocked is the slice of modules that became ready (i.e. waitingCount is now zero)
	// after the modules that the worker ran finished.
	unblocked []*moduleInfo

	// error is set when a visit method returned an error, signalling that parallelVisit should
	// abort.
	error bool

	// pause is set when the visit method called pause, signalling that the worker is waiting
	// indefinitely on the unpause channel, which should be closed when the requested module
	// has completed.
	pause pauseSpec
}

// run is the main loop method of a parallelVisitWorker.  It receives batches of modules to
// call the visit method on from queueCh, and then calls runQueue on them.
func (worker *parallelVisitWorker) run() {
	for queued := range worker.queueCh {
		if len(worker.queue) > 0 {
			panic(fmt.Errorf("already have queued work"))
		}
		errored := worker.runQueue(queued)
		if errored {
			return
		}
	}
}

// runQueue processes a single batch of modules.
func (worker *parallelVisitWorker) runQueue(queue []*moduleInfo) bool {
	worker.queue = queue
	worker.done = nil
	worker.unblocked = nil
	// Use a loop on worker.currentQueueIndex so that the pause method can update it when it sends
	// the done and remaining modules back to the orchestrator.
	for worker.currentQueueIndex = 0; worker.currentQueueIndex < len(worker.queue); worker.currentQueueIndex++ {
		worker.current = worker.queue[worker.currentQueueIndex]
		worker.remaining = worker.queue[worker.currentQueueIndex+1:]
		ret := worker.visit(worker.current, worker.pause)
		worker.done = worker.queue[:worker.currentQueueIndex+1]
		if ret {
			// An error occurred.  Send the completed and uncompleted modules back to the orchestrator with
			// the error flag set.
			worker.responseCh <- parallelVisitWorkerResponse{
				done:      worker.done,
				error:     true,
				returned:  worker.remaining,
				unblocked: worker.unblocked,
			}
			return true
		}

		// Decrement waitingCount on the next modules in the tree based
		// on propagation order, and add them to the queue if they are
		// ready to start.
		for _, module := range worker.order.propagate(worker.current) {
			if module.waitingCount.Add(-1) == 0 {
				worker.unblocked = append(worker.unblocked, module)
			}
		}
	}

	// The batch is complete.  Send the completed modules back to the orchestrator.
	worker.responseCh <- parallelVisitWorkerResponse{
		done:      worker.done,
		unblocked: worker.unblocked,
	}
	worker.queue = nil
	return false
}

// pause is called by visitors (via the function pointer passed into the visit function) in order to
// signal that the visitor needs to wait until the visitor has completed on the target module.
func (worker *parallelVisitWorker) pause(until *moduleInfo) {
	// If the target module is already done there is no need to pause.  This is safe because waitingCount
	// will never change once it has reached -1.
	if until.waitingCount.Load() == -1 {
		return
	}
	unpause := make(chan struct{})
	// This visitor needs to pause.  Send the completed and uncompleted modules back to the orchestrator
	// with a pauseSpec that describes how and when to unpause this module.  The uncompleted modules are
	// returned to the orchestrator in case the pause is waiting (directly or transitively) for one of
	// the uncompleted modules.
	worker.responseCh <- parallelVisitWorkerResponse{
		done:      worker.done,
		returned:  worker.remaining,
		unblocked: worker.unblocked,
		pause: pauseSpec{
			paused:  worker.current,
			until:   until,
			unpause: unpause,
		},
	}
	// Reset the queue to contain only the current module, as everything else has already been returned
	// to the orchestrator.
	worker.done = nil
	worker.queue = []*moduleInfo{worker.current}
	worker.currentQueueIndex = 0
	worker.remaining = nil
	worker.unblocked = nil

	// Wait for the orchestrator to close the unpause channel to signal that the requested module has
	// finished.
	<-unpause
}

// parallelVisitLimit is the maximum number of visitors that can be simultaneously active in parallelVisit.
var parallelVisitLimit = runtime.NumCPU() * 2

// parallelVisitBatchSize is the number of visitors that will be batched together and passed to a single
// parallelVisitWorker in order to reduce channel communication overhead.  Setting this value higher
// amortizes the synchronization and coordination costs across more modules, but setting it too high
// risks having a single worker left processing modules after all the other workers have finished if it
// has too many long visitors in a single batch.
const parallelVisitBatchSize = 100

// parallelVisit calls visit on each module, guaranteeing that visit is not called on a module until
// visit on all of its dependencies (as determined by the visitOrderer) has finished.  A visit function
// can call the pause function to wait for another dependency to be visited before continuing.
// If a visit function returns true to cancel while another visitor is paused, the paused visitor will
// never be resumed and its goroutine will stay paused forever.
// The limit argument sets the maximum number of workers that can be running visit functions simultaneously.
// The total number of workers can be higher than limit if some of them are paused, but the paused workers
// won't be unpaused until the number of active workers drops below the limit.
func parallelVisit(moduleIter iter.Seq[*moduleInfo], order visitOrderer, limit int,
	visit func(module *moduleInfo, pause pauseFunc) bool) []error {

	// queueCh sends batches of modules to process from the orchestrator to the workers.
	queueCh := make(chan []*moduleInfo, limit)
	// responseCh receives responses from the workers when a batch has finished processing.
	responseCh := make(chan parallelVisitWorkerResponse, limit)

	// Closing queueCh when parallelVisit finishes signals the workers to exit.
	defer close(queueCh)

	activeWorkers := 0 // Number of workers running, not counting paused visitors.
	activeModules := 0 // Number of modules that have been sent to workers.
	visited := 0       // Number of modules whose visitors have finished.
	pausedWorkers := 0 // Number of workers that are waiting on an unpause channel
	workers := 0       // Total number of spawned workers.

	hung := make(map[*moduleInfo]bool)

	cancel := false // will be set when any worker returns an error.

	var queue []*moduleInfo           // The list of modules that are ready to be sent to workers.
	var returnedQueue [][]*moduleInfo // The list of modules that were sent to workers and then returned and need to be resent.
	var unpauseQueue []pauseSpec      // Visitors that are ready to unpause but backlogged due to limit.

	queuedModules := 0 // The number of modules that are queued in queue, returnedQueue or unpauseQueue.

	// pauseMap holds the map from modules that are being waited on to the list of pauseSpecs that are waiting on them.
	pauseMap := make(map[*moduleInfo][]pauseSpec)

	// newWorker spawns a new worker goroutine.
	newWorker := func() {
		worker := &parallelVisitWorker{
			visit:      visit,
			order:      order,
			queueCh:    queueCh,
			responseCh: responseCh,
		}
		workers++
		go worker.run()
	}

	toVisit := 0

	// Initialize waitingCount on each module with the number of modules that need to complete before it can run.
	// Add any modules whose waitingCount is 0 to the initial queue of ready modules.
	for module := range moduleIter {
		hung[module] = true
		toVisit++
		waitingCount := order.waitCount(module)
		module.waitingCount.Store(int32(waitingCount))
		if waitingCount == 0 {
			queue = append(queue, module)
			queuedModules++
		}
	}
	queue = slices.Grow(queue, toVisit-len(queue))

	// queueWork is called to send work to any available workers, including spawning new workers if there is work
	// to do and the number of active workers is below the limit.
	queueWork := func() {
		for queuedModules > 0 && activeWorkers < limit {
			// First priority: unpause any paused workers that are ready, as the visitor functions may already
			// be holding resources.
			if len(unpauseQueue) > 0 {
				unpause := unpauseQueue[0]
				unpauseQueue = unpauseQueue[1:]
				pausedWorkers--
				queuedModules--
				activeModules++
				close(unpause.unpause)
			} else {
				// If there are worker slots available and no idle workers, spawn a new worker.
				if activeWorkers < limit && activeWorkers+pausedWorkers == workers {
					newWorker()
				}
				var batch []*moduleInfo
				// Second priority: re-send any returned work back to a worker.
				if len(returnedQueue) > 0 {
					batch = returnedQueue[0]
					returnedQueue = returnedQueue[1:]
				} else {
					// Send a batch of work from the queue.  Limit the size of the batch to the size of the queue
					// divided by the number of available workers to avoid sending a big batch of work to a single
					// worker when other workers are available and to parallelVisitBatchSize.
					availableWorkersSlots := limit - activeWorkers
					queueSizePerAvailableWorker := (len(queue) + availableWorkersSlots - 1) / availableWorkersSlots
					batchSize := min(parallelVisitBatchSize, queueSizePerAvailableWorker)
					batch = queue[:batchSize]
					queue = queue[batchSize:]
				}
				activeModules += len(batch)
				queuedModules -= len(batch)
				if len(batch) == 0 {
					panic("zero length batch")
				}
				queueCh <- batch
			}
			activeWorkers++
		}
	}

	// Call queueWork before starting the loop so that activeModules is nonzero.
	queueWork()

	// The main orchestrator loop, which runs until there are no workers doing work.
	for activeModules > 0 {
		// Wait for a response from a worker.
		response := <-responseCh
		activeWorkers--

		if response.error {
			// Once cancel is set no more work will be sent to workers.
			cancel = true
		}

		// Process finished modules.
		visited += len(response.done)
		activeModules -= len(response.done)
		for _, doneModule := range response.done {
			delete(hung, doneModule)
			// Mark this module as done.  Nothing else should be updating waitingCount, so a single attempt
			// at CompareAndSwap should always succeed.  This is the only location that will ever update
			// waitingCount from 0 to -1, and once it is -1 it will never be changed for the rest of this
			// call to parallelVisit.
			if !doneModule.waitingCount.CompareAndSwap(0, -1) {
				panic(fmt.Errorf("failed to atomically mark module %s as done", doneModule))
			}
			// Add any modules that were paused on this module to the unpause queue.
			if unpauses, ok := pauseMap[doneModule]; ok {
				delete(pauseMap, doneModule)
				queuedModules += len(unpauses)
				unpauseQueue = append(unpauseQueue, unpauses...)
			}
		}

		if len(response.unblocked) > 0 {
			// Add any modules that were made ready to the queue.
			queuedModules += len(response.unblocked)
			queue = append(queue, response.unblocked...)
		}

		// Re-queue any returned modules.
		if len(response.returned) > 0 {
			queuedModules += len(response.returned)
			activeModules -= len(response.returned)
			returnedQueue = append(returnedQueue, response.returned)
		}

		// Handle a requested pause.
		if response.pause.paused != nil {
			// This goroutine is the only one that can set waitingCount to -1, so reading it here does not
			// race with updating pauseMap if the value is not yet -1.
			if response.pause.until.waitingCount.Load() == -1 {
				// Module being paused for is already finished, resume immediately.
				// activeWorkers was decremented above when the response was received,
				// re-increment it as it is going to resume.
				activeWorkers++
				close(response.pause.unpause)
			} else {
				// Register for unpausing.
				pauseMap[response.pause.until] = append(pauseMap[response.pause.until], response.pause)
				pausedWorkers++
				activeModules--
			}

			// If a dependency is created on demand, add it to the queue for processing.
			// The main coordinator goroutine will be responsible for deduping on demand variant
			// requested from multiple rdeps.
			if response.pause.until.createdOnDemand {
				queue = append(queue, response.pause.until)
				toVisit++
				hung[response.pause.until] = true
				queuedModules++
			}

		}

		// Each time a response has been handled check if there is work that can now be queued.
		if !cancel {
			queueWork()
		}
	}

	// The orchestrator loop has finished because there are no modules being processed.  In the normal case all
	// the modules should have been visited.  If an error occurred there may be queued or paused modules.
	// If a deadlock occurred and all remaining modules are not ready or paused then there is newly added
	// cyclic dependency.
	if !cancel {
		// Invariant checks: no queued, returned or unpaused modules.  These weren't waiting on anything except
		// the parallelism limit so they should have run.
		if len(queue) > 0 {
			panic(fmt.Errorf("parallelVisit finished with %d queued visitors", len(queue)))
		}
		if len(returnedQueue) > 0 {
			panic(fmt.Errorf("parallelVisit finished with %d returned queued visitors", len(returnedQueue)))
		}
		if len(unpauseQueue) > 0 {
			panic(fmt.Errorf("parallelVisit finished with %d queued unpaused visitors", len(unpauseQueue)))
		}

		if visited != toVisit || len(pauseMap) > 0 {
			// Probably a deadlock due to a dependency cycle. Start from each module in the order
			// of the input modules list and perform a depth-first search for any module that is
			// in the walk path twice.  Note this traverses from modules to the modules that would
			// have been unblocked when that module finished, i.e. the reverse of the visitOrderer.
			// This search takes into account both the pre-existing dependencies and any newly
			// added dependencies that are still in the pauseMap.

			// In order to reduce duplicated work, once a module has been checked and determined
			// not to be part of a cycle add it and everything that depends on it to the checked
			// map.
			checked := make(map[*moduleInfo]bool, toVisit) // modules that were already checked
			checking := make(map[*moduleInfo]bool)         // modules actively being checked

			var errs []error
			var check func(group *moduleInfo) []*moduleInfo

			check = func(module *moduleInfo) []*moduleInfo {
				if checking[module] {
					// This is a cycle.
					return []*moduleInfo{module}
				}
				if checked[module] {
					return nil
				}

				checked[module] = true
				checking[module] = true
				defer delete(checking, module)

				// Create an iterator that yields the existing dependencies of this module (via order.propagate),
				// followed by any newly added dependencies stored in pauseMap.
				dependencyIter := func(yield func(*moduleInfo) bool) {
					for _, dep := range order.propagate(module) {
						if !yield(dep) {
							return
						}
					}
					for _, pauseSpec := range pauseMap[module] {
						if !yield(pauseSpec.paused) {
							return
						}
					}
				}

				var cycle []*moduleInfo
				for dep := range dependencyIter {
					cycle = check(dep)
					if cycle != nil {
						break
					}
				}

				if cycle != nil {
					if cycle[0] == module {
						// We are the "start" of the cycle, so we're responsible
						// for generating the errors.
						slices.Reverse(cycle)
						errs = append(errs, cycleError(cycle)...)

						// We can continue processing this module's children to
						// find more cycles.  Since all the modules that were
						// part of the found cycle were marked as visited we
						// won't run into that cycle again.
					} else {
						// We're not the "start" of the cycle, so we just append
						// our module to the list and return it.
						return append(cycle, module)
					}
				}

				return nil
			}

			for module := range moduleIter {
				check(module)
			}

			if len(errs) > 0 {
				return errs
			}
		}

		// Invariant check: if there was no dependency cycle and no cancellation every module
		// should have been visited, so there is nothing left to be paused on.
		if len(pauseMap) > 0 {
			panic(fmt.Errorf("parallelVisit finished with %d paused visitors", len(pauseMap)))
		}

		// Invariant check: if there was no dependency cycle and no cancellation every module
		// should have been visited.
		if visited != toVisit {
			panic(fmt.Errorf("parallelVisit ran %d visitors, expected %d. Unvisited modules map: %v. Try rebuilding with SOONG_SPLIT_ALL_VARIANTS=true. Please file a go/soong-bug if build is successful with the environment variable.", visited, toVisit, hung))
		}
	}

	return nil
}

func cycleError(cycle []*moduleInfo) (errs []error) {
	// The cycle list is in reverse order because all the 'check' calls append
	// their own module to the list.
	errs = append(errs, &BlueprintError{
		Err: fmt.Errorf("encountered dependency cycle:"),
		Pos: cycle[len(cycle)-1].pos,
	})

	// Iterate backwards through the cycle list.
	curModule := cycle[0]
	for i := len(cycle) - 1; i >= 0; i-- {
		nextModule := cycle[i]
		errs = append(errs, &BlueprintError{
			Err: fmt.Errorf("    %s depends on %s",
				curModule, nextModule),
			Pos: curModule.pos,
		})
		curModule = nextModule
	}

	return errs
}

// updateDependencies recursively walks the module dependency graph and updates
// additional fields based on the dependencies.  It builds a sorted list of modules
// such that dependencies of a module always appear first, and populates reverse
// dependency links and counts of total dependencies.  It also reports errors when
// it encounters dependency cycles.  This should be called after resolveDependencies,
// as well as after any mutator pass has called addDependency
func (c *Context) updateDependencies() (errs []error) {
	c.cachedDepsModified = true

	for module := range c.iterateAllVariants() {
		// Reset the forward and reverse deps without reducing their capacity to avoid reallocation.
		module.reverseDeps = module.reverseDeps[:0]
		module.forwardDeps = module.forwardDeps[:0]
	}

	for module := range c.iterateAllVariants() {
		// Add an implicit dependency ordering on all earlier modules in the same module group
		selfIndex := slices.Index(module.group.modules, module)
		module.forwardDeps = slices.Grow(module.forwardDeps, selfIndex+len(module.directDeps))
		module.forwardDeps = append(module.forwardDeps, module.group.modules[:selfIndex]...)

		for _, dep := range module.directDeps {
			module.forwardDeps = append(module.forwardDeps, dep.module)
		}

		for _, dep := range module.forwardDeps {
			dep.reverseDeps = append(dep.reverseDeps, module)
		}
	}

	return
}

// Gets a list of strings from the given list of ninjaStrings by invoking ninjaString.Value on each.
func getNinjaStrings(nStrs []*ninjaString, nameTracker *nameTracker) []string {
	var strs []string
	for _, nstr := range nStrs {
		strs = append(strs, nstr.Value(nameTracker))
	}
	return strs
}

type WeightedOutputsModuleInfo struct {
	Type      string
	DepsCount int
	SrcsCount int
	Outputs   []string
}

func (c *Context) GetWeightedOutputsFromPredicate(predicate func(*WeightedOutputsModuleInfo) (bool, int)) map[string]int {
	outputToWeight := make(map[string]int)
	for m := range c.iterateAllVariants() {
		info := WeightedOutputsModuleInfo{
			Type:      m.typeName,
			DepsCount: len(m.directDeps),
			SrcsCount: 0,
		}
		for _, bDef := range m.actionDefs.buildDefs {
			info.SrcsCount += len(bDef.InputStrings) + len(bDef.Inputs) + len(bDef.ImplicitStrings) + len(bDef.Implicits)
			info.Outputs = append(info.Outputs, bDef.OutputStrings...)
			info.Outputs = append(info.Outputs, bDef.ImplicitOutputStrings...)
			info.Outputs = append(info.Outputs, getNinjaStrings(bDef.Outputs, c.nameTracker)...)
			info.Outputs = append(info.Outputs, getNinjaStrings(bDef.ImplicitOutputs, c.nameTracker)...)
		}
		if ok, weight := predicate(&info); ok {
			for _, o := range info.Outputs {
				if val, ok := outputToWeight[o]; ok {
					if val > weight {
						continue
					}
				}
				outputToWeight[o] = weight
			}
		}
	}
	return outputToWeight
}

// PrepareBuildActions generates an internal representation of all the build
// actions that need to be performed.  This process involves invoking the
// GenerateBuildActions method on each of the Module objects created during the
// parse phase and then on each of the registered Singleton objects.
//
// If the ResolveDependencies method has not already been called it is called
// automatically by this method.
//
// The config argument is made available to all of the Module and Singleton
// objects via the Config method on the ModuleContext and SingletonContext
// objects passed to GenerateBuildActions.  It is also passed to the functions
// specified via PoolFunc, RuleFunc, and VariableFunc so that they can compute
// config-specific values.
//
// The returned deps is a list of the ninja files dependencies that were added
// by the modules and singletons via the ModuleContext.AddNinjaFileDeps(),
// SingletonContext.AddNinjaFileDeps(), and PackageContext.AddNinjaFileDeps()
// methods.

func (c *Context) PrepareBuildActions(config interface{}) (deps []string, errs []error) {
	c.BeginEvent("prepare_build_actions")
	defer c.EndEvent("prepare_build_actions")
	pprof.Do(c.Context, pprof.Labels("blueprint", "PrepareBuildActions"), func(ctx context.Context) {
		c.buildActionsReady = false

		c.liveGlobals = newLiveTracker(c, config)
		// Add all the global rules/variable/pools here because when we restore from
		// cache we don't have the build defs available to build the globals.
		// TODO(b/356414070): Revisit this logic once we have a clearer picture about
		// how the incremental build pieces fit together.
		if c.GetIncrementalEnabled() {
			if c.keyValueStoreCache == nil {
				c.keyValueStoreCache = &KeyValueStoreCache{}
				dbPath := filepath.Join(c.SrcDir(), c.IncrementalDBDir())
				// Remove gob files and all the cached data from the key-value store for a full build.
				if !c.GetIncrementalAnalysis() {
					err := errors.Join(c.keyValueStoreCache.reset(c, dbPath), c.fs.Remove(filepath.Join(dbPath, OrderOnlyStringsCacheFile)))
					if err != nil {
						panic(fmt.Errorf("error resetting incremental db: %w", err))
					}
				}
				if err := c.keyValueStoreCache.open(dbPath); err != nil {
					panic(fmt.Errorf("error opening incremental db: %w", err))
				}
				c.EncContext = gobtools.NewEncContext(c.keyValueStoreCache.referencesDb)
			}

			for _, p := range packageContexts {
				for _, v := range p.scope.variables {
					err := c.liveGlobals.addVariable(v)
					if err != nil {
						errs = []error{err}
						return
					}
				}
				for _, v := range p.scope.rules {
					_, err := c.liveGlobals.addRule(v)
					if err != nil {
						errs = []error{err}
						return
					}
				}
				for _, v := range p.scope.pools {
					err := c.liveGlobals.addPool(v)
					if err != nil {
						errs = []error{err}
						return
					}
				}
			}
		}

		if !c.dependenciesReady {
			var extraDeps []string
			extraDeps, errs = c.resolveDependencies(ctx, config)
			if len(errs) > 0 {
				return
			}
			deps = append(deps, extraDeps...)
		}

		var depsModules []string
		depsModules, errs = c.generateModuleBuildActions(config, c.liveGlobals)
		if len(errs) > 0 {
			return
		}

		pprof.Do(c.Context, pprof.Labels("blueprint", "GC"), func(ctx context.Context) {
			runtime.GC()
		})

		var depsSingletons []string
		depsSingletons, errs = c.generateSingletonBuildActions(config, c.singletonInfo, c.liveGlobals)
		if len(errs) > 0 {
			return
		}

		deps = append(deps, depsModules...)
		deps = append(deps, depsSingletons...)

		if c.outDir != nil {
			err := c.liveGlobals.addNinjaStringDeps(c.outDir)
			if err != nil {
				errs = []error{err}
				return
			}
		}

		pkgNames, depsPackages := c.makeUniquePackageNames(c.liveGlobals)

		deps = append(deps, depsPackages...)

		nameTracker := c.memoizeFullNames(c.liveGlobals, pkgNames)

		// This will panic if it finds a problem since it's a programming error.
		c.checkForVariableReferenceCycles(c.liveGlobals.variables, nameTracker)

		c.nameTracker = nameTracker
		c.globalVariables = c.liveGlobals.variables
		c.globalPools = c.liveGlobals.pools
		c.globalRules = c.liveGlobals.rules

		c.buildActionsReady = true
	})

	if len(errs) > 0 {
		return nil, errs
	}

	return deps, nil
}

func (c *Context) runMutators(ctx context.Context, config interface{}, mutatorGroups [][]*mutatorInfo) (deps []string, errs []error) {
	c.finishedMutators = make([]bool, len(c.mutatorInfo))

	pprof.Do(ctx, pprof.Labels("blueprint", "runMutators"), func(ctx context.Context) {
		mutatorIndexAfterLastCreateModule := -1
		mutatorIndexPartialAnalysis := -1
		for i := len(mutatorGroups) - 1; i >= 0; i-- {
			if mutatorIndexAfterLastCreateModule == -1 && mutatorGroups[i][0].usesCreateModule {
				mutatorIndexAfterLastCreateModule = mutatorGroups[i][0].index
			}
			if mutatorIndexPartialAnalysis == -1 && mutatorGroups[i][0].prePartial {
				mutatorIndexPartialAnalysis = mutatorGroups[i][0].index
			}
		}
		c.mutatorIndexAfterLastCreateModule = mutatorIndexAfterLastCreateModule + 1
		c.mutatorIndexPartialAnalysis = mutatorIndexPartialAnalysis + 1

		for _, mutatorGroup := range mutatorGroups {
			if mutatorGroup[0].index == c.mutatorIndexPartialAnalysis && len(c.partialAnalysisTargets) > 0 {
				targetMap := make(map[string]bool, len(c.partialAnalysisTargets))
				for _, t := range c.partialAnalysisTargets {
					targetMap[t] = false
				}

				foundCount := 0
				for _, m := range c.moduleGroups {
					if alreadyFound, isTarget := targetMap[m.name]; isTarget {
						m.passive = false
						if !alreadyFound {
							targetMap[m.name] = true
							foundCount++
						}
					} else {
						m.passive = true
					}
				}

				if foundCount < len(targetMap) {
					notFound := make([]string, 0, len(targetMap)-foundCount)
					for target, found := range targetMap {
						if !found {
							notFound = append(notFound, target)
						}
					}
					panic(fmt.Sprintf("the requested targets are not found: %v", notFound))
				}
			}
			name := mutatorGroup[0].name
			if len(mutatorGroup) > 1 {
				name += "_plus_" + strconv.Itoa(len(mutatorGroup)-1)
			}
			pprof.Do(ctx, pprof.Labels("mutator", name), func(context.Context) {
				c.BeginEvent(name)
				defer c.EndEvent(name)
				var newDeps []string
				if mutatorGroup[0].transitionPropagateMutator != nil {
					newDeps, errs = c.runMutator(config, mutatorGroup, topDownMutator)
				} else if mutatorGroup[0].bottomUpMutator != nil {
					newDeps, errs = c.runMutator(config, mutatorGroup, bottomUpMutator)
				} else {
					panic("no mutator set on " + mutatorGroup[0].name)
				}
				if len(errs) > 0 {
					return
				}
				deps = append(deps, newDeps...)
			})
			if len(errs) > 0 {
				return
			}
		}
	})

	if len(errs) > 0 {
		return nil, errs
	}

	return deps, nil
}

type mutatorDirection interface {
	run(mutator []*mutatorInfo, ctx *mutatorContext)
	orderer() visitOrderer
	fmt.Stringer
}

type bottomUpMutatorImpl struct{}

func (bottomUpMutatorImpl) run(mutatorGroup []*mutatorInfo, ctx *mutatorContext) {
	for _, mutator := range mutatorGroup {
		ctx.mutator = mutator
		ctx.module.startedMutator = mutator.index
		mutator.bottomUpMutator(ctx)
		ctx.module.finishedMutator = mutator.index
	}
}

func (bottomUpMutatorImpl) orderer() visitOrderer {
	return bottomUpVisitor
}

func (bottomUpMutatorImpl) String() string {
	return "bottom up mutator"
}

type topDownMutatorImpl struct{}

func (topDownMutatorImpl) run(mutatorGroup []*mutatorInfo, ctx *mutatorContext) {
	if len(mutatorGroup) > 1 {
		panic(fmt.Errorf("top down mutator group %s must only have 1 mutator, found %d", mutatorGroup[0].name, len(mutatorGroup)))
	}
	mutatorGroup[0].transitionPropagateMutator(ctx)
}

func (topDownMutatorImpl) orderer() visitOrderer {
	return topDownVisitor
}

func (topDownMutatorImpl) String() string {
	return "top down mutator"
}

var (
	topDownMutator  topDownMutatorImpl
	bottomUpMutator bottomUpMutatorImpl
)

type reverseDep struct {
	module *moduleInfo
	dep    depInfo
}

var mutatorContextPool = pool.New[mutatorContext]()

func (c *Context) runMutator(config interface{}, mutatorGroup []*mutatorInfo,
	direction mutatorDirection) (deps []string, errs []error) {

	type globalStateChange struct {
		reverse             []reverseDep
		rename              []rename
		replace             []replace
		newModules          []*moduleInfo
		onDemandModules     []*moduleInfo
		onDemandReverseDeps []reverseDep
		deps                []string
	}

	type createdOnDemandStateChange struct {
		module              *moduleInfo
		moduleReplaceWithCh chan *moduleInfo
	}

	reverseDeps := make(map[*moduleInfo][]depInfo)
	onDemandReverseDeps := make(map[*moduleInfo]map[*moduleInfo]bool)
	var rename []rename
	var replace []replace
	var newModules []*moduleInfo
	var onDemandModules []*moduleInfo

	errsCh := make(chan []error)
	globalStateCh := make(chan globalStateChange)
	createdOnDemandStateCh := make(chan createdOnDemandStateChange)
	done := make(chan bool)

	c.needsUpdateDependencies = 0

	visit := func(module *moduleInfo, pause pauseFunc) bool {
		if module.splitModules != nil {
			panic("split module found in sorted module list")
		}

		if module.createdOnDemand {
			// Send a request to the main coordinator goroutine to check
			// if the previous mutators need to be run on this on demand variant.
			// This allows the coordinator goroutine to dedupe processing if necessary.
			moduleReplaceWithCh := make(chan *moduleInfo)
			createdOnDemandStateCh <- createdOnDemandStateChange{
				module,
				moduleReplaceWithCh,
			}
			moduleReplaceWith := <-moduleReplaceWithCh
			if module == moduleReplaceWith {
				c.rerunMutatorsOnVariantOnDemand(module, c.mutatorIndexAfterLastCreateModule, mutatorGroup[len(mutatorGroup)-1].index, pause, config) // Mutate till the current mutator.
				globalStateCh <- globalStateChange{
					onDemandModules:     []*moduleInfo{module},
					onDemandReverseDeps: module.newOnDemandReverseDeps,
				}
			}
			module.createdOnDemandReplaceWith = moduleReplaceWith
			return false
		}

		mctx := mutatorContextPool.Get()
		*mctx = mutatorContext{
			baseModuleContext: baseModuleContext{
				context: c,
				config:  config,
				module:  module,
			},
			mutator:   mutatorGroup[0],
			pauseFunc: pause,
		}

		module.startedMutator = mutatorGroup[0].index

		func() {
			defer func() {
				if r := recover(); r != nil {
					in := fmt.Sprintf("%s %q for %s", direction, mutatorGroup[0].name, module)
					if err, ok := r.(panicError); ok {
						err.addIn(in)
						mctx.error(err)
					} else {
						mctx.error(newPanicErrorf(r, in))
					}
				}
			}()
			direction.run(mutatorGroup, mctx)
		}()

		module.finishedMutator = mutatorGroup[len(mutatorGroup)-1].index

		hasErrors := false
		if len(mctx.errs) > 0 {
			errsCh <- mctx.errs
			hasErrors = true
		} else {
			if len(mctx.reverseDeps) > 0 || len(mctx.replace) > 0 || len(mctx.rename) > 0 || len(mctx.newModules) > 0 || len(mctx.ninjaFileDeps) > 0 || len(mctx.module.newOnDemandReverseDeps) > 0 {
				globalStateCh <- globalStateChange{
					reverse:             mctx.reverseDeps,
					replace:             mctx.replace,
					rename:              mctx.rename,
					newModules:          mctx.newModules,
					deps:                mctx.ninjaFileDeps,
					onDemandReverseDeps: module.newOnDemandReverseDeps,
				}
			}
		}
		if module.startedMutator == c.mutatorIndexAfterLastCreateModule {
			// Store core module info after the final CreateModule mutator.
			// This ensures that storeCoreModuleInfo is called on modules
			// created using CreateModule.
			mctx.storeCoreModuleInfo()
		}

		mutatorContextPool.Put(mctx)
		mctx = nil

		return hasErrors
	}

	type onDemandModuleUniqueKey struct {
		group   *moduleGroup
		variant string
	}
	variantToOnDemandModule := map[onDemandModuleUniqueKey]*moduleInfo{}

	// Process errs and reverseDeps in a single goroutine
	go func() {
		for {
			select {
			case newErrs := <-errsCh:
				errs = append(errs, newErrs...)
			case globalStateChange := <-globalStateCh:
				for _, r := range globalStateChange.reverse {
					reverseDeps[r.module] = append(reverseDeps[r.module], r.dep)
				}
				for _, r := range globalStateChange.onDemandReverseDeps {
					if _, exists := onDemandReverseDeps[r.module]; !exists {
						onDemandReverseDeps[r.module] = make(map[*moduleInfo]bool)
					}
					onDemandReverseDeps[r.module][r.dep.module] = true
				}
				replace = append(replace, globalStateChange.replace...)
				rename = append(rename, globalStateChange.rename...)
				newModules = append(newModules, globalStateChange.newModules...)
				onDemandModules = append(onDemandModules, globalStateChange.onDemandModules...)
				deps = append(deps, globalStateChange.deps...)
			case createdOnDemandStateChange := <-createdOnDemandStateCh:
				var variantSb strings.Builder
				mod := createdOnDemandStateChange.module
				for _, mutator := range slices.Sorted(maps.Keys(mod.requestedOnDemandVariant.variations)) {
					variantSb.WriteString("-")
					variantSb.WriteString(mod.requestedOnDemandVariant.variations[mutator])
				}
				uniqueDepKey := onDemandModuleUniqueKey{
					mod.group,
					variantSb.String(),
				}
				if existing, exists := variantToOnDemandModule[uniqueDepKey]; exists {
					createdOnDemandStateChange.moduleReplaceWithCh <- existing
				} else {
					createdOnDemandStateChange.moduleReplaceWithCh <- createdOnDemandStateChange.module
					variantToOnDemandModule[uniqueDepKey] = createdOnDemandStateChange.module
				}
			case <-done:
				return
			}
		}
	}()

	visitErrs := parallelVisit(c.iterateAllVariants(), direction.orderer(), parallelVisitLimit, visit)

	if len(visitErrs) > 0 {
		return nil, visitErrs
	}

	for _, mutator := range mutatorGroup {
		c.finishedMutators[mutator.index] = true
	}

	done <- true

	if len(errs) > 0 {
		return nil, errs
	}

	transitionMutator := mutatorGroup[0].transitionMutator

	if transitionMutator != nil {
		for _, group := range c.moduleGroups {
			for i := 0; i < len(group.modules); i++ {
				module := group.modules[i]

				// Update module group to contain newly split variants
				if module.splitModules != nil {
					group.modules, i = spliceModules(group.modules, i, module.splitModules)
				}

				// Fix up any remaining dependencies on modules that were split into variants
				// by replacing them with the first variant
				for j, dep := range module.directDeps {
					if dep.module.obsoletedByNewVariants {
						module.directDeps[j].module = dep.module.splitModules.firstModule()
					}
				}

				if module.createdBy != nil && module.createdBy.obsoletedByNewVariants {
					module.createdBy = module.createdBy.splitModules.firstModule()
				}
			}
		}

		c.completedTransitionMutators = transitionMutator.index + 1
	} else {
		for _, group := range c.moduleGroups {
			for _, module := range group.modules {
				// Add any new forward dependencies to the reverse dependencies of the dependency to avoid
				// having to call a full c.updateDependencies().
				for _, m := range module.newDirectDeps {
					m.reverseDeps = append(m.reverseDeps, module)
				}
				module.newDirectDeps = nil
			}
		}
	}

	// Add in any new reverse dependencies that were added by the mutator
	for module, deps := range reverseDeps {
		sort.Sort(depSorter{deps, c.nameInterface})
		module.directDeps = append(module.directDeps, deps...)
		for _, dep := range deps {
			module.forwardDeps = append(module.forwardDeps, dep.module)
			dep.module.reverseDeps = append(dep.module.reverseDeps, module)
		}
	}

	// Set forward/reverseDeps of on-demand variants.
	// Sort the deps to ensure deterministic orderding.
	for module, depsAsKeys := range onDemandReverseDeps {
		deps := slices.Collect(maps.Keys(depsAsKeys))
		slices.SortFunc(deps, func(a, b *moduleInfo) int {
			return cmp.Compare(a.variant.name, b.variant.name)
		})
		for _, dep := range deps {
			module.forwardDeps = append(module.forwardDeps, dep)
			dep.reverseDeps = append(dep.reverseDeps, module)
		}
		module.newOnDemandReverseDeps = nil
	}

	for _, module := range newModules {
		errs = c.addModule(module)
		if len(errs) > 0 {
			return nil, errs
		}
	}

	errs = c.handleRenames(rename)
	if len(errs) > 0 {
		return nil, errs
	}

	errs = c.handleReplacements(replace)
	if len(errs) > 0 {
		return nil, errs
	}

	if c.needsUpdateDependencies > 0 {
		errs = c.updateDependencies()
		if len(errs) > 0 {
			return nil, errs
		}
	}

	// Add the on-demand variant into its module group.
	// Since there can be inter-variant deps, it needs to be inserted before its inter-variant rdep.
	insertIntoModuleGroup := func(module *moduleInfo) {
		isInterVariantDep := false
		// Check reverseDeps to see if inter-variant rdep exists.
		for _, rdep := range module.reverseDeps {
			if rdep.group == module.group {
				isInterVariantDep = true
				break
			}
		}
		insertIndex := len(module.group.modules)
		if isInterVariantDep {
			insertIndex = slices.IndexFunc(module.group.modules, func(groupModule *moduleInfo) bool {
				return slices.ContainsFunc(groupModule.directDeps, func(directDep depInfo) bool {
					return directDep.module == module
				})
			})
			if insertIndex == -1 {
				insertIndex = len(module.group.modules)
			}
		}
		module.group.modules = slices.Insert(module.group.modules, insertIndex, module)
	}

	moduleGroupsWithOnDemandModules := make(map[*moduleGroup]bool)

	for _, module := range onDemandModules {
		moduleGroupsWithOnDemandModules[module.group] = true
		insertIntoModuleGroup(module)
		for _, fd := range module.newDirectDeps {
			fd.reverseDeps = append(fd.reverseDeps, module)
			// AddVariationDependency allows adding a dependency on itself, but only if
			// that module is earlier in the module list than this one, since we always
			// run GenerateBuildActions in order for the variants of a module
			if fd.group == module.group && beforeInModuleList(module, fd, module.group.modules) {
				return nil, []error{&BlueprintError{
					Err: fmt.Errorf("%q depends on later version of itself", module.Name()),
					Pos: module.pos,
				}}
			}
		}

		// The module has been created and mutated.
		// For future mutators, this variant is the same as normal variants.
		// Set this flag to false so that we do not try to call `rerunMutatorsOnVariantOnDemand` again.
		module.createdOnDemand = false
		module.createdOnDemandReplaceWith = nil
		module.newDirectDeps = nil
		module.newOnDemandReverseDeps = nil
		module.createdOnDemandSupportedSplits = nil
		module.group.cachedVariantsOnDemand = nil
	}

	for g := range moduleGroupsWithOnDemandModules {
		slices.SortStableFunc(g.modules, func(a, b *moduleInfo) int {
			return cmp.Compare(a.sortIndex, b.sortIndex)
		})
	}

	return deps, errs
}

// Removes modules[i] from the list and inserts newModules... where it was located, returning
// the new slice and the index of the last inserted element
func spliceModules(modules moduleList, i int, newModules moduleList) (moduleList, int) {
	spliceSize := len(newModules)
	newLen := len(modules) + spliceSize - 1
	var dest moduleList
	if cap(modules) >= len(modules)-1+len(newModules) {
		// We can fit the splice in the existing capacity, do everything in place
		dest = modules[:newLen]
	} else {
		dest = make(moduleList, newLen)
		copy(dest, modules[:i])
	}

	// Move the end of the slice over by spliceSize-1
	copy(dest[i+spliceSize:], modules[i+1:])

	// Copy the new modules into the slice
	copy(dest[i:], newModules)

	return dest, i + spliceSize - 1
}

func (c *Context) generateModuleBuildActions(config interface{},
	liveGlobals *liveTracker) ([]string, []error) {

	c.BeginEvent("generateModuleBuildActions")
	defer c.EndEvent("generateModuleBuildActions")
	var deps []string
	var errs []error

	cancelCh := make(chan struct{})
	errsCh := make(chan []error)
	depsCh := make(chan []string)

	go func() {
		for {
			select {
			case <-cancelCh:
				close(cancelCh)
				return
			case newErrs := <-errsCh:
				errs = append(errs, newErrs...)
			case newDeps := <-depsCh:
				deps = append(deps, newDeps...)

			}
		}
	}()

	visitErrs := parallelVisit(c.iterateAllVariants(), bottomUpVisitor, parallelVisitLimit,
		func(module *moduleInfo, pause pauseFunc) bool {
			module.cachedUniqueName = c.nameInterface.UniqueName(newNamespaceContext(module), module.group.name)
			sanitizedName := toNinjaName(module.cachedUniqueName)
			sanitizedVariant := toNinjaName(module.variant.name)

			prefix := moduleNamespacePrefix(sanitizedName + "_" + sanitizedVariant)

			// The parent scope of the moduleContext's local scope gets overridden to be that of the
			// calling Go package on a per-call basis.  Since the initial parent scope doesn't matter we
			// just set it to nil.
			scope := newLocalScope(nil, prefix)

			mctx := &moduleContext{
				baseModuleContext: baseModuleContext{
					context: c,
					config:  config,
					module:  module,
				},
				scope:              scope,
				handledMissingDeps: module.missingDeps == nil,
			}

			// Use a deferred call for this, to avoid errors from trying to evaluate the select()
			// expressions in the configurable values that mctx.evaluator encounters too early.
			defer func() {
				if c.moduleDebugDataChannel != nil {
					c.moduleDebugDataChannel <- getModuleDebugJson(mctx.evaluator, module)
				}
				// The evaluator isn't needed anymore. Avoid possibly cyclic ref that may increase gc load.
				mctx.evaluator = nil
			}()

			module.startedGenerateBuildActions = true

			func() {
				defer func() {
					if r := recover(); r != nil {
						in := fmt.Sprintf("GenerateBuildActions for %s", module)
						if err, ok := r.(panicError); ok {
							err.addIn(in)
							mctx.error(err)
						} else {
							mctx.error(newPanicErrorf(r, in))
						}
					}
				}()
				if !module.restoreModuleBuildActions(c) || c.incrementalProviderTest {
					if !c.SkipCloneModulesAfterMutators {
						// Replaces every build logic module with a clone of itself.  Prevents introducing problems where
						// a mutator sets a non-property member variable on a module, which works until a later mutator
						// creates variants of that module.
						module.logicModule, module.properties = c.cloneLogicModule(module)
						module.logicModule.setInfo(module)
					}

					module.logicModule.GenerateBuildActions(mctx)
				}
				module.calculateProviderHash()
			}()

			module.finishedGenerateBuildActions = true

			if len(mctx.errs) > 0 {
				errsCh <- mctx.errs
				return true
			}

			if module.missingDeps != nil && !mctx.handledMissingDeps && !module.incrementalRestored {
				var errs []error
				for _, depName := range module.missingDeps {
					errs = append(errs, c.missingDependencyError(module, depName))
				}
				errsCh <- errs
				return true
			}

			depsCh <- mctx.ninjaFileDeps

			if module.freeAfterGenerateBuildActions {
				// This module is freed after GenerateBuildActions complete, requiring all future accesses
				// to go through ModuleProxy instead of the Module.
				// Cache Module.Name() and Module.String() for future use in ModuleProxy.Name() and ModuleProxy.String()
				module.cachedName = module.logicModule.Name()
				module.cachedString = module.logicModule.String()
				// When soong debug data is requested, don't remove these info, they will show up in soong-debug-info.json.
				if c.moduleDebugDataChannel == nil {
					// TODO: logicModule is needed to evaluate configurable properties, we should figure out an alternative.
					module.logicModule = nil
					module.properties = nil
					module.propertyPos = nil
				}
			}

			newErrs := c.processLocalBuildActions(&module.actionDefs,
				&mctx.actionDefs, liveGlobals)
			if len(newErrs) > 0 {
				errsCh <- newErrs
				return true
			}
			return false
		})

	cancelCh <- struct{}{}
	<-cancelCh

	errs = append(errs, visitErrs...)

	return deps, errs
}

func (c *Context) WriteIncrementalDebugInfo(filename string, modules []*moduleInfo) {
	f, err := os.Create(filename)
	if err != nil {
		// We expect this to be writable
		panic(fmt.Sprintf("couldn't create incremental module debug file %s: %s", filename, err))
	}
	defer f.Close()

	needComma := false
	f.WriteString("{\n\"modules\": [\n")

	for _, module := range modules {
		if module.incrementalDebugInfo == nil {
			continue
		}
		if needComma {
			f.WriteString(",\n")
		} else {
			needComma = true
		}

		f.Write(module.incrementalDebugInfo)
	}

	f.WriteString("\n]\n}")
}

func (c *Context) generateOneSingletonBuildActions(config interface{},
	info *singletonInfo, liveGlobals *liveTracker) ([]string, []error) {

	var deps []string
	var errs []error

	// The parent scope of the singletonContext's local scope gets overridden to be that of the
	// calling Go package on a per-call basis.  Since the initial parent scope doesn't matter we
	// just set it to nil.
	scope := newLocalScope(nil, singletonNamespacePrefix(info.name))

	sctx := &singletonContext{
		singleton:    info,
		context:      c,
		config:       config,
		scope:        scope,
		globals:      liveGlobals,
		depProviders: make(map[int]bool),
	}

	info.startedGenerateBuildActions = true

	func() {
		defer func() {
			if r := recover(); r != nil {
				in := fmt.Sprintf("GenerateBuildActions for singleton %s", info.name)
				if err, ok := r.(panicError); ok {
					err.addIn(in)
					sctx.error(err)
				} else {
					sctx.error(newPanicErrorf(r, in))
				}
			}
		}()

		c.restoreSingleton(info)
		if !info.incrementalRestored || c.incrementalProviderTest {
			info.singleton.GenerateBuildActions(sctx)
		}
		// A caching entry for singletons is only written if the singleton
		// depends on at least one provider. If zero providers are consumed,
		// the cache restore process would fail to locate
		// a cache entry, forcing the singleton to re-execute on every build.
		//
		// An edge case where a coding change removes a provider dependency is safely handled
		// because any detected code change automatically invalidates the entire global cache,
		// ensuring system consistency.
		if info.buildActionCacheKey != nil && !info.incrementalRestored {
			cache := make(map[int]proptools.Hash)
			for k, _ := range sctx.depProviders {
				// A singleton might depend on both module providers and singleton providers, and
				// the hashes of all the former are stored in context globally, and the hashes of
				// the latter are stored inside the current singletonInfo.
				if providerRegistry[k].mutator == singletonTag {
					cache[k] = info.providerValueHashes[k]
				} else {
					cache[k] = c.providerValueHashes[k]
				}
			}

			var providerHashes []ProviderHash
			for i, p := range info.providers {
				if p == nil {
					continue
				}
				err := c.keyValueStoreCache.writeProvider(c.EncContext, info.providerInitialValueHashes[i], CachedProvider{
					Id:    providerRegistry[i],
					Value: p,
				})
				if err != nil {
					panic(err)
				}
				providerHashes = append(providerHashes,
					ProviderHash{
						Id:   providerRegistry[i],
						Hash: info.providerInitialValueHashes[i],
					})
			}

			if err := c.keyValueStoreCache.writeSingletonBuildAction(c.EncContext, info.buildActionCacheKey,
				&SingletonActionCachedData{
					ProviderHashes:           providerHashes,
					DependencyProviderHashes: cache,
					GlobCache:                info.globCache,
				}); err != nil {
				panic(err)
			}
		}
	}()

	info.finishedGenerateBuildActions = true

	if len(sctx.errs) > 0 {
		errs = append(errs, sctx.errs...)
		return deps, errs
	}

	deps = append(deps, sctx.ninjaFileDeps...)
	info.subninjas = append(info.subninjas, sctx.subninjas...)

	newErrs := c.processLocalBuildActions(&info.actionDefs,
		&sctx.actionDefs, liveGlobals)
	errs = append(errs, newErrs...)
	return deps, errs
}

func (c *Context) restoreSingleton(info *singletonInfo) {
	info.incrementalSupported = c.GetIncrementalEnabled() && info.singleton.IncrementalSupported()
	if !info.incrementalSupported {
		return
	}

	info.buildActionCacheKey = &DataCacheKey{
		Id: info.name,
	}

	// We can pursue a incremental analysis because there is no soong coding change, no
	// product config and env variable changes.
	if !c.GetIncrementalAnalysis() {
		return
	}

	data, err := c.keyValueStoreCache.readSingletonBuildAction(c.EncContext, info.buildActionCacheKey)
	if err != nil {
		panic(err)
	}
	if data == nil {
		return
	}

	// This logic here assumes a singleton's behavior is a pure function of its providers.
	// Conditional access to certain providers must also be based on other provider
	// values, ensuring that any behavioral change is captured by the input providers hashes.
	for k, v := range data.DependencyProviderHashes {
		var hash proptools.Hash
		if providerRegistry[k].mutator == singletonTag {
			hash = info.providerValueHashes[k]
		} else {
			hash = c.providerValueHashes[k]
		}
		if hash != v {
			return
		}
	}

	for _, glob := range data.GlobCache {
		result, err := c.glob(glob.Pattern, glob.Excludes)
		if err != nil {
			panic(newPanicErrorf(err, "failed to glob for cached singleton: %s %s %v", info.name, glob.Pattern, glob.Excludes))
		}
		hash, err := proptools.CalculateHash(stringList(result))
		if err != nil {
			panic(newPanicErrorf(err, "failed to calculate hash for cached glob result: %s", info.name))
		}
		if hash != glob.Result {
			// Don't restore, a glob result has changed.
			return
		}
	}

	info.incrementalRestored = true
	info.providerInitialValueHashes = make([]proptools.Hash, len(providerRegistry))
	info.hasUnrestoredProvider = make([]bool, len(providerRegistry))
	for _, provider := range data.ProviderHashes {
		info.hasUnrestoredProvider[provider.Id.id] = true
		info.providerInitialValueHashes[provider.Id.id] = provider.Hash
	}
}

func (c *Context) generateParallelSingletonBuildActions(config interface{},
	singletons []*singletonInfo, liveGlobals *liveTracker) ([]string, []error) {

	c.BeginEvent("generateParallelSingletonBuildActions")
	defer c.EndEvent("generateParallelSingletonBuildActions")

	var deps []string
	var errs []error

	wg := sync.WaitGroup{}
	cancelCh := make(chan struct{})
	depsCh := make(chan []string)
	errsCh := make(chan []error)

	go func() {
		for {
			select {
			case <-cancelCh:
				close(cancelCh)
				return
			case dep := <-depsCh:
				deps = append(deps, dep...)
			case newErrs := <-errsCh:
				if len(errs) <= maxErrors {
					errs = append(errs, newErrs...)
				}
			}
		}
	}()

	for _, info := range singletons {
		if !info.parallel {
			// Skip any singletons registered with parallel=false.
			continue
		}
		wg.Add(1)
		go func(inf *singletonInfo) {
			defer wg.Done()
			newDeps, newErrs := c.generateOneSingletonBuildActions(config, inf, liveGlobals)
			depsCh <- newDeps
			errsCh <- newErrs
		}(info)
	}
	wg.Wait()

	cancelCh <- struct{}{}
	<-cancelCh

	return deps, errs
}

func (c *Context) calculateProvidersHashes() {
	c.providerValueHashes = make([]proptools.Hash, len(providerRegistry))
	var wg sync.WaitGroup
	for i := range len(providerRegistry) {
		wg.Add(1)
		go func() {
			defer wg.Done()

			var providerHashes []proptools.Hash
			// For singleton providers we collect provider hashes from singletons.
			if providerRegistry[i].mutator != singletonTag {
				c.visitAllModuleInfos(func(m *moduleInfo) {
					if m.providerInitialValueHashes != nil {
						providerHashes = append(providerHashes, m.providerInitialValueHashes[i])
					}
				})
			}
			var err error
			c.providerValueHashes[i], err = proptools.CalculateHash(hashList(providerHashes))
			if err != nil {
				panic(err)
			}
		}()
	}
	wg.Wait()
}

func (c *Context) generateSingletonBuildActions(config interface{},
	singletons []*singletonInfo, liveGlobals *liveTracker) ([]string, []error) {

	c.BeginEvent("generateSingletonBuildActions")
	defer c.EndEvent("generateSingletonBuildActions")

	var deps []string
	var errs []error

	// Run one singleton.  Use a variable to simplify manual validation testing.
	var runSingleton = func(info *singletonInfo) {
		c.BeginEvent("singleton:" + info.name)
		defer c.EndEvent("singleton:" + info.name)
		newDeps, newErrs := c.generateOneSingletonBuildActions(config, info, liveGlobals)
		deps = append(deps, newDeps...)
		errs = append(errs, newErrs...)
	}

	// Force a resort of the module groups before running singletons so that two singletons running in parallel
	// don't cause a data race when they trigger a resort in VisitAllModules.
	c.sortedModuleGroups()

	if c.GetIncrementalEnabled() {
		c.calculateProvidersHashes()
	}

	// First, take care of any singletons that want to run in parallel.
	deps, errs = c.generateParallelSingletonBuildActions(config, singletons, liveGlobals)

	for _, info := range singletons {
		if !info.parallel {
			if c.GetIncrementalEnabled() {
				// A singleton might depend on modules and other singletons that run before it.
				// We already captured the provider hashes of all the modules in calculateProvidersHashes,
				// now we calculate the hashes of all singletons that the current one might depend on.
				info.providerValueHashes = make([]proptools.Hash, len(providerRegistry))
				for i := range len(providerRegistry) {
					var providerHashes []proptools.Hash
					if providerRegistry[i].mutator == singletonTag {
						c.VisitAllSingletons(func(s SingletonProxy) {
							hash := proptools.ZeroHash
							if s.singleton.providerInitialValueHashes != nil {
								hash = s.singleton.providerInitialValueHashes[i]
							}
							providerHashes = append(providerHashes, hash)
						})
					}
					var err error
					info.providerValueHashes[i], err = proptools.CalculateHash(hashList(providerHashes))
					if err != nil {
						panic(err)
					}
				}
			}
			runSingleton(info)
			if len(errs) > maxErrors {
				break
			}
		}
	}

	return deps, errs
}

func (c *Context) processLocalBuildActions(out, in *localBuildActions,
	liveGlobals *liveTracker) []error {

	var errs []error

	// First we go through and add everything referenced by the module's
	// buildDefs to the live globals set.  This will end up adding the live
	// locals to the set as well, but we'll take them out after.
	for _, def := range in.buildDefs {
		err := liveGlobals.AddBuildDefDeps(def)
		if err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errs
	}

	out.buildDefs = append(out.buildDefs, in.buildDefs...)

	// We use the now-incorrect set of live "globals" to determine which local
	// definitions are live.  As we go through copying those live locals to the
	// moduleGroup we remove them from the live globals set.
	for _, v := range in.variables {
		isLive := liveGlobals.RemoveVariableIfLive(v)
		if isLive {
			out.variables = append(out.variables, v)
		}
	}

	for _, r := range in.rules {
		isLive := liveGlobals.RemoveRuleIfLive(r)
		if isLive {
			out.rules = append(out.rules, r)
		}
	}

	return nil
}

func (c *Context) walkDeps(topModule *moduleInfo, allowDuplicates bool,
	visitDown func(depInfo, *moduleInfo) bool, visitUp func(depInfo, *moduleInfo)) {

	visited := make(map[*moduleInfo]bool)
	var visiting *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "WalkDeps(%s, %s, %s) for dependency %s",
				topModule, funcName(visitDown), funcName(visitUp), visiting))
		}
	}()

	var walk func(module *moduleInfo)
	walk = func(module *moduleInfo) {
		for _, dep := range module.directDeps {
			if allowDuplicates || !visited[dep.module] {
				visiting = dep.module
				recurse := true
				if visitDown != nil {
					recurse = visitDown(dep, module)
				}
				if recurse && !visited[dep.module] {
					walk(dep.module)
					visited[dep.module] = true
				}
				if visitUp != nil {
					visitUp(dep, module)
				}
			}
		}
	}

	walk(topModule)
}

type replace struct {
	from, to  *moduleInfo
	predicate ReplaceDependencyPredicate
}

type rename struct {
	group *moduleGroup
	name  string
}

type onDemandDep struct {
	from, to *moduleInfo
	tag      DependencyTag
}

// moduleVariantsThatDependOn takes the name of a module and a dependency and returns the all the variants of the
// module that depends on the dependency.
func (c *Context) moduleVariantsThatDependOn(name string, dep *moduleInfo) []*moduleInfo {
	group := c.moduleGroupFromName(name, dep.namespace())
	var variants []*moduleInfo

	if group == nil {
		return nil
	}

	for _, m := range group.modules {
		for _, moduleDep := range m.directDeps {
			if moduleDep.module == dep {
				variants = append(variants, m)
			}
		}
	}

	return variants
}

func (c *Context) handleRenames(renames []rename) []error {
	var errs []error
	for _, rename := range renames {
		group, name := rename.group, rename.name
		if name == group.name || len(group.modules) < 1 {
			continue
		}

		errs = append(errs, c.nameInterface.Rename(group.name, rename.name, group.namespace)...)
	}

	return errs
}

func (c *Context) handleReplacements(replacements []replace) []error {
	var errs []error
	changedDeps := false
	for _, replace := range replacements {
		for _, m := range replace.from.reverseDeps {
			for i, d := range m.directDeps {
				if d.module == replace.from {
					// If the replacement has a predicate then check it.
					if replace.predicate == nil || replace.predicate(m.logicModule, d.tag, d.module.logicModule) {
						m.directDeps[i].module = replace.to
						changedDeps = true
					}
				}
			}
		}

	}

	if changedDeps {
		c.needsUpdateDependencies++
	}
	return errs
}

func (c *Context) discoveredMissingDependencies(module *moduleInfo, depName string, depVariations variationMap) (errs []error) {
	if !depVariations.empty() {
		depName = depName + "{" + c.prettyPrintVariant(depVariations) + "}"
	}
	if c.allowMissingDependencies {
		module.missingDeps = append(module.missingDeps, depName)
		return nil
	}
	return []error{c.missingDependencyError(module, depName)}
}

func (c *Context) missingDependencyError(module *moduleInfo, depName string) (errs error) {
	guess := namesLike(depName, module.Name(), c.moduleGroups)
	err := c.nameInterface.MissingDependencyError(module.Name(), module.namespace(), depName, guess)
	return &BlueprintError{
		Err: err,
		Pos: module.pos,
	}
}

func (c *Context) moduleGroupFromName(name string, namespace Namespace) *moduleGroup {
	group, exists := c.nameInterface.ModuleFromName(name, namespace)
	if exists {
		return group.moduleGroup
	}
	return nil
}

func (c *Context) sortedModuleGroups() []*moduleGroup {
	moduleLess := func(a, b *moduleInfo) int {
		return cmp.Compare(a.variant.name, b.variant.name)
	}
	if c.cachedSortedModuleGroups == nil || c.cachedDepsModified {
		unwrap := func(wrappers []ModuleGroup) []*moduleGroup {
			result := make([]*moduleGroup, 0, len(wrappers))
			for _, group := range wrappers {
				// Order the module variants in-place
				slices.SortFunc(group.moduleGroup.modules, moduleLess)
				result = append(result, group.moduleGroup)
			}
			return result
		}

		c.cachedSortedModuleGroups = unwrap(c.nameInterface.AllModules())
		c.cachedDepsModified = false
	}

	return c.cachedSortedModuleGroups
}

func (c *Context) visitAllModuleVariants(module *moduleInfo,
	visit func(*moduleInfo)) {

	for _, module := range module.group.modules {
		visit(module)
	}
}

func (c *Context) visitAllModuleInfos(visit func(*moduleInfo)) {
	for _, moduleGroup := range c.sortedModuleGroups() {
		for _, module := range moduleGroup.modules {
			visit(module)
		}
	}
}

func (c *Context) requireNinjaVersion(major, minor, micro int) {
	if major != 1 {
		panic("ninja version with major version != 1 not supported")
	}
	if c.requiredNinjaMinor < minor {
		c.requiredNinjaMinor = minor
		c.requiredNinjaMicro = micro
	}
	if c.requiredNinjaMinor == minor && c.requiredNinjaMicro < micro {
		c.requiredNinjaMicro = micro
	}
}

func (c *Context) setOutDir(value *ninjaString) {
	if c.outDir == nil {
		c.outDir = value
	}
}

func (c *Context) makeUniquePackageNames(
	liveGlobals *liveTracker) (map[*packageContext]string, []string) {

	pkgs := make(map[string]*packageContext)
	pkgNames := make(map[*packageContext]string)
	longPkgNames := make(map[*packageContext]bool)

	processPackage := func(pctx *packageContext) {
		if pctx == nil {
			// This is a built-in rule and has no package.
			return
		}
		if _, ok := pkgNames[pctx]; ok {
			// We've already processed this package.
			return
		}

		otherPkg, present := pkgs[pctx.shortName]
		if present {
			// Short name collision.  Both this package and the one that's
			// already there need to use their full names.  We leave the short
			// name in pkgNames for now so future collisions still get caught.
			longPkgNames[pctx] = true
			longPkgNames[otherPkg] = true
		} else {
			// No collision so far.  Tentatively set the package's name to be
			// its short name.
			pkgNames[pctx] = pctx.shortName
			pkgs[pctx.shortName] = pctx
		}
	}

	// We try to give all packages their short name, but when we get collisions
	// we need to use the full unique package name.
	for v, _ := range liveGlobals.variables {
		processPackage(v.packageContext())
	}
	for p, _ := range liveGlobals.pools {
		processPackage(p.packageContext())
	}
	for r, _ := range liveGlobals.rules {
		processPackage(r.packageContext())
	}

	// Add the packages that had collisions using their full unique names.  This
	// will overwrite any short names that were added in the previous step.
	for pctx := range longPkgNames {
		pkgNames[pctx] = pctx.fullName
	}

	// Create deps list from calls to PackageContext.AddNinjaFileDeps
	deps := []string{}
	for _, pkg := range pkgs {
		deps = append(deps, pkg.ninjaFileDeps...)
	}

	return pkgNames, deps
}

// memoizeFullNames stores the full name of each live global variable, rule and pool since each is
// guaranteed to be used at least twice, once in the definition and once for each usage, and many
// are used much more than once.
func (c *Context) memoizeFullNames(liveGlobals *liveTracker, pkgNames map[*packageContext]string) *nameTracker {
	nameTracker := &nameTracker{
		pkgNames:  pkgNames,
		variables: make(map[Variable]string),
		rules:     make(map[Rule]string),
		pools:     make(map[Pool]string),
	}
	for v := range liveGlobals.variables {
		nameTracker.variables[v] = v.fullName(pkgNames)
	}
	for r := range liveGlobals.rules {
		nameTracker.rules[r] = r.fullName(pkgNames)
	}
	for p := range liveGlobals.pools {
		nameTracker.pools[p] = p.fullName(pkgNames)
	}
	return nameTracker
}

func (c *Context) checkForVariableReferenceCycles(
	variables map[Variable]*ninjaString, nameTracker *nameTracker) {

	visited := make(map[Variable]bool)  // variables that were already checked
	checking := make(map[Variable]bool) // variables actively being checked

	var check func(v Variable) []Variable

	check = func(v Variable) []Variable {
		visited[v] = true
		checking[v] = true
		defer delete(checking, v)

		value := variables[v]
		for _, dep := range value.Variables() {
			if checking[dep] {
				// This is a cycle.
				return []Variable{dep, v}
			}

			if !visited[dep] {
				cycle := check(dep)
				if cycle != nil {
					if cycle[0] == v {
						// We are the "start" of the cycle, so we're responsible
						// for generating the errors.  The cycle list is in
						// reverse order because all the 'check' calls append
						// their own module to the list.
						msgs := []string{"detected variable reference cycle:"}

						// Iterate backwards through the cycle list.
						curName := nameTracker.Variable(v)
						curValue := value.Value(nameTracker)
						for i := len(cycle) - 1; i >= 0; i-- {
							next := cycle[i]
							nextName := nameTracker.Variable(next)
							nextValue := variables[next].Value(nameTracker)

							msgs = append(msgs, fmt.Sprintf(
								"    %q depends on %q", curName, nextName))
							msgs = append(msgs, fmt.Sprintf(
								"    [%s = %s]", curName, curValue))

							curName = nextName
							curValue = nextValue
						}

						// Variable reference cycles are a programming error,
						// not the fault of the Blueprint file authors.
						panic(strings.Join(msgs, "\n"))
					} else {
						// We're not the "start" of the cycle, so we just append
						// our module to the list and return it.
						return append(cycle, v)
					}
				}
			}
		}

		return nil
	}

	for v := range variables {
		if !visited[v] {
			cycle := check(v)
			if cycle != nil {
				panic("inconceivable!")
			}
		}
	}
}

// ModuleTypePropertyStructs returns a mapping from module type name to a list of pointers to
// property structs returned by the factory for that module type.
func (c *Context) ModuleTypePropertyStructs() map[string][]interface{} {
	ret := make(map[string][]interface{}, len(c.moduleFactories))
	for moduleType, factory := range c.moduleFactories {
		_, ret[moduleType] = factory()
	}

	return ret
}

func (c *Context) ModuleTypeFactories() map[string]ModuleFactory {
	return maps.Clone(c.moduleFactories)
}

func (c *Context) ModuleName(logicModule ModuleOrProxy) string {
	return logicModule.info().Name()
}

func (c *Context) ModuleDir(logicModule ModuleOrProxy) string {
	return filepath.Dir(c.BlueprintFile(logicModule))
}

func (c *Context) ModuleSubDir(logicModule ModuleOrProxy) string {
	return logicModule.info().variant.name
}

func (c *Context) ModuleType(logicModule ModuleOrProxy) string {
	return logicModule.info().typeName
}

// ModuleProvider returns the value, if any, for the provider for a module.  If the value for the
// provider was not set it returns nil and false.  The return value should always be considered read-only.
// It panics if called before the appropriate mutator or GenerateBuildActions pass for the provider on the
// module.  The value returned may be a deep copy of the value originally passed to SetProvider.
func (c *Context) ModuleProvider(logicModule ModuleOrProxy, provider AnyProviderKey) (any, bool) {
	return c.provider(logicModule.info(), provider.provider())
}

func (c *Context) BlueprintFile(logicModule ModuleOrProxy) string {
	return logicModule.info().relBlueprintsFile
}

func (c *Context) moduleErrorf(module *moduleInfo, format string,
	args ...interface{}) error {
	if module == nil {
		// This can happen if ModuleErrorf is called from a load hook
		return &BlueprintError{
			Err: fmt.Errorf(format, args...),
		}
	}

	return &ModuleError{
		BlueprintError: BlueprintError{
			Err: fmt.Errorf(format, args...),
			Pos: module.pos,
		},
		module: module,
	}
}

func (c *Context) ModuleErrorf(logicModule ModuleOrProxy, format string,
	args ...interface{}) error {
	return c.moduleErrorf(logicModule.info(), format, args...)
}

func (c *Context) PropertyErrorf(logicModule ModuleOrProxy, property string, format string,
	args ...interface{}) error {

	module := logicModule.info()
	if module == nil {
		// This can happen if PropertyErrorf is called from a load hook
		return &BlueprintError{
			Err: fmt.Errorf(format, args...),
		}
	}

	pos := module.propertyPos[property]
	if !pos.IsValid() {
		pos = module.pos
	}

	return &PropertyError{
		ModuleError: ModuleError{
			BlueprintError: BlueprintError{
				Err: fmt.Errorf(format, args...),
				Pos: pos,
			},
			module: module,
		},
		property: property,
	}
}

func (c *Context) VisitAllModules(visit func(Module)) {
	var visitingModule *moduleInfo
	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitAllModules(%s) for %s",
				funcName(visit), visitingModule))
		}
	}()

	c.visitAllModuleInfos(func(module *moduleInfo) {
		visitingModule = module
		if module.logicModule == nil {
			panic(fmt.Errorf("VisitAllModules visited module %s that called FreeAfterGenerateBuildActions()", module))
		}
		visit(module.logicModule)
	})
}

func (c *Context) VisitAllModulesIf(pred func(Module) bool, visit func(Module)) {
	var visitingModule *moduleInfo
	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitAllModulesIf(%s, %s) for %s",
				funcName(pred), funcName(visit), visitingModule))
		}
	}()

	c.visitAllModuleInfos(func(module *moduleInfo) {
		visitingModule = module
		if module.logicModule == nil {
			panic(fmt.Errorf("VisitAllModulesIf visited module %s that called FreeAfterGenerateBuildActions()", module))
		}
		if pred(module.logicModule) {
			visit(module.logicModule)
		}
	})
}

func (c *Context) VisitAllModulesProxies(visit func(ModuleProxy)) {
	var visitingModule *moduleInfo
	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitAllModules(%s) for %s",
				funcName(visit), visitingModule))
		}
	}()

	c.visitAllModuleInfos(func(module *moduleInfo) {
		visitingModule = module
		visit(ModuleProxy{module})
	})
}

func (c *Context) VisitAllModulesOrProxies(visit func(ModuleOrProxy)) {
	var visitingModule *moduleInfo
	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitAllModules(%s) for %s",
				funcName(visit), visitingModule))
		}
	}()

	c.visitAllModuleInfos(func(module *moduleInfo) {
		visitingModule = module
		if module.logicModule != nil {
			visit(module.logicModule)
		} else {
			visit(ModuleProxy{module})
		}
	})

}

func (c *Context) VisitDirectDeps(module Module, visit func(Module)) {
	c.VisitDirectDepsWithTags(module, func(m Module, _ DependencyTag) {
		visit(m)
	})
}

func (c *Context) VisitDirectDepsProxies(module ModuleOrProxy, visit func(ModuleProxy)) {
	topModule := module.info()

	var visiting *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDirectDepsProxies(%s, %s) for dependency %s",
				topModule, funcName(visit), visiting))
		}
	}()

	for _, dep := range topModule.directDeps {
		visiting = dep.module
		visit(ModuleProxy{dep.module})
	}
}

func (c *Context) VisitDirectDepsProxiesWithTags(module ModuleOrProxy, visit func(ModuleProxy, DependencyTag)) {
	topModule := module.info()

	var visiting *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDirectDepsProxies(%s, %s) for dependency %s",
				topModule, funcName(visit), visiting))
		}
	}()

	for _, dep := range topModule.directDeps {
		visiting = dep.module
		visit(ModuleProxy{dep.module}, dep.tag)
	}
}

func (c *Context) VisitDirectDepsWithTags(module Module, visit func(Module, DependencyTag)) {
	topModule := module.info()

	var visiting *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDirectDeps(%s, %s) for dependency %s",
				topModule, funcName(visit), visiting))
		}
	}()

	for _, dep := range topModule.directDeps {
		visiting = dep.module
		if dep.module.logicModule == nil {
			panic(fmt.Errorf("VisitDirectDepsWithTags visited module %s that called FreeAfterGenerateBuildActions()", dep.module))
		}
		visit(dep.module.logicModule, dep.tag)
	}
}

func (c *Context) VisitDirectDepsIf(module Module, pred func(Module) bool, visit func(Module)) {
	topModule := module.info()

	var visiting *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDirectDepsIf(%s, %s, %s) for dependency %s",
				topModule, funcName(pred), funcName(visit), visiting))
		}
	}()

	for _, dep := range topModule.directDeps {
		visiting = dep.module
		if dep.module.logicModule == nil {
			panic(fmt.Errorf("VisitDirectDepsIf visited module %s that called FreeAfterGenerateBuildActions()", dep.module))
		}
		if pred(dep.module.logicModule) {
			visit(dep.module.logicModule)
		}
	}
}

func (c *Context) VisitDepsDepthFirst(module Module, visit func(Module)) {
	topModule := module.info()

	var visiting *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDepsDepthFirst(%s, %s) for dependency %s",
				topModule, funcName(visit), visiting))
		}
	}()

	c.walkDeps(topModule, false, nil, func(dep depInfo, parent *moduleInfo) {
		visiting = dep.module
		if dep.module.logicModule == nil {
			panic(fmt.Errorf("VisitDepsDepthFirst visited module %s that called FreeAfterGenerateBuildActions()", dep.module))
		}
		visit(dep.module.logicModule)
	})
}

func (c *Context) VisitDepsDepthFirstIf(module Module, pred func(Module) bool, visit func(Module)) {
	topModule := module.info()

	var visiting *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDepsDepthFirstIf(%s, %s, %s) for dependency %s",
				topModule, funcName(pred), funcName(visit), visiting))
		}
	}()

	c.walkDeps(topModule, false, nil, func(dep depInfo, parent *moduleInfo) {
		if dep.module.logicModule == nil {
			panic(fmt.Errorf("VisitDepsDepthFirstIf visited module %s that called FreeAfterGenerateBuildActions()", dep.module))
		}
		if pred(dep.module.logicModule) {
			visiting = dep.module
			visit(dep.module.logicModule)
		}
	})
}

func (c *Context) PrimaryModule(module Module) Module {
	return c.primaryModule(module.info()).logicModule
}

func (c *Context) primaryModule(moduleInfo *moduleInfo) *moduleInfo {
	return moduleInfo.group.modules.firstModule()
}

func (c *Context) IsPrimaryModule(module ModuleOrProxy) bool {
	return module.info().group.modules.firstModule() == module.info()
}

func (c *Context) IsFinalModule(module ModuleOrProxy) bool {
	return module.info().group.modules.lastModule() == module.info()
}

func (c *Context) VisitAllModuleVariants(module ModuleOrProxy, visit func(Module)) {
	var visitingModule *moduleInfo
	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitAllModuleVariants(%s) for %s",
				funcName(visit), visitingModule))
		}
	}()

	c.visitAllModuleVariants(module.info(), func(module *moduleInfo) {
		visitingModule = module
		if module.logicModule == nil {
			panic(fmt.Errorf("VisitAllModuleVariants visited module %s that called FreeAfterGenerateBuildActions()", module))
		}

		visit(module.logicModule)
	})
}

func (c *Context) VisitAllModuleVariantProxies(module ModuleProxy, visit func(ModuleProxy)) {
	var visitingModule *moduleInfo
	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitAllModuleVariantProxies(%s) for %s",
				funcName(visit), visitingModule))
		}
	}()

	c.visitAllModuleVariants(module.info(), func(module *moduleInfo) {
		visitingModule = module
		visit(ModuleProxy{module})
	})
}

func (c *Context) VisitAllSingletons(visit func(singleton SingletonProxy)) {
	var singleton *singletonInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitAllSingletons(%s) for %s",
				funcName(visit), singleton.name))
		}
	}()

	for _, singleton = range c.singletonInfo {
		// Only return the finished ones
		if singleton.finishedGenerateBuildActions {
			visit(SingletonProxy{singleton: singleton})
		}
	}
}

func (c *Context) ModuleToProxy(module ModuleOrProxy) ModuleProxy {
	return ModuleProxy{module.info()}
}

func (c *Context) CaptureBuildParams() {
	c.captureBuildParams = true
}

func (c *Context) BuildParamsForModule(module ModuleOrProxy) []BuildParams {
	if p := module.info().buildParams; p != nil {
		return *p
	}
	return nil
}

// Singletons returns a list of all registered Singletons.
func (c *Context) Singletons() []Singleton {
	var ret []Singleton
	for _, s := range c.singletonInfo {
		ret = append(ret, s.singleton)
	}
	return ret
}

// SingletonName returns the name that the given singleton was registered with.
func (c *Context) SingletonName(singleton Singleton) string {
	for _, s := range c.singletonInfo {
		if s.singleton == singleton {
			return s.name
		}
	}
	return ""
}

func (c *Context) singletonByName(name string) *singletonInfo {
	for _, s := range c.singletonInfo {
		if s.name == name {
			return s
		}
	}
	return nil
}

// Checks that the hashes of all the providers match the hashes from when they were first set.
// Does nothing on success, returns a list of errors otherwise. It's recommended to run this
// in a goroutine.
func (c *Context) VerifyProvidersWereUnchanged() []error {
	if !c.buildActionsReady {
		return []error{ErrBuildActionsNotReady}
	}

	errors := parallelVisitSimple(c.iterateAllVariants(), 1000, func(m *moduleInfo, _ int) []error {
		var errors []error
		for i, provider := range m.providers {
			if provider != nil {
				hash, err := proptools.CalculateHashReflection(provider)
				if err != nil {
					errors = append(errors, fmt.Errorf("provider %q on module %q was modified after being set, and no longer hashable afterwards: %s", providerRegistry[i].typ, m.Name(), err.Error()))
					continue
				}
				if m.providerInitialValueHashes[i] != hash {
					errors = append(errors, fmt.Errorf("provider %q on module %q was modified after being set", providerRegistry[i].typ, m.Name()))
				}
			} else if m.providerInitialValueHashes[i] != proptools.ZeroHash && !m.hasUnrestoredProvider[i] {
				// This should be unreachable, because in setProvider we check if the provider has already been set.
				errors = append(errors, fmt.Errorf("provider %q on module %q was unset somehow, this is an internal error", providerRegistry[i].typ, m.Name()))
			}
		}
		return errors
	})

	return errors
}

// WriteBuildFile writes the Ninja manifest text for the generated build
// actions to w.  If this is called before PrepareBuildActions successfully
// completes then ErrBuildActionsNotReady is returned.
func (c *Context) WriteBuildFile(w StringWriterWriter, shardNinja bool, ninjaFileName string) error {
	var err error
	pprof.Do(c.Context, pprof.Labels("blueprint", "WriteBuildFile"), func(ctx context.Context) {
		if !c.buildActionsReady {
			err = ErrBuildActionsNotReady
			return
		}

		nw := newNinjaWriter(w)

		if err = c.writeBuildFileHeader(nw); err != nil {
			return
		}

		if err = c.writeNinjaRequiredVersion(nw); err != nil {
			return
		}

		if err = c.writeSubninjas(nw, c.subninjas); err != nil {
			return
		}

		// TODO: Group the globals by package.

		if err = c.writeGlobalVariables(nw); err != nil {
			return
		}

		if err = c.writeGlobalPools(nw); err != nil {
			return
		}

		if err = c.writeBuildDir(nw); err != nil {
			return
		}

		if err = c.writeGlobalRules(nw); err != nil {
			return
		}

		if err = c.writeAllModuleActions(nw, shardNinja, ninjaFileName); err != nil {
			return
		}

		if err = c.writeAllSingletonActions(nw); err != nil {
			return
		}
	})

	return err
}

type pkgAssociation struct {
	PkgName string
	PkgPath string
}

type pkgAssociationSorter struct {
	pkgs []pkgAssociation
}

func (s *pkgAssociationSorter) Len() int {
	return len(s.pkgs)
}

func (s *pkgAssociationSorter) Less(i, j int) bool {
	iName := s.pkgs[i].PkgName
	jName := s.pkgs[j].PkgName
	return iName < jName
}

func (s *pkgAssociationSorter) Swap(i, j int) {
	s.pkgs[i], s.pkgs[j] = s.pkgs[j], s.pkgs[i]
}

func (c *Context) writeBuildFileHeader(nw *ninjaWriter) error {
	var pkgs []pkgAssociation
	maxNameLen := 0
	for pkg, name := range c.nameTracker.pkgNames {
		pkgs = append(pkgs, pkgAssociation{
			PkgName: name,
			PkgPath: pkg.pkgPath,
		})
		if len(name) > maxNameLen {
			maxNameLen = len(name)
		}
	}

	for i := range pkgs {
		pkgs[i].PkgName += strings.Repeat(" ", maxNameLen-len(pkgs[i].PkgName))
	}

	sort.Sort(&pkgAssociationSorter{pkgs})

	params := map[string]interface{}{
		"Pkgs": pkgs,
	}

	buf := bytes.NewBuffer(nil)
	err := fileHeaderTemplate.Execute(buf, params)
	if err != nil {
		return err
	}

	return nw.Comment(buf.String())
}

func (c *Context) writeNinjaRequiredVersion(nw *ninjaWriter) error {
	value := fmt.Sprintf("%d.%d.%d", c.requiredNinjaMajor, c.requiredNinjaMinor,
		c.requiredNinjaMicro)

	err := nw.Assign("ninja_required_version", value)
	if err != nil {
		return err
	}

	return nw.BlankLine()
}

func (c *Context) writeSubninjas(nw *ninjaWriter, subninjas []string) error {
	for _, subninja := range subninjas {
		err := nw.Subninja(subninja)
		if err != nil {
			return err
		}
	}
	return nw.BlankLine()
}

func (c *Context) writeBuildDir(nw *ninjaWriter) error {
	if c.outDir != nil {
		err := nw.Assign("builddir", c.outDir.Value(c.nameTracker))
		if err != nil {
			return err
		}

		err = nw.BlankLine()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Context) writeGlobalVariables(nw *ninjaWriter) error {
	visited := make(map[Variable]bool)

	var walk func(v Variable) error
	walk = func(v Variable) error {
		visited[v] = true

		// First visit variables on which this variable depends.
		value := c.globalVariables[v]
		for _, dep := range value.Variables() {
			if !visited[dep] {
				err := walk(dep)
				if err != nil {
					return err
				}
			}
		}

		err := nw.Assign(c.nameTracker.Variable(v), value.Value(c.nameTracker))
		if err != nil {
			return err
		}

		err = nw.BlankLine()
		if err != nil {
			return err
		}

		return nil
	}

	globalVariables := make([]Variable, 0, len(c.globalVariables))
	for variable := range c.globalVariables {
		globalVariables = append(globalVariables, variable)
	}

	slices.SortFunc(globalVariables, func(a, b Variable) int {
		return cmp.Compare(c.nameTracker.Variable(a), c.nameTracker.Variable(b))
	})

	for _, v := range globalVariables {
		if !visited[v] {
			err := walk(v)
			if err != nil {
				return nil
			}
		}
	}

	return nil
}

func (c *Context) writeGlobalPools(nw *ninjaWriter) error {
	globalPools := make([]Pool, 0, len(c.globalPools))
	for pool := range c.globalPools {
		globalPools = append(globalPools, pool)
	}

	slices.SortFunc(globalPools, func(a, b Pool) int {
		return cmp.Compare(c.nameTracker.Pool(a), c.nameTracker.Pool(b))
	})

	for _, pool := range globalPools {
		name := c.nameTracker.Pool(pool)
		def := c.globalPools[pool]
		err := def.WriteTo(nw, name)
		if err != nil {
			return err
		}

		err = nw.BlankLine()
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Context) writeGlobalRules(nw *ninjaWriter) error {
	globalRules := make([]Rule, 0, len(c.globalRules))
	for rule := range c.globalRules {
		globalRules = append(globalRules, rule)
	}

	slices.SortFunc(globalRules, func(a, b Rule) int {
		return cmp.Compare(c.nameTracker.Rule(a), c.nameTracker.Rule(b))
	})

	for _, rule := range globalRules {
		name := c.nameTracker.Rule(rule)
		def := c.globalRules[rule]
		err := def.WriteTo(nw, name, c.nameTracker)
		if err != nil {
			return err
		}

		err = nw.BlankLine()
		if err != nil {
			return err
		}
	}

	return nil
}

type depSorter struct {
	deps          []depInfo
	nameInterface NameInterface
}

func (s depSorter) Len() int {
	return len(s.deps)
}

func (s depSorter) Less(i, j int) bool {
	return moduleLess(s.deps[i].module, s.deps[j].module, s.nameInterface)
}

func (s depSorter) Swap(i, j int) {
	s.deps[i], s.deps[j] = s.deps[j], s.deps[i]
}

func moduleLess(iMod, jMod *moduleInfo, nameInterface NameInterface) bool {
	iName := nameInterface.UniqueName(newNamespaceContext(iMod), iMod.group.name)
	jName := nameInterface.UniqueName(newNamespaceContext(jMod), jMod.group.name)
	if iName == jName {
		iVariantName := iMod.variant.name
		jVariantName := jMod.variant.name
		if iVariantName == jVariantName {
			panic(fmt.Sprintf("duplicate module name: %s %s: %#v and %#v\n",
				iName, iVariantName, iMod.variant.variations, jMod.variant.variations))
		} else {
			return iVariantName < jVariantName
		}
	} else {
		return iName < jName
	}
}

func GetNinjaShardFiles(ninjaFile string) []string {
	suffix := ".ninja"
	if !strings.HasSuffix(ninjaFile, suffix) {
		panic(fmt.Errorf("ninja file name in wrong format : %s", ninjaFile))
	}
	base := strings.TrimSuffix(ninjaFile, suffix)
	ninjaShardCnt := 10
	fileNames := make([]string, ninjaShardCnt)

	for i := 0; i < ninjaShardCnt; i++ {
		fileNames[i] = fmt.Sprintf("%s.%d%s", base, i, suffix)
	}
	return fileNames
}

func (c *Context) writeAllModuleActions(nw *ninjaWriter, shardNinja bool, ninjaFileName string) error {
	c.BeginEvent("modules")
	defer c.EndEvent("modules")

	var modules []*moduleInfo

	for module := range c.iterateAllVariants() {
		modules = append(modules, module)
	}

	// cachedNameModuleLess is similar to moduleLess, but uses the namespaced name cached in
	// moduleInfo.uniqueName instead of recomputing it each time.
	cachedNameModuleLess := func(a, b *moduleInfo) int {
		if a.cachedUniqueName == b.cachedUniqueName {
			if a.variant.name == b.variant.name {
				panic(fmt.Sprintf("duplicate module name: %s %s: %#v and %#v\n",
					a.cachedUniqueName, a.variant.name, a.variant.variations, a.variant.variations))
			} else {
				return cmp.Compare(a.variant.name, b.variant.name)
			}
		} else {
			return cmp.Compare(a.cachedUniqueName, b.cachedUniqueName)
		}
	}

	slices.SortFunc(modules, cachedNameModuleLess)

	phonys := c.deduplicateOrderOnlyDeps(modules)

	c.EventHandler.Do("sort_phony_builddefs", func() {
		// sorting for determinism, the phony output names are stable
		sort.Slice(phonys.buildDefs, func(i int, j int) bool {
			return phonys.buildDefs[i].OutputStrings[0] < phonys.buildDefs[j].OutputStrings[0]
		})
	})

	if err := c.writeLocalBuildActions(nw, phonys); err != nil {
		return err
	}

	if shardNinja {
		var wg sync.WaitGroup
		errorCh := make(chan error)
		files := GetNinjaShardFiles(ninjaFileName)
		shardedModules := proptools.ShardByCount(modules, len(files))
		for i, batchModules := range shardedModules {
			file := files[i]
			wg.Add(1)
			go func(file string, batchModules []*moduleInfo) {
				defer wg.Done()
				f, err := pathtools.OpenWithTruncateOnClose(c.fs, JoinPath(c.SrcDir(), file))
				if err != nil {
					errorCh <- fmt.Errorf("error opening Ninja file shard: %s", err)
					return
				}
				defer func() {
					err := f.Close()
					if err != nil {
						errorCh <- err
					}
				}()
				buf := bufio.NewWriterSize(f, 16*1024*1024)
				defer func() {
					err := buf.Flush()
					if err != nil {
						errorCh <- err
					}
				}()
				writer := newNinjaWriter(buf)
				err = c.writeIncrementalModules(batchModules, writer)
				if err != nil {
					errorCh <- err
				}
			}(file, batchModules)
			nw.Subninja(file)
		}

		if c.GetIncrementalEnabled() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				parallelVisitSimple(slices.Values(modules), parallelVisitLimit,
					func(m *moduleInfo, _ int) []error {
						if !m.incrementalRestored {
							m.cacheModuleBuildActions(c.EncContext, c.keyValueStoreCache)
						}
						return nil
					})
			}()
		}

		go func() {
			wg.Wait()
			close(errorCh)
		}()

		if c.incrementalDebugFile != "" {
			c.WriteIncrementalDebugInfo(c.incrementalDebugFile, modules)
		}

		var errors []error
		for newErrors := range errorCh {
			errors = append(errors, newErrors)
		}
		if len(errors) > 0 {
			return proptools.MergeErrors(errors)
		}
		return nil
	} else {
		return c.writeModuleAction(modules, nw)
	}
}

// A simplified version of parallelVisit where multiple calls to it can be run at the same time.
func parallelVisitSimple(moduleIter iter.Seq[*moduleInfo], limit int, visit func(module *moduleInfo, idx int) []error) []error {
	type moduleToProcess struct {
		module *moduleInfo
		index  int
	}
	toProcess := make(chan moduleToProcess)
	errorCh := make(chan []error)
	var wg sync.WaitGroup
	go func() {
		idx := 0
		for m := range moduleIter {
			toProcess <- moduleToProcess{m, idx}
			idx++
		}
		close(toProcess)
	}()
	for i := 0; i < limit; i++ {
		wg.Add(1)
		go func() {
			var errors []error
			for m := range toProcess {
				errors = append(errors, visit(m.module, m.index)...)
			}
			if len(errors) > 0 {
				errorCh <- errors
			}
			wg.Done()
		}()
	}
	go func() {
		wg.Wait()
		close(errorCh)
	}()

	var errors []error
	for newErrors := range errorCh {
		errors = append(errors, newErrors...)
	}
	return errors
}
func (c *Context) writeIncrementalModules(modules []*moduleInfo, baseWriter *ninjaWriter) error {
	// Use a sync.Pool to reuse buffers and reduce memory allocations.
	bufferPool := sync.Pool{
		New: func() any {
			// Pre-allocate a reasonably sized buffer for each new writer.
			return bytes.NewBuffer(make([]byte, 0, 128*1024))
		},
	}
	headerBufPool := sync.Pool{New: func() any { return new(bytes.Buffer) }}

	writeSignals := make([]chan struct{}, len(modules))
	for i := range writeSignals {
		writeSignals[i] = make(chan struct{})
	}

	errs := parallelVisitSimple(slices.Values(modules), parallelVisitLimit, func(m *moduleInfo, idx int) []error {
		var moduleBytes []byte
		var err error

		if m.incrementalRestored && !c.incrementalProviderTest {
			// Read from the cache if the module is restored.
			moduleBytes, err = c.keyValueStoreCache.readNinjaStatements(m.buildActionCacheKey)
		} else {
			// Generate statements for dirty modules.
			inMemoryWriter := bufferPool.Get().(*bytes.Buffer)
			inMemoryWriter.Reset()
			defer bufferPool.Put(inMemoryWriter)

			headerBuf := headerBufPool.Get().(*bytes.Buffer)
			headerBuf.Reset()
			defer headerBufPool.Put(headerBuf)

			mWriter := newNinjaWriter(inMemoryWriter)
			err = c.writeOneModuleAction(m, mWriter, headerBuf)
			if err == nil {
				// Make a copy of the bytes, as the buffer will be reused.
				moduleBytes = slices.Clone(inMemoryWriter.Bytes())
				// Write the newly generated statements back to the cache for incremental module.
				if m.buildActionCacheKey != nil {
					err = c.keyValueStoreCache.writeNinjaStatements(m.buildActionCacheKey, moduleBytes)
				}
			}
		}

		// Wait for the signal from the previous thread before writing even when an
		// error happens to ensure no concurrently writing of the ninja file.
		if idx > 0 {
			<-writeSignals[idx-1]
		}

		if err == nil {
			baseWriter.writer.Write(moduleBytes)
		}

		// Signal the next thread that it's clear to write.
		close(writeSignals[idx])
		if err != nil {
			return []error{err}
		}
		return nil
	})

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (c *Context) writeModuleAction(modules []*moduleInfo, nw *ninjaWriter) error {
	buf := bytes.NewBuffer(nil)
	for _, module := range modules {
		if err := c.writeOneModuleAction(module, nw, buf); err != nil {
			return err
		}
	}
	return nil
}

func (c *Context) writeOneModuleAction(module *moduleInfo, nw *ninjaWriter, buf *bytes.Buffer) error {
	if len(module.actionDefs.variables)+len(module.actionDefs.rules)+len(module.actionDefs.buildDefs) == 0 {
		return nil
	}
	buf.Reset()

	// In order to make the bootstrap build manifest independent of the
	// build dir we need to output the Blueprints file locations in the
	// comments as paths relative to the source directory.
	relPos := module.pos
	relPos.Filename = module.relBlueprintsFile

	// Get the name and location of the factory function for the module.
	factoryFunc := runtime.FuncForPC(reflect.ValueOf(module.factory).Pointer())
	factoryName := factoryFunc.Name()

	infoMap := map[string]interface{}{
		"name":      module.Name(),
		"typeName":  module.typeName,
		"goFactory": factoryName,
		"pos":       relPos,
		"variant":   module.variant.name,
	}
	if err := moduleHeaderTemplate.Execute(buf, infoMap); err != nil {
		return err
	}

	if err := nw.Comment(buf.String()); err != nil {
		return err
	}

	if err := nw.BlankLine(); err != nil {
		return err
	}

	if err := c.writeLocalBuildActions(nw, &module.actionDefs); err != nil {
		return err
	}

	if err := nw.BlankLine(); err != nil {
		return err
	}

	return nil
}

func (c *Context) writeAllSingletonActions(nw *ninjaWriter) error {
	c.BeginEvent("singletons")
	defer c.EndEvent("singletons")

	buf := bytes.NewBuffer(nil)
	var ninjaBytes []byte
	var err error
	inMemoryWriter := bytes.NewBuffer(nil)

	for _, info := range c.singletonInfo {
		if info.incrementalRestored && !c.incrementalProviderTest {
			// Read from the cache if the singleton is restored.
			ninjaBytes, err = c.keyValueStoreCache.readNinjaStatements(info.buildActionCacheKey)
			if err != nil {
				return err
			}
		} else {
			if len(info.actionDefs.variables)+len(info.actionDefs.rules)+len(info.actionDefs.buildDefs)+len(info.subninjas) == 0 {
				continue
			}

			inMemoryWriter.Reset()
			sWriter := newNinjaWriter(inMemoryWriter)
			// Get the name of the factory function for the module.
			factory := info.factory
			factoryFunc := runtime.FuncForPC(reflect.ValueOf(factory).Pointer())
			factoryName := factoryFunc.Name()

			buf.Reset()
			infoMap := map[string]interface{}{
				"name":      info.name,
				"goFactory": factoryName,
			}
			err = singletonHeaderTemplate.Execute(buf, infoMap)
			if err != nil {
				return err
			}

			err = sWriter.Comment(buf.String())
			if err != nil {
				return err
			}

			err = sWriter.BlankLine()
			if err != nil {
				return err
			}

			err = c.writeLocalBuildActions(sWriter, &info.actionDefs)
			if err != nil {
				return err
			}

			err = c.writeSubninjas(sWriter, info.subninjas)

			err = sWriter.BlankLine()
			if err != nil {
				return err
			}
			ninjaBytes = inMemoryWriter.Bytes()

			if info.incrementalSupported {
				c.keyValueStoreCache.writeNinjaStatements(info.buildActionCacheKey, ninjaBytes)
			}
		}
		nw.writer.Write(ninjaBytes)
	}

	return nil
}

func (c *Context) GetEventHandler() *metrics.EventHandler {
	return c.EventHandler
}

func (c *Context) BeginEvent(name string) {
	c.EventHandler.Begin(name)
}

func (c *Context) EndEvent(name string) {
	c.EventHandler.End(name)
}

func (c *Context) SetBeforePrepareBuildActionsHook(hookFn func() error) {
	c.BeforePrepareBuildActionsHook = hookFn
}

func (c *Context) RestoredGlobMetrics() pathtools.RestoredGlobsMetrics {
	return c.restoredGlobMetrics
}

// keyForPhonyCandidate gives a unique identifier for a set of deps.
func keyForPhonyCandidate(stringDeps []string) uint64 {
	hasher := fnv.New64a()
	write := func(s string) {
		// The hasher doesn't retain or modify the input slice, so pass the string data directly to avoid
		// an extra allocation and copy.
		_, err := hasher.Write(unsafe.Slice(unsafe.StringData(s), len(s)))
		if err != nil {
			panic(fmt.Errorf("write failed: %w", err))
		}
	}
	for _, d := range stringDeps {
		write(d)
	}
	return hasher.Sum64()
}

// deduplicateOrderOnlyDeps searches for common sets of order-only dependencies across all
// buildDef instances in the provided moduleInfo instances. Each such
// common set forms a new buildDef representing a phony output that then becomes
// the sole order-only dependency of those buildDef instances
func (c *Context) deduplicateOrderOnlyDeps(modules []*moduleInfo) *localBuildActions {
	c.BeginEvent("deduplicate_order_only_deps")
	defer c.EndEvent("deduplicate_order_only_deps")

	var phonys []*buildDef
	if c.orderOnlyStringsCache == nil {
		c.orderOnlyStringsCache = make(OrderOnlyStringsCache)
	}
	c.orderOnlyStrings.Range(func(key uniquelist.UniqueList[string], info *orderOnlyStringsInfo) bool {
		if info.dedup {
			dedup := fmt.Sprintf("dedup-%x", keyForPhonyCandidate(key.ToSlice()))
			phony := &buildDef{
				Rule:          Phony,
				OutputStrings: []string{dedup},
				InputStrings:  key.ToSlice(),
			}
			info.dedupName = dedup
			phonys = append(phonys, phony)
			if info.incremental {
				c.orderOnlyStringsCache[phony.OutputStrings[0]] = phony.InputStrings
			}
		}
		return true
	})

	parallelVisit(slices.Values(modules), unorderedVisitorImpl{}, parallelVisitLimit,
		func(m *moduleInfo, pause pauseFunc) bool {
			for _, def := range m.actionDefs.buildDefs {
				if info, loaded := c.orderOnlyStrings.Load(def.OrderOnlyStrings); loaded {
					if info.dedup {
						def.OrderOnlyStrings = uniquelist.Make([]string{info.dedupName})
						m.orderOnlyStrings = append(m.orderOnlyStrings, info.dedupName)
					}
				}
			}
			return false
		})

	return &localBuildActions{buildDefs: phonys}
}

func (c *Context) writeLocalBuildActions(nw *ninjaWriter,
	defs *localBuildActions) error {

	// Write the local variable assignments.
	for _, v := range defs.variables {
		// A localVariable doesn't need the package names or config to
		// determine its name or value.
		name := v.fullName(nil)
		value, err := v.value(nil, nil)
		if err != nil {
			panic(err)
		}
		err = nw.Assign(name, value.Value(c.nameTracker))
		if err != nil {
			return err
		}
	}

	if len(defs.variables) > 0 {
		err := nw.BlankLine()
		if err != nil {
			return err
		}
	}

	// Write the local rules.
	for _, r := range defs.rules {
		// A localRule doesn't need the package names or config to determine
		// its name or definition.
		name := r.fullName(nil)
		def, err := r.def(nil)
		if err != nil {
			panic(err)
		}

		err = def.WriteTo(nw, name, c.nameTracker)
		if err != nil {
			return err
		}

		err = nw.BlankLine()
		if err != nil {
			return err
		}
	}

	// Write the build definitions.
	for _, buildDef := range defs.buildDefs {
		err := buildDef.WriteTo(nw, c.nameTracker)
		if err != nil {
			return err
		}

		if len(buildDef.Args) > 0 {
			err = nw.BlankLine()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func beforeInModuleList(a, b *moduleInfo, list moduleList) bool {
	found := false
	if a == b {
		return false
	}
	for _, l := range list {
		if l == a {
			found = true
		} else if l == b {
			return found
		}
	}

	missing := a
	if found {
		missing = b
	}
	panic(fmt.Errorf("element %v not found in list %v", missing, list))
}

type panicError struct {
	panic interface{}
	stack []byte
	in    string
}

func newPanicErrorf(panic interface{}, in string, a ...interface{}) error {
	buf := make([]byte, 4096)
	count := runtime.Stack(buf, false)
	return panicError{
		panic: panic,
		in:    fmt.Sprintf(in, a...),
		stack: buf[:count],
	}
}

func (p panicError) Error() string {
	return fmt.Sprintf("panic in %s\n%s\n%s\n", p.in, p.panic, p.stack)
}

func (p *panicError) addIn(in string) {
	p.in += " in " + in
}

func funcName(f interface{}) string {
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

// json representation of a dependency
type depJson struct {
	Name    string      `json:"name"`
	Variant string      `json:"variant"`
	TagType string      `json:"tag_type"`
	TagData interface{} `json:"tag_data"`
}

// json representation of a provider
type providerJson struct {
	Type   string      `json:"type"`
	Debug  string      `json:"debug"` // from GetDebugString on the provider data
	Fields interface{} `json:"fields"`
}

// interface for getting debug info from various data.
// TODO: Consider having this return a json object instead
type Debuggable interface {
	GetDebugString() string
}

// Convert a slice in a reflect.Value to a value suitable for outputting to json
func debugSlice(evaluator proptools.ConfigurableEvaluator, value reflect.Value) interface{} {
	size := value.Len()
	if size == 0 {
		return nil
	}
	result := make([]interface{}, size)
	for i := 0; i < size; i++ {
		result[i] = debugValue(evaluator, value.Index(i))
	}
	return result
}

// Convert a map in a reflect.Value to a value suitable for outputting to json
func debugMap(evaluator proptools.ConfigurableEvaluator, value reflect.Value) interface{} {
	if value.IsNil() {
		return nil
	}
	result := make(map[string]interface{})
	iter := value.MapRange()
	for iter.Next() {
		// In the (hopefully) rare case of a key collision (which will happen when multiple
		// go-typed keys have the same string representation, we'll just overwrite the last
		// value.
		result[debugKey(iter.Key())] = debugValue(evaluator, iter.Value())
	}
	return result
}

// Convert a value into a string, suitable for being a json map key.
func debugKey(value reflect.Value) string {
	return fmt.Sprintf("%v", value)
}

// Convert a single value (possibly a map or slice too) in a reflect.Value to a value suitable for outputting to json
func debugValue(evaluator proptools.ConfigurableEvaluator, value reflect.Value) interface{} {
	if proptools.IsConfigurable(value.Type()) {
		if evaluator == nil {
			return "<configurable value>"
		} else {
			if value.Kind() == reflect.Interface {
				value = value.Elem() // Get the underlying value in the interface.
			}
			if value.Kind() != reflect.Ptr {
				value = value.Addr() // The method needs a pointer receiver.
			}
			// value is now *proptools.Configurable[<something>]
			value = value.MethodByName("Get").Call([]reflect.Value{reflect.ValueOf(evaluator)})[0]
			// value is now an unaddressable proptools.ConfigurableOptional[<something>]
			ptrVal := reflect.New(value.Type())
			ptrVal.Elem().Set(value)
			// ptrVal is now *proptools.ConfigurableOptional[<something>]
			if ptrVal.MethodByName("IsEmpty").Call(nil)[0].Bool() {
				return nil
			}
			value = ptrVal.MethodByName("Get").Call(nil)[0]
		}
	}

	// Remember if we originally received a reflect.Interface.
	wasInterface := value.Kind() == reflect.Interface
	// Dereference pointers down to the real type
	for value.Kind() == reflect.Ptr || value.Kind() == reflect.Interface {
		// If it's nil, return nil
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}

	switch kind := value.Kind(); kind {
	case reflect.Bool:
		return value.Bool()
	case reflect.String:
		return value.String()
	case reflect.Int:
		return value.Int()
	case reflect.Uint:
		return value.Uint()
	case reflect.Slice:
		return debugSlice(evaluator, value)
	case reflect.Struct:
		// At least some of the private struct fields cause stack overflow here.  Do not include them until
		// we track the recursion down.
		if !value.CanInterface() {
			return nil
		}
		// If we originally received an interface, and there is a String() method, call that.
		// TODO: figure out why Path doesn't work correctly otherwise (in aconfigPropagatingDeclarationsInfo)
		if s, ok := value.Interface().(interface{ String() string }); wasInterface && ok {
			return s.String()
		}
		return debugStruct(evaluator, value)
	case reflect.Map:
		return debugMap(evaluator, value)
	default:
		// TODO: add cases as we find them.
		return fmt.Sprintf("debugValue(Kind=%v, wasInterface=%v)", kind, wasInterface)
	}

	return nil
}

// Convert an object in a reflect.Value to a value suitable for outputting to json
func debugStruct(evaluator proptools.ConfigurableEvaluator, value reflect.Value) interface{} {
	result := make(map[string]interface{})
	debugStructAppend(evaluator, value, &result)
	if len(result) == 0 {
		return nil
	}
	return result
}

// Convert an object to a value suiable for outputting to json
func debugStructAppend(evaluator proptools.ConfigurableEvaluator, value reflect.Value, result *map[string]interface{}) {
	for value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	if value.IsZero() {
		return
	}

	if value.Kind() != reflect.Struct {
		// TODO: could maybe support other types
		return
	}

	structType := value.Type()
	for i := 0; i < value.NumField(); i++ {
		v := debugValue(evaluator, value.Field(i))
		if v != nil {
			(*result)[structType.Field(i).Name] = v
		}
	}
}

func debugPropertyStruct(evaluator proptools.ConfigurableEvaluator, props interface{}, result *map[string]interface{}) {
	if props == nil {
		return
	}
	debugStructAppend(evaluator, reflect.ValueOf(props), result)
}

// Get the debug json for a single module. Returns thae data as
// flattened json text for easy concatenation by GenerateModuleDebugInfo.
func getModuleDebugJson(evaluator proptools.ConfigurableEvaluator, module *moduleInfo) []byte {
	info := struct {
		Name       string                 `json:"name"`
		SourceFile string                 `json:"source_file"`
		SourceLine int                    `json:"source_line"`
		Type       string                 `json:"type"`
		Variant    string                 `json:"variant"`
		Deps       []depJson              `json:"deps"`
		Providers  []providerJson         `json:"providers"`
		Debug      string                 `json:"debug"` // from GetDebugString on the module
		Properties map[string]interface{} `json:"properties"`
	}{
		Name:       module.Name(),
		SourceFile: module.pos.Filename,
		SourceLine: module.pos.Line,
		Type:       module.typeName,
		Variant:    module.variant.name,
		Deps: func() []depJson {
			result := make([]depJson, len(module.directDeps))
			for i, dep := range module.directDeps {
				result[i] = depJson{
					Name:    dep.module.Name(),
					Variant: dep.module.variant.name,
				}
				t := reflect.TypeOf(dep.tag)
				if t != nil {
					result[i].TagType = t.PkgPath() + "." + t.Name()
					result[i].TagData = debugStruct(nil, reflect.ValueOf(dep.tag))
				}
			}
			return result
		}(),
		Providers: func() []providerJson {
			result := make([]providerJson, 0, len(module.providers))
			for _, p := range module.providers {
				pj := providerJson{}
				include := false

				t := reflect.TypeOf(p)
				if t != nil {
					pj.Type = t.PkgPath() + "." + t.Name()
					include = true
				}

				if dbg, ok := p.(Debuggable); ok {
					pj.Debug = dbg.GetDebugString()
					if pj.Debug != "" {
						include = true
					}
				}

				if p != nil {
					pj.Fields = debugValue(nil, reflect.ValueOf(p))
					include = true
				}

				if include {
					result = append(result, pj)
				}
			}
			return result
		}(),
		Debug: func() string {
			if dbg, ok := module.logicModule.(Debuggable); ok {
				return dbg.GetDebugString()
			} else {
				return ""
			}
		}(),
		Properties: func() map[string]interface{} {
			result := make(map[string]interface{})
			for _, props := range module.properties {
				debugPropertyStruct(evaluator, props, &result)
			}
			return result
		}(),
	}
	buf, _ := json.Marshal(info)
	return buf
}

// InitializeModuleDebugInfoCollection sets up a channel and a receiver to write
// out/soong/soong-debug-info.json. Called if GENERATE_SOONG_DEBUG=true. Returns a function to be
// deferred until all modules have been processed.
func (this *Context) InitializeModuleDebugInfoCollection(filename string) func() {
	err := os.MkdirAll(filepath.Dir(filename), 0777)
	if err != nil {
		// We expect this to be writable
		panic(fmt.Sprintf("couldn't create directory for soong module debug file %s: %s", filepath.Dir(filename), err))
	}

	f, err := os.Create(filename)
	if err != nil {
		// We expect this to be writable
		panic(fmt.Sprintf("couldn't create soong module debug file %s: %s", filename, err))
	}

	this.moduleDebugDataChannel = make(chan []byte, 10)
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer f.Close()
		defer wg.Done()

		needComma := false
		f.WriteString("{\n\"modules\": [\n")

		for moduleData := range this.moduleDebugDataChannel {
			if needComma {
				f.WriteString(",\n")
			} else {
				needComma = true
			}
			f.Write(moduleData)
		}

		f.WriteString("\n]\n}")
	}()

	return func() {
		close(this.moduleDebugDataChannel)
		wg.Wait()
	}
}

// getModule returns a module with the given name and variations, from the root namespace.
// It will return nil if the module doesn't exist.
func (c *Context) getModule(moduleName string, variant []Variation) *moduleInfo {
	possibleDeps := c.moduleGroupFromName(
		moduleName,
		// Search in the root namespace, but soong's nameInterface implementation will allow
		// using the //path:module syntax to specify other namespaces.
		c.nameInterface.GetNamespace(newNamespaceContextFromFilename(".")),
	)
	if possibleDeps == nil {
		return nil
	}

	var vm variationMap
	for _, variation := range variant {
		vm.set(variation.Mutator, variation.Variation)
	}
	for _, module := range possibleDeps.modules {
		if module.variant.variations.equal(vm) {
			return module
		}
	}
	return nil
}

// RecordSandboxMetrics visits all the rules and records their sandboxing state in the metrics.
func (c *Context) RecordSandboxMetrics(config any) {
	sbConfig := config.(sandboxConfig)
	metrics := sbConfig.ActionSandboxMetrics()

	if !sbConfig.IsActionSandboxedBuild() || metrics == nil {
		return
	}
	// We would get incorrect results if we recorded these metrics on an incremental analysis,
	// as we wouldn't process all rules
	if c.GetIncrementalAnalysis() {
		return
	}

	for rule, def := range c.globalRules {
		name := c.nameTracker.Rule(rule)
		metrics.updateSandboxMetrics(name, def.Source, def.SandboxDisabled)
	}

	for module := range c.iterateAllVariants() {
		for _, r := range module.actionDefs.rules {
			name := r.fullName(nil)
			metrics.updateSandboxMetrics(name, r.def_.Source, r.def_.SandboxDisabled)
		}
	}

	for _, info := range c.singletonInfo {
		for _, r := range info.actionDefs.rules {
			name := r.fullName(nil)
			metrics.updateSandboxMetrics(name, r.def_.Source, r.def_.SandboxDisabled)
		}
	}
}

var fileHeaderTemplate = template.Must(template.New("fileHeader").Parse(
	`******************************************************************************
***            This file is generated and should not be edited             ***
******************************************************************************
{{if .Pkgs}}
This file contains variables, rules, and pools with name prefixes indicating
they were generated by the following Go packages:
{{range .Pkgs}}
    {{.PkgName}} [from Go package {{.PkgPath}}]{{end}}{{end}}

`))

var moduleHeaderTemplate = template.Must(template.New("moduleHeader").Parse(
	`# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #
Module:  {{.name}}
Variant: {{.variant}}
Type:    {{.typeName}}
Factory: {{.goFactory}}
Defined: {{.pos}}
`))

var singletonHeaderTemplate = template.Must(template.New("singletonHeader").Parse(
	`# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #
Singleton: {{.name}}
Factory:   {{.goFactory}}
`))

func JoinPath(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}
