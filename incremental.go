// Copyright 2024 Google Inc. All rights reserved.
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
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/akrylysov/pogreb"

	"github.com/google/blueprint/dbtools"
	"github.com/google/blueprint/gobtools"
	"github.com/google/blueprint/pool"
	"github.com/google/blueprint/proptools"
	"github.com/google/blueprint/syncmap"
)

//go:generate go run ./gobtools/codegen

const moduleActionsDbName = "module_actions.db"
const singletonActionsDbName = "singleton_actions.db"
const providersDbName = "providers.db"
const referencesDbName = "references.db"
const ninjaDbName = "ninja.db"
const sourceDeclarationSnapshotCacheKey = "\x00blueprint:source-declaration-snapshot:v3"
const sourceDeclarationFileSnapshotCacheKeyPrefix = "\x00blueprint:source-declaration-file:v3\x00"
const mutatorModuleStateCacheKeyPrefix = "\x00blueprint:mutator-module-state:v1\x00"
const mutatorStateCacheVersionKeyPrefix = "\x00blueprint:mutator-state-cache-version:v1\x00"
const dependencyResolutionCacheKeyPrefix = "\x00blueprint:dependency-resolution:v2\x00"
const dependencyResolutionCacheVersion = 2

var IncrementalInfoDbNames = []string{
	moduleActionsDbName,
	singletonActionsDbName,
	providersDbName,
	referencesDbName,
	ninjaDbName,
}

// @auto-generate: gob
type DataCacheKey struct {
	Id string
}

func (k *DataCacheKey) bytes() []byte {
	return unsafe.Slice(unsafe.StringData(k.Id), len(k.Id))
}

type ProviderCacheKey struct {
	DataCacheKey
	ProviderId int
}

func (k *ProviderCacheKey) bytes() []byte {
	buf := make([]byte, len(k.DataCacheKey.Id), len(k.DataCacheKey.Id)+8)
	copy(buf, k.DataCacheKey.bytes())
	buf = binary.LittleEndian.AppendUint64(buf, uint64(k.ProviderId))
	return buf
}

// @auto-generate: gob
type CachedProvider struct {
	Id    *providerKey
	Value any
}

// @auto-generate: gob
type ProviderHash struct {
	Id   *providerKey
	Hash proptools.Hash
}

// @auto-generate: gob
type ModuleActionCachedData struct {
	InputHash        proptools.Hash
	ProviderHashes   []ProviderHash
	OrderOnlyStrings []string
	GlobCache        []globResultCache
}

// @auto-generate: gob
type MutatorModuleStateCachedData struct {
	Version int
	Valid   bool
	State   string
}

// @auto-generate: gob
type SourceModuleDeclaration struct {
	Key                   string
	PropertiesHash        proptools.Hash
	ModuleCacheKeys       []string
	DependentDeclarations []string
	GlobCache             []globResultCache
}

// @auto-generate: gob
type SourceDeclarationFileIndex struct {
	Version       int
	Files         []string
	GraphComplete bool
}

// @auto-generate: gob
type SourceModuleDeclarationFileSnapshot struct {
	Version int
	Modules []SourceModuleDeclaration
}

const sourceDeclarationSnapshotVersion = 3
const mutatorModuleStateCacheVersion = 1

func (c *Context) mutatorModuleStateCacheKey(mutator string, module *moduleInfo) []byte {
	if module == nil || module.group == nil || module.sourceDeclarationKey == "" {
		return nil
	}
	sourceHash, ok := c.sourceModuleDeclarations[module.sourceDeclarationKey]
	if !ok {
		return nil
	}
	return []byte(fmt.Sprintf("%s%s\x00%s\x00%s\x00%s\x00%s\x00%x",
		mutatorModuleStateCacheKeyPrefix, mutator, module.sourceDeclarationKey,
		module.typeName, module.group.name, module.variant.name, sourceHash))
}

func mutatorStateCacheVersionKey(mutator string) []byte {
	return []byte(mutatorStateCacheVersionKeyPrefix + mutator)
}

func (c *Context) dependencyResolutionCacheKey(mutator, version string, module *moduleInfo) []byte {
	if module == nil || module.sourceDeclarationKey == "" {
		return nil
	}
	sourceHash, ok := c.sourceModuleDeclarations[module.sourceDeclarationKey]
	if !ok {
		return nil
	}
	return []byte(fmt.Sprintf("%s%s\x00%s\x00%s\x00%x",
		dependencyResolutionCacheKeyPrefix, mutator, version, module.moduleCacheKey(), sourceHash))
}

func sourceModuleDeclarationKey(relBlueprintsFile, typeName, name string) string {
	return relBlueprintsFile + "\x00" + typeName + "\x00" + name
}

func sourceDeclarationFileSnapshotKey(file string) []byte {
	return []byte(sourceDeclarationFileSnapshotCacheKeyPrefix + file)
}

func (c *Context) sourceDeclarationFiles() map[string][]SourceModuleDeclaration {
	files := make(map[string][]SourceModuleDeclaration)
	for key, hash := range c.sourceModuleDeclarations {
		file := strings.SplitN(key, "\x00", 2)[0]
		declaration := SourceModuleDeclaration{Key: key, PropertiesHash: hash}
		declaration.ModuleCacheKeys = slices.Clone(c.sourceDeclarationModuleCacheKeys[key])
		declaration.DependentDeclarations = slices.Clone(c.sourceDeclarationDependents[key])
		declaration.GlobCache = slices.Clone(c.sourceDeclarationGlobs[key])
		files[file] = append(files[file], declaration)
	}
	for file := range files {
		sort.Slice(files[file], func(i, j int) bool { return files[file][i].Key < files[file][j].Key })
		for moduleIndex := range files[file] {
			globs := files[file][moduleIndex].GlobCache
			sort.Slice(globs, func(i, j int) bool {
				if globs[i].Pattern != globs[j].Pattern {
					return globs[i].Pattern < globs[j].Pattern
				}
				return strings.Join(globs[i].Excludes, "\x00") < strings.Join(globs[j].Excludes, "\x00")
			})
			files[file][moduleIndex].GlobCache = globs
		}
	}
	return files
}

func (c *Context) compareSourceModuleDeclarationSnapshot() error {
	index, err := c.keyValueStoreCache.readSourceDeclarationFileIndex(c.EncContext)
	if err != nil {
		return err
	}
	c.changedSourceModuleDeclarations = nil
	c.addedSourceModuleDeclarations = nil
	c.removedSourceModuleDeclarations = nil
	c.affectedSourceModuleDeclarations = nil
	c.affectedModuleCacheKeys = nil
	c.previousSourceDeclarationDependents = make(map[string][]string)
	previousModuleCacheKeys := make(map[string][]string)
	c.sourceDeclarationFileSnapshots = make(map[string]*SourceModuleDeclarationFileSnapshot)
	c.sourceDeclarationFileIndex = index
	c.sourceDeclarationSnapshotValid = index != nil && index.Version == sourceDeclarationSnapshotVersion
	c.sourceDeclarationGraphValid = c.sourceDeclarationSnapshotValid && index.GraphComplete
	if !c.sourceDeclarationSnapshotValid {
		for key := range c.sourceModuleDeclarations {
			c.changedSourceModuleDeclarations = append(c.changedSourceModuleDeclarations, key)
		}
		sort.Strings(c.changedSourceModuleDeclarations)
		if c.mutatorVisitStatsEnabled {
			indexVersion := 0
			if index != nil {
				indexVersion = index.Version
			}
			fmt.Fprintf(os.Stderr, "soong: source declaration snapshot: valid=false graph-complete=%t index-version=%d expected-version=%d current-declarations=%d\n",
				index != nil && index.GraphComplete, indexVersion, sourceDeclarationSnapshotVersion,
				len(c.changedSourceModuleDeclarations))
		}
		return nil
	}

	previousFiles := make(map[string]bool, len(index.Files))
	for _, file := range index.Files {
		previousFiles[file] = true
	}
	currentFiles := c.sourceDeclarationFiles()
	currentGlobHashes := make(map[globKey]proptools.Hash)
	invalidSnapshotFiles := 0
	for file, current := range currentFiles {
		delete(previousFiles, file)
		previous, err := c.keyValueStoreCache.readSourceModuleDeclarationFile(c.EncContext, file)
		if err != nil {
			return err
		}
		c.sourceDeclarationFileSnapshots[file] = previous
		previousHashes := make(map[string]proptools.Hash)
		previousDeclarations := make(map[string]SourceModuleDeclaration)
		if previous != nil && previous.Version == sourceDeclarationSnapshotVersion {
			for _, module := range previous.Modules {
				previousHashes[module.Key] = module.PropertiesHash
				previousDeclarations[module.Key] = module
			}
		} else {
			c.sourceDeclarationGraphValid = false
			invalidSnapshotFiles++
		}
		for _, module := range current {
			if previousHash, ok := previousHashes[module.Key]; !ok {
				c.addedSourceModuleDeclarations = append(c.addedSourceModuleDeclarations, module.Key)
				c.changedSourceModuleDeclarations = append(c.changedSourceModuleDeclarations, module.Key)
			} else if previousHash != module.PropertiesHash {
				c.changedSourceModuleDeclarations = append(c.changedSourceModuleDeclarations, module.Key)
			} else if previousDeclaration, ok := previousDeclarations[module.Key]; ok {
				changed, err := c.sourceModuleGlobsChanged(previousDeclaration.GlobCache, currentGlobHashes)
				if err != nil {
					return fmt.Errorf("checking cached globs for %q: %w", module.Key, err)
				}
				if changed {
					c.changedSourceModuleDeclarations = append(c.changedSourceModuleDeclarations, module.Key)
				}
			}
			delete(previousHashes, module.Key)
		}
		for key := range previousHashes {
			c.removedSourceModuleDeclarations = append(c.removedSourceModuleDeclarations, key)
		}
		for key, declaration := range previousDeclarations {
			c.previousSourceDeclarationDependents[key] = append(c.previousSourceDeclarationDependents[key], declaration.DependentDeclarations...)
			previousModuleCacheKeys[key] = append(previousModuleCacheKeys[key], declaration.ModuleCacheKeys...)
		}
	}
	for file := range previousFiles {
		previous, err := c.keyValueStoreCache.readSourceModuleDeclarationFile(c.EncContext, file)
		if err != nil {
			return err
		}
		if previous != nil {
			for _, module := range previous.Modules {
				c.removedSourceModuleDeclarations = append(c.removedSourceModuleDeclarations, module.Key)
				c.previousSourceDeclarationDependents[module.Key] = append(c.previousSourceDeclarationDependents[module.Key], module.DependentDeclarations...)
				previousModuleCacheKeys[module.Key] = append(previousModuleCacheKeys[module.Key], module.ModuleCacheKeys...)
			}
		}
	}
	sort.Strings(c.changedSourceModuleDeclarations)
	sort.Strings(c.addedSourceModuleDeclarations)
	sort.Strings(c.removedSourceModuleDeclarations)
	if len(c.addedSourceModuleDeclarations) != 0 || len(c.removedSourceModuleDeclarations) != 0 {
		c.sourceDeclarationGraphValid = false
	}
	if c.sourceDeclarationGraphValid {
		c.affectedSourceModuleDeclarations = affectedSourceDeclarations(c.changedSourceModuleDeclarations, c.previousSourceDeclarationDependents)
		seenModuleKeys := make(map[string]struct{})
		for _, sourceKey := range c.affectedSourceModuleDeclarations {
			for _, moduleKey := range previousModuleCacheKeys[sourceKey] {
				seenModuleKeys[moduleKey] = struct{}{}
			}
		}
		for moduleKey := range seenModuleKeys {
			c.affectedModuleCacheKeys = append(c.affectedModuleCacheKeys, moduleKey)
		}
		sort.Strings(c.affectedModuleCacheKeys)
	}
	if c.mutatorVisitStatsEnabled {
		fmt.Fprintf(os.Stderr, "soong: source declaration snapshot: valid=%t graph-complete=%t invalid-files=%d added=%d removed=%d changed=%d affected=%d\n",
			c.sourceDeclarationSnapshotValid, c.sourceDeclarationGraphValid, invalidSnapshotFiles,
			len(c.addedSourceModuleDeclarations), len(c.removedSourceModuleDeclarations),
			len(c.changedSourceModuleDeclarations), len(c.affectedSourceModuleDeclarations))
		if len(c.addedSourceModuleDeclarations) != 0 {
			fmt.Fprintf(os.Stderr, "soong: source declaration additions include: %q\n",
				strings.Join(c.addedSourceModuleDeclarations[:min(3, len(c.addedSourceModuleDeclarations))], ", "))
		}
		if len(c.removedSourceModuleDeclarations) != 0 {
			fmt.Fprintf(os.Stderr, "soong: source declaration removals include: %q\n",
				strings.Join(c.removedSourceModuleDeclarations[:min(3, len(c.removedSourceModuleDeclarations))], ", "))
		}
	}
	return nil
}

// sourceModuleGlobsChanged checks the previous build's glob results before
// mutators run. Blueprint source properties can be unchanged while files
// matched by those properties have been added or removed; those source modules
// still need to enter the incremental invalidation closure.
func (c *Context) sourceModuleGlobsChanged(globs []globResultCache, currentHashes map[globKey]proptools.Hash) (bool, error) {
	for _, glob := range globs {
		key := globToKey(glob.Pattern, glob.Excludes)
		hash, exists := currentHashes[key]
		if !exists {
			matches, err := c.glob(glob.Pattern, glob.Excludes)
			if err != nil {
				return false, err
			}
			hash, err = proptools.CalculateHash(stringList(matches))
			if err != nil {
				return false, err
			}
			currentHashes[key] = hash
		}
		if hash != glob.Result {
			return true, nil
		}
	}
	return false, nil
}

func affectedSourceDeclarations(changed []string, dependents map[string][]string) []string {
	seen := make(map[string]struct{}, len(changed))
	queue := slices.Clone(changed)
	for _, key := range changed {
		seen[key] = struct{}{}
	}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		for _, dependent := range dependents[key] {
			if _, exists := seen[dependent]; exists {
				continue
			}
			seen[dependent] = struct{}{}
			queue = append(queue, dependent)
		}
	}
	result := make([]string, 0, len(seen))
	for key := range seen {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func sourceModuleDeclarationsEqual(a, b []SourceModuleDeclaration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || a[i].PropertiesHash != b[i].PropertiesHash ||
			!slices.Equal(a[i].ModuleCacheKeys, b[i].ModuleCacheKeys) ||
			!slices.Equal(a[i].DependentDeclarations, b[i].DependentDeclarations) ||
			!globResultCachesEqual(a[i].GlobCache, b[i].GlobCache) {
			return false
		}
	}
	return true
}

func globResultCachesEqual(a, b []globResultCache) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].equal(b[i]) {
			return false
		}
	}
	return true
}

func (c *Context) writeSourceModuleDeclarationSnapshots() error {
	currentFiles := c.sourceDeclarationFiles()
	files := make([]string, 0, len(currentFiles))
	for file, current := range currentFiles {
		files = append(files, file)
		previous := c.sourceDeclarationFileSnapshots[file]
		if previous != nil && previous.Version == sourceDeclarationSnapshotVersion && sourceModuleDeclarationsEqual(previous.Modules, current) {
			continue
		}
		snapshot := &SourceModuleDeclarationFileSnapshot{Version: sourceDeclarationSnapshotVersion, Modules: current}
		if err := c.keyValueStoreCache.writeSourceModuleDeclarationFile(c.EncContext, file, snapshot); err != nil {
			return err
		}
	}
	sort.Strings(files)
	if c.sourceDeclarationFileIndex == nil || c.sourceDeclarationFileIndex.Version != sourceDeclarationSnapshotVersion ||
		c.sourceDeclarationFileIndex.GraphComplete != c.sourceDeclarationGraphComplete || !slices.Equal(c.sourceDeclarationFileIndex.Files, files) {
		index := &SourceDeclarationFileIndex{Version: sourceDeclarationSnapshotVersion, Files: files, GraphComplete: c.sourceDeclarationGraphComplete}
		if err := c.keyValueStoreCache.writeSourceDeclarationFileIndex(c.EncContext, index); err != nil {
			return err
		}
	}
	return nil
}

// @auto-generate: gob
type SingletonActionCachedData struct {
	CacheVersion                    int
	ProviderHashes                  []ProviderHash
	DependencyProviderHashes        map[int]proptools.Hash // Kept for decoding older cache entries.
	ModuleDependencyBloom           []uint64
	ModuleProviderDependencyBitsets []ModuleProviderDependencyBitset
	ModuleSetHash                   string
	SingletonProviderDependencies   []SingletonProviderDependency
	SingletonSetHash                string
	GlobCache                       []globResultCache
}

// @auto-generate: gob
type ModuleProviderDependencyBitset struct {
	ProviderId int
	Modules    []uint64
}

// @auto-generate: gob
type SingletonProviderDependency struct {
	SingletonName string
	ProviderId    int
	Hash          proptools.Hash
}

const singletonActionCacheVersion = 4

const moduleDependencyBloomBitsPerModule = 10
const moduleDependencyBloomHashCount = 7

func moduleDependencyBloomWords(moduleCount int) int {
	words := (moduleCount*moduleDependencyBloomBitsPerModule + 63) / 64
	if words < 1024 {
		return 1024
	}
	return words
}

func moduleDependencyBloomAdd(bloom []uint64, key string) {
	bitCount := uint64(len(bloom) * 64)
	if bitCount == 0 {
		return
	}
	first, step := moduleDependencyBloomHashes(key)
	for i := uint64(0); i < moduleDependencyBloomHashCount; i++ {
		bit := (first + i*step) % bitCount
		bloom[bit/64] |= uint64(1) << (bit % 64)
	}
}

func moduleDependencyBloomMayContain(bloom []uint64, key string) bool {
	bitCount := uint64(len(bloom) * 64)
	if bitCount == 0 {
		return false
	}
	first, step := moduleDependencyBloomHashes(key)
	for i := uint64(0); i < moduleDependencyBloomHashCount; i++ {
		bit := (first + i*step) % bitCount
		if bloom[bit/64]&(uint64(1)<<(bit%64)) == 0 {
			return false
		}
	}
	return true
}

func moduleDependencyBloomHashes(key string) (uint64, uint64) {
	first := uint64(14695981039346656037)
	second := uint64(14695981039346656037)
	for i := 0; i < len(key); i++ {
		b := uint64(key[i])
		first = (first ^ b) * 1099511628211
		second = (second * 1099511628211) ^ b
	}
	return first, second | 1
}

// A dbWriteRequest is passed to providerDbWriter() through writerCh to write a key-value pair to a database.
type dbWriteRequest struct {
	key   proptools.Hash
	value *bytes.Buffer
}

type KeyValueStoreCache struct {
	moduleActionsDb    dbtools.KeyValueStore
	singletonActionsDb dbtools.KeyValueStore
	// Use a separate DB for providers so that we only read them when necessary.
	providerDb   dbtools.KeyValueStore
	referencesDb dbtools.KeyValueStore
	ninjaDb      dbtools.KeyValueStore

	writerCh   chan dbWriteRequest
	writerDone chan bool

	hashesInProviderDb   map[proptools.Hash]struct{}
	cachedProviderHashes syncmap.SyncMap[proptools.Hash, CachedProvider]
}

var bufferPool = pool.New[bytes.Buffer]()

const maxCacheReadBufferCapacity = 8 * 1024 * 1024

var cacheReadBufferPool = sync.Pool{
	New: func() any { return make([]byte, 0, 64*1024) },
}

func (b *KeyValueStoreCache) openForTests() error {
	b.hashesInProviderDb = make(map[proptools.Hash]struct{})
	b.providerDbWriter()
	b.moduleActionsDb = &dbtools.InMemKeyValueStore{}
	b.singletonActionsDb = &dbtools.InMemKeyValueStore{}
	b.providerDb = &dbtools.InMemKeyValueStore{}
	b.referencesDb = &dbtools.InMemKeyValueStore{}
	b.ninjaDb = &dbtools.InMemKeyValueStore{}
	return nil
}

func (b *KeyValueStoreCache) open(dbPath string) error {
	b.hashesInProviderDb = make(map[proptools.Hash]struct{})
	b.providerDbWriter()
	return errors.Join(
		openDb(dbPath, moduleActionsDbName, &b.moduleActionsDb),
		openDb(dbPath, singletonActionsDbName, &b.singletonActionsDb),
		openDb(dbPath, providersDbName, &b.providerDb),
		openDb(dbPath, referencesDbName, &b.referencesDb),
		openDb(dbPath, ninjaDbName, &b.ninjaDb),
	)
}

// providerDbWriter starts a background goroutine that takes write requests from
// b.writerCh and writes them to the database.  It avoids lock contention
// on the database by moving all writes into a single goroutine, and verifies
// that a given provider hash is only written once.
func (b *KeyValueStoreCache) providerDbWriter() {
	b.writerCh = make(chan dbWriteRequest, 1000)
	b.writerDone = make(chan bool)
	go func() {
		defer close(b.writerDone)
		for req := range b.writerCh {
			b.handleDbWriteRequest(req)
		}
	}()
}

func (b *KeyValueStoreCache) handleDbWriteRequest(req dbWriteRequest) {
	// The request contains a buffer in req.value that should be returned to the pool.
	defer bufferPool.Put(req.value)

	if _, stored := b.hashesInProviderDb[req.key]; stored {
		return
	}
	b.hashesInProviderDb[req.key] = struct{}{}
	err := b.providerDb.Put(req.key.Bytes(), req.value.Bytes())
	if err != nil {
		panic(err)
	}
}

func openDb(dbPath string, dbName string, dbToOpen *dbtools.KeyValueStore) error {
	if *dbToOpen != nil {
		panic(fmt.Errorf("db %s is already open", dbName))
	}
	db, err := pogreb.Open(filepath.Join(dbPath, dbName), nil)
	if err != nil {
		return err
	}
	*dbToOpen = db
	return nil
}

func (b *KeyValueStoreCache) flush() {
	// Close the writerCh.  Any calls to write() concurrent with the call to flush() may panic.
	close(b.writerCh)
	// Wait for the providerDbWriter goroutine to finish.
	<-b.writerDone
	// Restart the providerDbWriter goroutine
	b.providerDbWriter()
}

func (b *KeyValueStoreCache) close() error {
	// Close the writerCh.  Any calls to write() after this will panic.
	close(b.writerCh)
	// Wait for the providerDbWriter goroutine to finish.
	<-b.writerDone
	return errors.Join(
		b.moduleActionsDb.Close(),
		b.singletonActionsDb.Close(),
		b.providerDb.Close(),
		b.referencesDb.Close(),
		b.ninjaDb.Close())
}

func (b *KeyValueStoreCache) reset(c *Context, dbPath string) error {
	c.BeginEvent("reset_build_action_cache")
	defer c.EndEvent("reset_build_action_cache")

	return errors.Join(
		c.fs.Remove(filepath.Join(dbPath, moduleActionsDbName)),
		c.fs.Remove(filepath.Join(dbPath, singletonActionsDbName)),
		c.fs.Remove(filepath.Join(dbPath, providersDbName)),
		c.fs.Remove(filepath.Join(dbPath, referencesDbName)),
		c.fs.Remove(filepath.Join(dbPath, ninjaDbName)))
}

func (b *KeyValueStoreCache) readModuleBuildAction(ctx gobtools.EncContext, key *DataCacheKey) (*ModuleActionCachedData, error) {
	var ret ModuleActionCachedData
	if ok, err := read(ctx, b.moduleActionsDb, key.bytes(), &ret); err != nil {
		return nil, err
	} else if !ok {
		return nil, nil
	}
	return &ret, nil
}

func (b *KeyValueStoreCache) readMutatorModuleState(ctx gobtools.EncContext, key []byte) (*MutatorModuleStateCachedData, error) {
	var ret MutatorModuleStateCachedData
	if ok, err := read(ctx, b.moduleActionsDb, key, &ret); err != nil {
		return nil, err
	} else if !ok {
		return nil, nil
	}
	return &ret, nil
}

func (b *KeyValueStoreCache) writeMutatorModuleState(ctx gobtools.EncContext, key []byte, data *MutatorModuleStateCachedData) error {
	return b.write(ctx, b.moduleActionsDb, key, data)
}

func (b *KeyValueStoreCache) readDependencyResolution(ctx gobtools.EncContext, key []byte) (string, error) {
	var ret MutatorModuleStateCachedData
	if ok, err := read(ctx, b.moduleActionsDb, key, &ret); err != nil {
		return "", err
	} else if !ok || !ret.Valid || ret.Version != dependencyResolutionCacheVersion {
		return "", nil
	}
	return ret.State, nil
}

func (b *KeyValueStoreCache) writeDependencyResolution(ctx gobtools.EncContext, key []byte, state string) error {
	return b.writeMutatorModuleState(ctx, key, &MutatorModuleStateCachedData{
		Version: dependencyResolutionCacheVersion,
		Valid:   true,
		State:   state,
	})
}

func (b *KeyValueStoreCache) readMutatorStateCacheVersion(ctx gobtools.EncContext, mutator string) (string, error) {
	data, err := b.readMutatorModuleState(ctx, mutatorStateCacheVersionKey(mutator))
	if err != nil || data == nil || !data.Valid || data.Version != mutatorModuleStateCacheVersion {
		return "", err
	}
	return data.State, nil
}

func (b *KeyValueStoreCache) writeMutatorStateCacheVersion(ctx gobtools.EncContext, mutator, version string) error {
	return b.writeMutatorModuleState(ctx, mutatorStateCacheVersionKey(mutator), &MutatorModuleStateCachedData{
		Version: mutatorModuleStateCacheVersion,
		Valid:   true,
		State:   version,
	})
}

func (b *KeyValueStoreCache) readSourceDeclarationFileIndex(ctx gobtools.EncContext) (*SourceDeclarationFileIndex, error) {
	var ret SourceDeclarationFileIndex
	ok, err := read(ctx, b.moduleActionsDb, []byte(sourceDeclarationSnapshotCacheKey), &ret)
	if err != nil || !ok {
		return nil, err
	}
	return &ret, nil
}

func (b *KeyValueStoreCache) readSourceModuleDeclarationFile(ctx gobtools.EncContext, file string) (*SourceModuleDeclarationFileSnapshot, error) {
	var ret SourceModuleDeclarationFileSnapshot
	ok, err := read(ctx, b.moduleActionsDb, sourceDeclarationFileSnapshotKey(file), &ret)
	if err != nil || !ok {
		return nil, err
	}
	return &ret, nil
}

func (b *KeyValueStoreCache) readNinjaStatements(key *DataCacheKey) ([]byte, error) {
	return b.ninjaDb.Get(key.bytes())
}

func (b *KeyValueStoreCache) readSingletonBuildAction(ctx gobtools.EncContext, key *DataCacheKey) (*SingletonActionCachedData, error) {
	var ret SingletonActionCachedData
	if ok, err := read(ctx, b.singletonActionsDb, key.bytes(), &ret); err != nil {
		return nil, err
	} else if !ok {
		return nil, nil
	}
	return &ret, nil
}

func (b *KeyValueStoreCache) readProvider(ctx gobtools.EncContext, hash proptools.Hash, provider *providerKey) (CachedProvider, error) {
	checkProvider := func(ret CachedProvider) {
		if *ret.Id != *provider {
			panic(fmt.Errorf("restored provider %#v but got provider %#v", provider, ret.Id))
		}
	}

	// Return the copy cached in memory if it has already been read from the disk cache.
	if cached, ok := b.cachedProviderHashes.Load(hash); ok {
		checkProvider(cached)
		return cached, nil
	}

	// Read from the disk cache.
	var ret CachedProvider
	ok, err := read(ctx, b.providerDb, hash.Bytes(), &ret)
	if err != nil {
		return CachedProvider{}, err
	} else if !ok {
		return CachedProvider{}, nil
	}

	checkProvider(ret)

	// Insert into the in-memory cache.
	ret, loaded := b.cachedProviderHashes.LoadOrStore(hash, ret)
	if loaded {
		checkProvider(ret)
	}

	return ret, nil
}

func read(ctx gobtools.EncContext, db dbtools.KeyValueStore, key []byte, ret gobtools.CustomDec) (bool, error) {
	var v []byte
	var scratch []byte
	reusableBuffer := false
	var err error
	if dbWithAppend, ok := db.(interface {
		GetAppend(key, buf []byte) ([]byte, error)
	}); ok {
		reusableBuffer = true
		scratch = cacheReadBufferPool.Get().([]byte)
		v, err = dbWithAppend.GetAppend(key, scratch[:0])
	} else {
		v, err = db.Get(key)
	}
	if err != nil {
		putCacheReadBuffer(scratch)
		return false, err
	}
	if v == nil {
		putCacheReadBuffer(scratch)
		return false, nil
	}
	if reusableBuffer {
		defer func() {
			putCacheReadBuffer(v)
			// GetAppend may allocate a larger slice while leaving the scratch slice
			// unused. Recycle both buffers when that happens.
			if cap(v) > cap(scratch) {
				putCacheReadBuffer(scratch)
			}
		}()
	}

	buf := bytes.NewReader(v)
	return true, ret.Decode(ctx, buf)
}

func putCacheReadBuffer(buf []byte) {
	if cap(buf) > 0 && cap(buf) <= maxCacheReadBufferCapacity {
		cacheReadBufferPool.Put(buf[:0])
	}
}

func (b *KeyValueStoreCache) writeModuleBuildAction(ctx gobtools.EncContext, key *DataCacheKey, data *ModuleActionCachedData) error {
	return b.write(ctx, b.moduleActionsDb, key.bytes(), data)
}

func (b *KeyValueStoreCache) writeSourceDeclarationFileIndex(ctx gobtools.EncContext, data *SourceDeclarationFileIndex) error {
	return b.write(ctx, b.moduleActionsDb, []byte(sourceDeclarationSnapshotCacheKey), data)
}

func (b *KeyValueStoreCache) writeSourceModuleDeclarationFile(ctx gobtools.EncContext, file string, data *SourceModuleDeclarationFileSnapshot) error {
	return b.write(ctx, b.moduleActionsDb, sourceDeclarationFileSnapshotKey(file), data)
}

func (b *KeyValueStoreCache) writeSingletonBuildAction(ctx gobtools.EncContext, key *DataCacheKey, data *SingletonActionCachedData) error {
	return b.write(ctx, b.singletonActionsDb, key.bytes(), data)
}

func (b *KeyValueStoreCache) writeProvider(ctx gobtools.EncContext, hash proptools.Hash, provider CachedProvider) error {
	buf := bufferPool.Get()
	buf.Reset()

	err := provider.Encode(ctx, buf)
	if err != nil {
		bufferPool.Put(buf)
		return err
	}

	// The buffer is transferred to the dbWriteRequest, so it must not be returned to the pool.
	b.writerCh <- dbWriteRequest{
		key:   hash,
		value: buf,
	}
	return nil
}

func (b *KeyValueStoreCache) writeNinjaStatements(key *DataCacheKey, data []byte) error {
	return b.ninjaDb.Put(key.bytes(), data)
}

// write encodes data to a byte buffer, and then sends a write request to the providerDbWriter goroutine to write it to the
// database.
func (b *KeyValueStoreCache) write(ctx gobtools.EncContext, db dbtools.KeyValueStore, key []byte, data gobtools.CustomEnc) error {
	buf := bufferPool.Get()
	defer bufferPool.Put(buf)
	buf.Reset()

	err := data.Encode(ctx, buf)
	if err != nil {
		return err
	}
	return db.Put(key, buf.Bytes())
}

// @auto-generate: gob
type OrderOnlyStringsCache map[string][]string
