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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/blueprint/gobtools"
	"github.com/google/blueprint/proptools"
	"github.com/google/blueprint/uniquelist"
)

//go:generate go run ./gobtools/codegen

// @auto-generate: gob
type ModuleBuildActionCacheInput struct {
	PropertiesHash    proptools.Hash
	DepProviderHashes []proptools.Hash
}

// ModuleUsesIncrementalWalkDeps is embedded into Modules to mark them
// as using WalkDepsProxy in methods that can be cached for incremental
// analysis.  It forces incremental analysis to compare the hashes of
// all transitive dependencies.  This causes more cache misses, so should
// be avoided in favor of propagating information into the providers of
// the direct dependencies.
type ModuleUsesIncrementalWalkDeps struct{}

func (ModuleUsesIncrementalWalkDeps) moduleUsesIncrementalWalkDeps() {}

var _ moduleUsesIncrementalWalkDeps = ModuleUsesIncrementalWalkDeps{}

type moduleUsesIncrementalWalkDeps interface {
	moduleUsesIncrementalWalkDeps()
}

// restoreModuleBuildActions restores a module from the cache if its inputs
// have not changed.  It returns true if the module was restored.
func (m *moduleInfo) restoreModuleBuildActions(ctx *Context) bool {
	if !ctx.GetIncrementalEnabled() {
		// Don't restore, incremental analysis is not enabled.
		return false
	}
	if _, optedOut := m.logicModule.(ModuleActionCacheOptOut); optedOut {
		m.incrementalRestoreReason = "module_action_cache_opt_out"
		return false
	}

	// Compute the hashes of the input data.
	hash, err := proptools.CalculateHashReflection(m.properties)
	if err != nil {
		panic(newPanicErrorf(err, "failed to calculate properties hash"))
	}

	// If the module is marked as using WalkDeps then collect hashes of transitive
	// dependencies into the input hash.  Otherwise, only collect the hashes of the
	// direct dependencies, which will reduce the cache miss rate.
	_, hashTransitiveDeps := m.logicModule.(moduleUsesIncrementalWalkDeps)

	cacheInput := new(ModuleBuildActionCacheInput)
	cacheInput.PropertiesHash = hash
	cacheInput.DepProviderHashes = make([]proptools.Hash, 0, len(m.directDeps))
	for _, dep := range m.directDeps {
		if hashTransitiveDeps {
			cacheInput.DepProviderHashes = append(cacheInput.DepProviderHashes, dep.module.transitiveProvidersHash)
		} else {
			cacheInput.DepProviderHashes = append(cacheInput.DepProviderHashes, dep.module.providersHash)
		}
	}
	hash, err = proptools.CalculateHash(cacheInput)
	if err != nil {
		panic(newPanicErrorf(err, "failed to calculate cache input hash"))
	}

	// Store the cache key and hash for use when writing this module to the cache.
	m.buildActionCacheKey = &DataCacheKey{
		Id: m.moduleCacheKey(),
	}
	m.buildActionInputHash = hash

	if ctx.incrementalDebugFile != "" {
		defer func() {
			if !ctx.incrementalDebugMissesOnly || !m.incrementalRestored {
				m.incrementalDebugInfo = m.incrementalDebugData(cacheInput)
			}
		}()
	}

	if !ctx.GetIncrementalAnalysis() {
		// Don't restore, incremental analysis is globally disabled (for example
		// because no cache is present, TARGET_PRODUCT was changed, or soong_build
		// was rebuilt).
		m.incrementalRestoreReason = "incremental_analysis_disabled"
		return false
	}

	// Read the module metadata from the cache.
	data, err := ctx.keyValueStoreCache.readModuleBuildAction(ctx.EncContext, m.buildActionCacheKey)
	if err != nil {
		panic(err)
	}
	if data == nil {
		// Don't restore, no cache entry or the input hash doesn't match.
		m.incrementalRestoreReason = "cache_entry_missing"
		return false
	}
	if m.buildActionInputHash != data.InputHash {
		m.cachePreviousProviderHashes(data)
		m.incrementalRestoreReason = "module_or_dependency_inputs_changed"
		return false
	}
	for _, glob := range data.GlobCache {
		result, err := ctx.glob(glob.Pattern, glob.Excludes)
		if err != nil {
			panic(newPanicErrorf(err, "failed to glob for cached module: %s %s %v", m.Name(), glob.Pattern, glob.Excludes))
		}
		hash, err := proptools.CalculateHash(stringList(result))
		if err != nil {
			panic(newPanicErrorf(err, "failed to calculate hash for cached glob result: %s", m.Name()))
		}
		if hash != glob.Result {
			// Don't restore, a glob result has changed.
			m.cachePreviousProviderHashes(data)
			m.incrementalRestoreReason = "glob_result_changed: " + glob.Pattern
			return false
		}
	}
	m.globCache = data.GlobCache

	// Restore the module from the cache.
	if m.providerInitialValueHashes == nil {
		m.providerInitialValueHashes = make([]proptools.Hash, len(providerRegistry))
	}

	m.hasUnrestoredProvider = make([]bool, len(providerRegistry))
	m.incrementalRestored = true
	m.incrementalRestoreReason = "cache_hit"

	for _, provider := range data.ProviderHashes {
		m.providerInitialValueHashes[provider.Id.id] = provider.Hash
		m.hasUnrestoredProvider[provider.Id.id] = true
	}

	m.orderOnlyStrings = data.OrderOnlyStrings
	for _, str := range data.OrderOnlyStrings {
		if !strings.HasPrefix(str, "dedup-") {
			continue
		}
		orderOnlyStrings, ok := ctx.orderOnlyStringsCache[str]
		if !ok {
			panic(fmt.Errorf("no cached value found for order only dep: %s", str))
		}
		key := uniquelist.Make(orderOnlyStrings)
		if info, loaded := ctx.orderOnlyStrings.LoadOrStore(key, &orderOnlyStringsInfo{
			dedup:       true,
			incremental: true,
		}); loaded {
			for {
				cpy := *info
				cpy.dedup = true
				cpy.incremental = true
				if ctx.orderOnlyStrings.CompareAndSwap(key, info, &cpy) {
					break
				}
				if info, loaded = ctx.orderOnlyStrings.Load(key); !loaded {
					// This shouldn't happen
					panic("order only string was removed unexpectedly")
				}
			}
		}
	}

	return true
}

func (m *moduleInfo) cachePreviousProviderHashes(data *ModuleActionCachedData) {
	m.cachedProviderInitialValueHashes = make([]proptools.Hash, len(providerRegistry))
	for _, provider := range data.ProviderHashes {
		if provider.Id != nil && provider.Id.id >= 0 && provider.Id.id < len(m.cachedProviderInitialValueHashes) {
			m.cachedProviderInitialValueHashes[provider.Id.id] = provider.Hash
		}
	}
	m.hasCachedProviderInitialValueHashes = true
}

// @auto-generate: gob
type hashList []proptools.Hash

// @auto-generate: gob
type stringList []string

// calculateProviderHash stores the hash of the providers of this module into
// moduleInfo.providersHash, and the hash of the providers of this module and
// all transitive dependencies into moduleInfo.transitiveProvidersHash.
func (m *moduleInfo) calculateProviderHash() {
	var err error
	m.providersHash, err = proptools.CalculateHash(hashList(m.providerInitialValueHashes))
	if err != nil {
		panic(newPanicErrorf(err, "failed to calculate providers hash"))
	}

	transitiveHashes := make([]proptools.Hash, 0, len(m.directDeps)+1)
	transitiveHashes = append(transitiveHashes, m.providersHash)
	for _, dep := range m.directDeps {
		transitiveHashes = append(transitiveHashes, dep.module.transitiveProvidersHash)
	}
	m.transitiveProvidersHash, err = proptools.CalculateHash(hashList(transitiveHashes))
	if err != nil {
		panic(newPanicErrorf(err, "failed to calculate transitive providers hash"))
	}
}

func (m *moduleInfo) cacheModuleBuildActions(ctx gobtools.EncContext, buildActionsCache *KeyValueStoreCache) {
	if _, optedOut := m.logicModule.(ModuleActionCacheOptOut); optedOut {
		return
	}

	var providerHashes []ProviderHash

	for i, p := range m.providers {
		if p != nil && providerRegistry[i].mutator == "" {
			err := buildActionsCache.writeProvider(ctx, m.providerInitialValueHashes[i],
				CachedProvider{
					Id:    providerRegistry[i],
					Value: p,
				})
			if err != nil {
				panic(err)
			}
			providerHashes = append(providerHashes,
				ProviderHash{
					Id:   providerRegistry[i],
					Hash: m.providerInitialValueHashes[i],
				})
		}
	}

	buildActionData := ModuleActionCachedData{
		InputHash:        m.buildActionInputHash,
		ProviderHashes:   providerHashes,
		OrderOnlyStrings: m.orderOnlyStrings,
		GlobCache:        m.globCache,
	}

	err := buildActionsCache.writeModuleBuildAction(ctx, m.buildActionCacheKey, &buildActionData)
	if err != nil {
		panic(err)
	}
}

type depProviders struct {
	Name      string   `json:"dep_name"`
	Type      string   `json:"dep_type"`
	Variant   string   `json:"dep_variant"`
	Providers []string `json:"dep_provider_hash"`
}

func (m *moduleInfo) incrementalDebugData(inputHash *ModuleBuildActionCacheInput) []byte {
	info := struct {
		Name          string         `json:"name"`
		CacheKey      string         `json:"cache_key"`
		Type          string         `json:"type"`
		Variant       string         `json:"variant"`
		PropHash      proptools.Hash `json:"properties_hash"`
		RestoreReason string         `json:"restore_reason"`
		Providers     []depProviders `json:"providers"`
	}{
		Name:          m.logicModule.Name(),
		CacheKey:      m.moduleCacheKey(),
		Type:          m.typeName,
		Variant:       m.variant.name,
		PropHash:      inputHash.PropertiesHash,
		RestoreReason: m.incrementalRestoreReason,
		Providers: func() []depProviders {
			result := make([]depProviders, 0, len(m.directDeps))
			for _, d := range m.directDeps {
				dep := d.module
				dp := depProviders{
					Name:    dep.Name(),
					Type:    dep.typeName,
					Variant: dep.variant.name,
				}
				for _, p := range providerRegistry {
					if dep.providerInitialValueHashes[p.id] == proptools.ZeroHash {
						continue
					}
					dp.Providers = append(dp.Providers,
						fmt.Sprintf("%s:%x", p.typ, dep.providerInitialValueHashes[p.id]))
				}
				result = append(result, dp)
			}
			return result
		}(),
	}
	buf, _ := json.Marshal(info)
	return buf
}
