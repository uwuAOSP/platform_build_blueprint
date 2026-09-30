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
	"fmt"
	"slices"

	"github.com/google/blueprint/pathtools"
	"github.com/google/blueprint/proptools"
)

type Singleton interface {
	GenerateBuildActions(SingletonContext)
	IncrementalSupported() bool
}

type SingletonContext interface {
	// Config returns the config object that was passed to Context.PrepareBuildActions.
	Config() interface{}

	// Name returns the name of the current singleton passed to Context.RegisterSingletonType
	Name() string

	// ModuleName returns the name of the given Module.  See BaseModuleContext.ModuleName for more information.
	ModuleName(module ModuleOrProxy) string

	// ModuleDir returns the directory of the given Module.  See BaseModuleContext.ModuleDir for more information.
	ModuleDir(module ModuleOrProxy) string

	// ModuleSubDir returns the unique subdirectory name of the given Module.  See ModuleContext.ModuleSubDir for
	// more information.
	ModuleSubDir(module ModuleOrProxy) string

	// ModuleType returns the type of the given Module.  See BaseModuleContext.ModuleType for more information.
	ModuleType(module ModuleOrProxy) string

	// BlueprintFile returns the path of the Blueprint file that defined the given module.
	BlueprintFile(module ModuleOrProxy) string

	// ModuleProvider returns the value, if any, for the provider for a module.  If the value for the
	// provider was not set it returns the zero value of the type of the provider, which means the
	// return value can always be type-asserted to the type of the provider.  The return value should
	// always be considered read-only.  It panics if called before the appropriate mutator or
	// GenerateBuildActions pass for the provider on the module.
	ModuleProvider(module ModuleOrProxy, provider AnyProviderKey) (any, bool)

	// SetSingletonProvider sets the value for a provider for the current singleton.
	// It panics if not called during the appropriate GenerateBuildActions pass for
	// the provider, if the provider is not registered as a singleton provider, if the value
	// is not of the appropriate type, or if the value has already been set. The value should not
	// be modified after being passed to SetSingletonProvider.
	SetSingletonProvider(provider AnyProviderKey, value any)

	// OtherSingletonProvider returns the value, if any, for the provider for a singleton. If the value for the
	// provider was not set it returns the zero value of the type of the provider, which means the
	// return value can always be type-asserted to the type of the provider. The return value should
	// always be considered read-only. It panics if called before the appropriate
	// GenerateBuildActions completes for the provider on the singleton, or if the provider is not
	// registered as a singleton provider.
	OtherSingletonProvider(singleton SingletonProxy, provider AnyProviderKey) (any, bool)

	// ModuleErrorf reports an error at the line number of the module type in the module definition.
	ModuleErrorf(module ModuleOrProxy, format string, args ...interface{})

	// Errorf reports an error at the specified position of the module definition file.
	Errorf(format string, args ...interface{})

	// OtherModulePropertyErrorf reports an error on the line number of the given property of the given module
	OtherModulePropertyErrorf(module ModuleOrProxy, property string, format string, args ...interface{})

	// Failed returns true if any errors have been reported.  In most cases the singleton can continue with generating
	// build rules after an error, allowing it to report additional errors in a single run, but in cases where the error
	// has prevented the singleton from creating necessary data it can return early when Failed returns true.
	Failed() bool

	// Variable creates a new ninja variable scoped to the singleton.  It can be referenced by calls to Rule and Build
	// in the same singleton.
	Variable(pctx PackageContext, name, value string)

	// Rule creates a new ninja rule scoped to the singleton.  It can be referenced by calls to Build in the same
	// singleton.
	Rule(pctx PackageContext, name string, params RuleParams, argNames ...string) Rule

	// Build creates a new ninja build statement.
	Build(pctx PackageContext, params BuildParams)

	// RequireNinjaVersion sets the generated ninja manifest to require at least the specified version of ninja.
	RequireNinjaVersion(major, minor, micro int)

	// SetOutDir sets the value of the top-level "builddir" Ninja variable
	// that controls where Ninja stores its build log files.  This value can be
	// set at most one time for a single build, later calls are ignored.
	SetOutDir(pctx PackageContext, value string)

	// AddSubninja adds a ninja file to include with subninja. This should likely
	// only ever be used inside bootstrap to handle glob rules.
	AddSubninja(file string)

	// Eval takes a string with embedded ninja variables, and returns a string
	// with all of the variables recursively expanded. Any variables references
	// are expanded in the scope of the PackageContext.
	Eval(pctx PackageContext, ninjaStr string) (string, error)

	// VisitAllModules calls visit for each defined variant of each module in an unspecified order.
	VisitAllModules(visit func(Module))

	// VisitAllModuleProxies calls visit for each defined variant of each module in an unspecified order.
	VisitAllModuleProxies(visit func(proxy ModuleProxy))

	// VisitAllModulesOrProxies calls visit for each defined variant of each module in an unspecified order,
	// passing a Module if the module did not call FreeModuleAfterGenerateBuildActions, or a ModuleProxy if
	// it did.
	VisitAllModulesOrProxies(visit func(ModuleOrProxy))

	// VisitAllModules calls pred for each defined variant of each module in an unspecified order, and if pred returns
	// true calls visit.
	VisitAllModulesIf(pred func(Module) bool, visit func(Module))

	// VisitDirectDeps calls visit for each direct dependency of the Module.  If there are
	// multiple direct dependencies on the same module visit will be called multiple times on
	// that module and OtherModuleDependencyTag will return a different tag for each.
	//
	// The Module passed to the visit function should not be retained outside of the visit
	// function, it may be invalidated by future mutators.
	VisitDirectDeps(module Module, visit func(Module))

	// VisitDirectDepsIf calls pred for each direct dependency of the Module, and if pred
	// returns true calls visit.  If there are multiple direct dependencies on the same module
	// pred and visit will be called multiple times on that module and OtherModuleDependencyTag
	// will return a different tag for each.
	//
	// The Module passed to the visit function should not be retained outside of the visit
	// function, it may be invalidated by future mutators.
	VisitDirectDepsIf(module Module, pred func(Module) bool, visit func(Module))

	// VisitDirectDepsProxies calls visit for each direct dependency of the ModuleProxy.  If there are
	// multiple direct dependencies on the same module visit will be called multiple times on
	// that module and OtherModuleDependencyTag will return a different tag for each.
	//
	// The ModuleProxy passed to the visit function should not be retained outside of the visit
	// function, it may be invalidated by future mutators.
	VisitDirectDepsProxies(module ModuleProxy, visit func(ModuleProxy))

	// VisitAllModuleVariants calls visit for each variant of the given module.
	VisitAllModuleVariants(module Module, visit func(Module))

	// VisitAllModuleVariantProxies calls visit for each variant of the given module.
	VisitAllModuleVariantProxies(module ModuleProxy, visit func(proxy ModuleProxy))

	VisitAllSingletons(visit func(singleton SingletonProxy))

	// PrimaryModule returns the first variant of the given module.  This can be used to perform
	// singleton actions that are only done once for all variants of a module.
	PrimaryModule(module Module) Module

	// PrimaryModuleProxy returns the proxy of the first variant of the given module.
	// This can be used to perform singleton actions that are only done once for
	// all variants of a module.
	PrimaryModuleProxy(module ModuleProxy) ModuleProxy

	// IsPrimaryModule returns if the given module is the first variant. This can be used to perform
	// singleton actions that are only done once for all variants of a module.
	IsPrimaryModule(module ModuleOrProxy) bool

	// IsFinalModule returns if the given module is the last variant. This can be used to perform
	// singleton actions that are only done once for all variants of a module.
	IsFinalModule(module ModuleOrProxy) bool

	// AddNinjaFileDeps adds dependencies on the specified files to the rule that creates the ninja manifest.  The
	// primary builder will be rerun whenever the specified files are modified.
	AddNinjaFileDeps(deps ...string)

	// GlobWithDeps returns a list of files and directories that match the
	// specified pattern but do not match any of the patterns in excludes.
	// Any directories will have a '/' suffix. It also adds efficient
	// dependencies to rerun the primary builder whenever a file matching
	// the pattern as added or removed, without rerunning if a file that
	// does not match the pattern is added to a searched directory.
	GlobWithDeps(pattern string, excludes []string) ([]string, error)

	// Fs returns a pathtools.Filesystem that can be used to interact with files.  Using the Filesystem interface allows
	// the singleton to be used in build system tests that run against a mock filesystem.
	Fs() pathtools.FileSystem

	// ModuleVariantsFromName returns the list of module variants named `name` in the same namespace as `referer`.
	// Allows generating build actions for `referer` based on the metadata for `name` deferred until the singleton context.
	ModuleVariantsFromName(referer ModuleProxy, name string) []ModuleProxy

	// HasMutatorFinished returns true if the given mutator has finished running.
	// It will panic if given an invalid mutator name.
	HasMutatorFinished(mutatorName string) bool

	// OtherModuleDependencyTag returns the dependency tag used to depend on a module, or nil if there is no dependency
	// on the module.  When called inside a Visit* method with current module being visited, and there are multiple
	// dependencies on the module being visited, it returns the dependency tag used for the current dependency.
	OtherModuleDependencyTag(module ModuleOrProxy) DependencyTag

	GetIncrementalAnalysis() bool
	GetIncrementalEnabled() bool

	// OtherModuleNamespace returns the namespace of the module.
	OtherModuleNamespace(module ModuleOrProxy) Namespace

	// GetModuleProxy returns a module with the given name and variations, from the root namespace.
	// It may return a nil ModuleProxy if the module doesn't exist.
	GetModuleProxy(moduleName string, variant []Variation) ModuleProxy
}

type SingletonProxy struct {
	singleton *singletonInfo
}

var _ SingletonContext = (*singletonContext)(nil)

func (s SingletonProxy) IncrementalInfo() *IncrementalMetricsInfo {
	return &IncrementalMetricsInfo{
		IncrementalRestored:  s.singleton.incrementalRestored,
		IncrementalSupported: s.singleton.incrementalSupported,
	}
}

type singletonContext struct {
	singleton *singletonInfo
	context   *Context
	config    interface{}
	scope     *localScope
	globals   *liveTracker

	subninjas     []string
	ninjaFileDeps []string
	errs          []error

	visitingDep depInfo

	actionDefs localBuildActions

	depProviders                    map[int]bool
	moduleDependencyBloom           []uint64
	moduleProviderDependencyBitsets map[int][]uint64
	moduleSetHash                   string
	singletonSetHash                string
	singletonProviderDependencies   []SingletonProviderDependency
}

func (s *singletonContext) recordModule(module *moduleInfo) {
	if module == nil || !s.singleton.incrementalSupported || !s.context.incrementalEnabled {
		return
	}
	if s.moduleDependencyBloom == nil {
		words := s.context.incrementalModuleBloomWords
		if words == 0 {
			words = 1
		}
		s.moduleDependencyBloom = make([]uint64, words)
	}
	key, ok := s.context.incrementalModuleCacheKeyByInfo[module]
	if !ok {
		key = module.moduleCacheKey()
	}
	moduleDependencyBloomAdd(s.moduleDependencyBloom, key)
}

func (s *singletonContext) recordModuleSet() {
	if s.singleton.incrementalSupported && s.context.incrementalEnabled {
		s.moduleSetHash = s.context.incrementalModuleSetHash
	}
}

func (s *singletonContext) recordSingletonSet() {
	if !s.singleton.incrementalSupported || !s.context.incrementalEnabled {
		return
	}
	s.singletonSetHash = s.context.finishedSingletonSetHash()
}

func (s *singletonContext) Config() interface{} {
	return s.config
}

func (s *singletonContext) Name() string {
	return s.singleton.name
}

func (s *singletonContext) ModuleName(logicModule ModuleOrProxy) string {
	s.recordModuleSet()
	return s.context.ModuleName(logicModule)
}

func (s *singletonContext) ModuleDir(logicModule ModuleOrProxy) string {
	s.recordModuleSet()
	return s.context.ModuleDir(logicModule)
}

func (s *singletonContext) ModuleSubDir(logicModule ModuleOrProxy) string {
	s.recordModuleSet()
	return s.context.ModuleSubDir(logicModule)
}

func (s *singletonContext) ModuleType(logicModule ModuleOrProxy) string {
	s.recordModuleSet()
	return s.context.ModuleType(logicModule)
}

func (s *singletonContext) ModuleProvider(logicModule ModuleOrProxy, provider AnyProviderKey) (any, bool) {
	providerId := provider.provider().id
	s.depProviders[providerId] = true
	if s.singleton.incrementalSupported && s.context.incrementalEnabled {
		s.recordModuleSet()
		if s.moduleProviderDependencyBitsets == nil {
			s.moduleProviderDependencyBitsets = make(map[int][]uint64)
		}
		bitset := s.moduleProviderDependencyBitsets[providerId]
		if bitset == nil {
			words := s.context.incrementalModuleBitsetWords
			if words == 0 {
				words = 1
			}
			bitset = make([]uint64, words)
			s.moduleProviderDependencyBitsets[providerId] = bitset
		}
		if moduleIndex, ok := s.context.incrementalModuleIndexByInfo[logicModule.info()]; ok {
			bitset[moduleIndex/64] |= uint64(1) << (moduleIndex % 64)
		}
	}
	return s.context.ModuleProvider(logicModule, provider)
}

func (s *singletonContext) cachedModuleProviderDependencyBitsets() []ModuleProviderDependencyBitset {
	if len(s.moduleProviderDependencyBitsets) == 0 {
		return nil
	}
	result := make([]ModuleProviderDependencyBitset, 0, len(s.moduleProviderDependencyBitsets))
	for providerId, modules := range s.moduleProviderDependencyBitsets {
		result = append(result, ModuleProviderDependencyBitset{ProviderId: providerId, Modules: modules})
	}
	slices.SortFunc(result, func(a, b ModuleProviderDependencyBitset) int {
		return a.ProviderId - b.ProviderId
	})
	return result
}

func (s *singletonContext) SetSingletonProvider(provider AnyProviderKey, value any) {
	s.context.setSingletonProvider(s.singleton, provider.provider(), value)
}

func (s *singletonContext) OtherSingletonProvider(singleton SingletonProxy, provider AnyProviderKey) (any, bool) {
	providerKey := provider.provider()
	s.depProviders[providerKey.id] = true
	value, found := s.context.singletonProvider(singleton.singleton, providerKey)
	hash := proptools.ZeroHash
	if providerKey.id < len(singleton.singleton.providerInitialValueHashes) {
		hash = singleton.singleton.providerInitialValueHashes[providerKey.id]
	}
	s.singletonProviderDependencies = append(s.singletonProviderDependencies, SingletonProviderDependency{
		SingletonName: singleton.singleton.name,
		ProviderId:    providerKey.id,
		Hash:          hash,
	})
	return value, found
}

func (s *singletonContext) BlueprintFile(logicModule ModuleOrProxy) string {
	s.recordModuleSet()
	return s.context.BlueprintFile(logicModule)
}

func (s *singletonContext) error(err error) {
	if err != nil {
		s.errs = append(s.errs, err)
	}
}

func (s *singletonContext) ModuleErrorf(logicModule ModuleOrProxy, format string,
	args ...interface{}) {

	s.recordModule(logicModule.info())
	s.error(s.context.ModuleErrorf(logicModule, format, args...))
}

func (s *singletonContext) Errorf(format string, args ...interface{}) {
	// TODO: Make this not result in the error being printed as "internal error"
	s.error(fmt.Errorf(format, args...))
}

func (s *singletonContext) OtherModulePropertyErrorf(logicModule ModuleOrProxy, property string, format string,
	args ...interface{}) {

	s.recordModule(logicModule.info())
	s.error(s.context.PropertyErrorf(logicModule, property, format, args...))
}

func (s *singletonContext) Failed() bool {
	return len(s.errs) > 0
}

func (s *singletonContext) Variable(pctx PackageContext, name, value string) {
	s.scope.ReparentTo(pctx)

	v, err := s.scope.AddLocalVariable(name, value)
	if err != nil {
		panic(err)
	}

	s.actionDefs.variables = append(s.actionDefs.variables, v)
}

func (s *singletonContext) Rule(pctx PackageContext, name string,
	params RuleParams, argNames ...string) Rule {

	s.scope.ReparentTo(pctx)

	config := s.Config()
	r, err := s.scope.AddLocalRule(config, name, &params, argNames...)
	if err != nil {
		panic(err)
	}

	s.actionDefs.rules = append(s.actionDefs.rules, r)

	return r
}

func (s *singletonContext) Build(pctx PackageContext, params BuildParams) {
	s.scope.ReparentTo(pctx)

	def, err := parseBuildParams(s.scope, &params, map[string]string{
		"module_name": s.singleton.name,
		"module_type": "singleton",
	})
	if err != nil {
		panic(err)
	}

	s.actionDefs.buildDefs = append(s.actionDefs.buildDefs, def)
}

func (s *singletonContext) Eval(pctx PackageContext, str string) (string, error) {
	s.scope.ReparentTo(pctx)

	ninjaStr, err := parseNinjaString(s.scope, str)
	if err != nil {
		return "", err
	}

	err = s.globals.addNinjaStringDeps(ninjaStr)
	if err != nil {
		return "", err
	}

	return s.globals.Eval(ninjaStr)
}

func (s *singletonContext) RequireNinjaVersion(major, minor, micro int) {
	s.context.requireNinjaVersion(major, minor, micro)
}

func (s *singletonContext) SetOutDir(pctx PackageContext, value string) {
	s.scope.ReparentTo(pctx)

	ninjaValue, err := parseNinjaString(s.scope, value)
	if err != nil {
		panic(err)
	}

	s.context.setOutDir(ninjaValue)
}

func (s *singletonContext) AddSubninja(file string) {
	s.subninjas = append(s.subninjas, file)
}

func (s *singletonContext) VisitAllModules(visit func(Module)) {
	s.recordModuleSet()
	s.context.VisitAllModules(func(module Module) {
		s.recordModule(module.info())
		visit(module)
	})
}

func (s *singletonContext) VisitAllModulesOrProxies(visit func(ModuleOrProxy)) {
	s.recordModuleSet()
	s.context.VisitAllModulesOrProxies(func(module ModuleOrProxy) {
		if _, isModule := module.(Module); isModule {
			s.recordModule(module.info())
		}
		visit(module)
	})
}

func (s *singletonContext) VisitAllModuleProxies(visit func(proxy ModuleProxy)) {
	s.recordModuleSet()
	s.context.VisitAllModulesProxies(visit)
}

func (s *singletonContext) VisitAllModulesIf(pred func(Module) bool,
	visit func(Module)) {

	s.recordModuleSet()
	s.context.VisitAllModulesIf(func(module Module) bool {
		s.recordModule(module.info())
		return pred(module)
	}, visit)
}

func (s *singletonContext) VisitDirectDeps(module Module, visit func(Module)) {
	s.recordModule(module.info())
	topModule := module.info()

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDirectDeps(%s, %s) for dependency %s",
				topModule, funcName(visit), s.visitingDep.module))
		}
	}()

	for _, dep := range topModule.directDeps {
		s.recordModule(dep.module)
		s.visitingDep = dep
		if dep.module.logicModule == nil {
			panic(fmt.Errorf("VisitDirectDeps visited module %s that called FreeAfterGenerateBuildActions()", dep.module))
		}
		visit(dep.module.logicModule)
	}
}

func (s *singletonContext) VisitDirectDepsIf(module Module, pred func(Module) bool, visit func(Module)) {
	s.recordModule(module.info())
	topModule := module.info()

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDirectDepsIf(%s, %s, %s) for dependency %s",
				topModule, funcName(pred), funcName(visit), s.visitingDep.module))
		}
	}()

	for _, dep := range topModule.directDeps {
		s.recordModule(dep.module)
		s.visitingDep = dep
		if dep.module.logicModule == nil {
			panic(fmt.Errorf("VisitDirectDepsIf visited module %s that called FreeAfterGenerateBuildActions()", dep.module))
		}
		if pred(dep.module.logicModule) {
			visit(dep.module.logicModule)
		}
	}
}

func (s *singletonContext) VisitDirectDepsProxies(module ModuleProxy, visit func(ModuleProxy)) {
	s.recordModule(module.info())
	topModule := module.info()

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDirectDepsProxies(%s, %s) for dependency %s",
				topModule, funcName(visit), s.visitingDep.module))
		}
	}()

	for _, dep := range topModule.directDeps {
		s.visitingDep = dep
		visit(ModuleProxy{dep.module})
	}
}

func (s *singletonContext) OtherModuleDependencyTag(module ModuleOrProxy) DependencyTag {
	s.recordModuleSet()
	if s.visitingDep.module == module.info() {
		return s.visitingDep.tag
	}
	return nil
}

func (s *singletonContext) PrimaryModule(module Module) Module {
	s.recordModuleSet()
	return s.context.PrimaryModule(module)
}

func (s *singletonContext) PrimaryModuleProxy(module ModuleProxy) ModuleProxy {
	s.recordModuleSet()
	return ModuleProxy{s.context.primaryModule(module.info())}
}

func (s *singletonContext) IsPrimaryModule(module ModuleOrProxy) bool {
	s.recordModuleSet()
	return s.context.IsPrimaryModule(module)
}

func (s *singletonContext) IsFinalModule(module ModuleOrProxy) bool {
	s.recordModuleSet()
	return s.context.IsFinalModule(module)
}

func (s *singletonContext) VisitAllModuleVariants(module Module, visit func(Module)) {
	s.recordModule(module.info())
	s.recordModuleSet()
	s.context.VisitAllModuleVariants(module, func(variant Module) {
		s.recordModule(variant.info())
		visit(variant)
	})
}

func (s *singletonContext) VisitAllModuleVariantProxies(module ModuleProxy, visit func(proxy ModuleProxy)) {
	s.recordModuleSet()
	s.context.VisitAllModuleVariantProxies(module, visit)
}

func (s *singletonContext) VisitAllSingletons(visit func(singleton SingletonProxy)) {
	if s.singleton.parallel {
		panic(fmt.Sprintf("VisitAllSingletons not allowed in parallel singletons %s", s.Name()))
	}
	s.recordSingletonSet()
	s.context.VisitAllSingletons(visit)
}

func (s *singletonContext) AddNinjaFileDeps(deps ...string) {
	s.ninjaFileDeps = append(s.ninjaFileDeps, deps...)
}

func (s *singletonContext) GlobWithDeps(pattern string, excludes []string) ([]string, error) {
	result, err := s.context.glob(pattern, excludes)
	if err == nil && s.context.incrementalEnabled && s.singleton.incrementalSupported {
		hash, err := proptools.CalculateHash(stringList(result))
		if err != nil {
			panic(newPanicErrorf(err, "failed to calculate hash for glob result: %s", s.singleton.name))
		}
		s.singleton.globCache = append(s.singleton.globCache, globResultCache{
			Pattern:  pattern,
			Excludes: excludes,
			Result:   hash,
		})
	}
	return result, err
}

func (s *singletonContext) Fs() pathtools.FileSystem {
	return s.context.fs
}

func (s *singletonContext) ModuleVariantsFromName(referer ModuleProxy, name string) []ModuleProxy {
	c := s.context
	s.recordModuleSet()

	refererInfo := referer.info()
	if refererInfo == nil {
		s.ModuleErrorf(referer, "could not find module %q", referer.Name())
		return nil
	}

	moduleGroup, exists := c.nameInterface.ModuleFromName(name, refererInfo.namespace())
	if !exists {
		return nil
	}
	result := make([]ModuleProxy, 0, len(moduleGroup.modules))
	for _, moduleInfo := range moduleGroup.modules {
		result = append(result, ModuleProxy{moduleInfo})
	}
	return result
}

func (s *singletonContext) HasMutatorFinished(mutatorName string) bool {
	return s.context.HasMutatorFinished(mutatorName)
}

func (s *singletonContext) GetIncrementalAnalysis() bool {
	return s.context.GetIncrementalAnalysis()
}

func (s *singletonContext) GetIncrementalEnabled() bool {
	return s.context.GetIncrementalEnabled()
}

func (s *singletonContext) OtherModuleNamespace(module ModuleOrProxy) Namespace {
	s.recordModuleSet()
	return s.context.nameInterface.GetNamespace(newNamespaceContext(module.info()))
}

func (s *singletonContext) GetModuleProxy(moduleName string, variant []Variation) ModuleProxy {
	s.recordModuleSet()
	module := s.context.getModule(moduleName, variant)
	return ModuleProxy{module}
}
