// Copyright 2015 Google Inc. All rights reserved.
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
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/blueprint/pathtools"
)

func verifyGlob(key globKey, pattern string, excludes []string, g pathtools.GlobResult) {
	if pattern != g.Pattern {
		panic(fmt.Errorf("Mismatched patterns %q and %q for glob key %q", pattern, g.Pattern, key))
	}
	if len(excludes) != len(g.Excludes) {
		panic(fmt.Errorf("Mismatched excludes %v and %v for glob key %q", excludes, g.Excludes, key))
	}

	for i := range excludes {
		if g.Excludes[i] != excludes[i] {
			panic(fmt.Errorf("Mismatched excludes %v and %v for glob key %q", excludes, g.Excludes, key))
		}
	}
}

func (c *Context) glob(pattern string, excludes []string) ([]string, error) {
	// Sort excludes so that two globs with the same excludes in a different order reuse the same
	// key.  Make a copy first to avoid modifying the caller's version.
	excludes = slices.Clone(excludes)
	sort.Strings(excludes)

	key := globToKey(pattern, excludes)
	c.nonMeowSourceGlobs.Store(key, true)
	return c.globWithKey(key, pattern, excludes)
}

func (c *Context) globForMeowSource(pattern string, excludes []string) ([]string, error) {
	excludes = slices.Clone(excludes)
	sort.Strings(excludes)
	key := globToKey(pattern, excludes)
	c.meowSourceGlobs.Store(key, true)
	return c.globWithKey(key, pattern, excludes)
}

// Validating a cached module's source list is not a new configuration use of
// that glob. Preserve the previous (globally checked) source-only classification
// when its cached declaration is available. A real regular use still calls
// glob and overrides source-only classification in WriteGlobFile. Otherwise
// inspect membership without registering a consumer: the cached actions may be
// rejected, in which case only freshly generated actions own the new glob.
func (c *Context) globForCachedBuildActions(pattern string, excludes []string) ([]string, error) {
	excludes = slices.Clone(excludes)
	sort.Strings(excludes)
	key := globToKey(pattern, excludes)
	if previous, ok := c.restoredGlobsFromCache[key]; ok && previous.MeowSourceOnly {
		return c.globForMeowSource(pattern, excludes)
	}
	return c.globWithKey(key, pattern, excludes)
}

// recordRestoredBuildActionGlobs commits uses only after all cache validation
// has succeeded. Unknown or ordinary cached consumers must still invalidate
// Soong, even if another module dynamically consumes the same glob.
func (c *Context) recordRestoredBuildActionGlobs(globs []globResultCache) {
	for _, glob := range globs {
		excludes := slices.Clone(glob.Excludes)
		sort.Strings(excludes)
		key := globToKey(glob.Pattern, excludes)
		if previous, ok := c.restoredGlobsFromCache[key]; ok && previous.MeowSourceOnly {
			c.meowSourceGlobs.Store(key, true)
		} else {
			c.nonMeowSourceGlobs.Store(key, true)
		}
	}
}

func (c *Context) globWithKey(key globKey, pattern string, excludes []string) ([]string, error) {
	// Try to get existing glob from the stored results, compute it otherwise
	entry, exists := c.globs.LoadOrCompute(key, func() globsMapEntry {
		// Check if the glob was restored from the cache
		if cachedGlob, exists := c.restoredGlobsFromCache[key]; exists {
			return globsMapEntry{result: cachedGlob}
		}
		// Get a globbed file list
		g, err := c.fs.Glob(pattern, excludes, pathtools.FollowSymlinks)
		if err != nil {
			return globsMapEntry{err: err}
		}
		return globsMapEntry{result: g}
	})

	if entry.err != nil {
		return nil, entry.err
	}

	g := entry.result
	if exists {
		// Glob has already been done, double check it is identical
		verifyGlob(key, pattern, excludes, g)
	}
	// Return a copy so that modifications don't affect the cached value.
	return slices.Clone(g.Matches), nil
}

// WriteGlobFile writes the list of globs and a timestamp to files next to finalOutFile.
func (c *Context) WriteGlobFile(finalOutFile string, startTime time.Time) error {
	var globKeys []globKey
	c.globs.Range(func(key globKey, value globsMapEntry) bool {
		globKeys = append(globKeys, key)
		return true
	})
	slices.SortFunc(globKeys, func(a, b globKey) int {
		return cmp.Or(cmp.Compare(a.pattern, b.pattern), cmp.Compare(a.excludes, b.excludes))
	})
	globs := make([]pathtools.GlobResult, 0, len(globKeys))
	for _, key := range globKeys {
		entry, _ := c.globs.Load(key)
		result := entry.result
		_, sourceOnly := c.meowSourceGlobs.Load(key)
		_, regularUse := c.nonMeowSourceGlobs.Load(key)
		result.MeowSourceOnly = sourceOnly && !regularUse
		globs = append(globs, result)
	}

	globsFile, err := os.Create(finalOutFile + ".globs")
	if err != nil {
		return err
	}
	defer globsFile.Close()
	if err := pathtools.WriteGlobFile(globsFile, globs); err != nil {
		return err
	}

	return os.WriteFile(
		finalOutFile+".globs_time",
		[]byte(fmt.Sprintf("%d\n", startTime.UnixMicro())),
		0666,
	)
}

// RestoreGlobsFromCache reads the list of globs that was written by WriteGlobFile during a
// previous run and returns any whose dependencies have not changed to avoid redoing all the
// glob traversals during incremental analysis.
func (c *Context) RestoreGlobsFromCache(finalOutFile string) error {
	c.EventHandler.Begin("restore_globs")
	defer c.EventHandler.End("restore_globs")

	globsFile, err := os.Open(finalOutFile + ".globs")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer globsFile.Close()

	globsTimeBytes, err := os.ReadFile(finalOutFile + ".globs_time")
	if errors.Is(err, os.ErrNotExist) {
		globsTimeBytes = []byte("0")
	} else if err != nil {
		return err
	}

	globsTimeMicros, err := strconv.ParseInt(strings.TrimSpace(string(globsTimeBytes)), 10, 64)
	if err != nil {
		return err
	}

	globs, globMetrics, err := pathtools.RestoreGlobsFromCache(c.fs, globsFile, globsTimeMicros)
	if err != nil {
		return err
	}

	c.restoredGlobMetrics = globMetrics

	c.restoredGlobsFromCache = make(map[globKey]pathtools.GlobResult, len(globs))
	for _, glob := range globs {
		key := globToKey(glob.Pattern, glob.Excludes)
		c.restoredGlobsFromCache[key] = glob
		if glob.MeowSourceOnly {
			c.meowSourceGlobs.Store(key, true)
		}
	}

	return nil
}

// globKey combines a pattern and a list of excludes into a hashable struct to be used as a key in
// a map.
type globKey struct {
	pattern  string
	excludes string
}

type globsMapEntry struct {
	result pathtools.GlobResult
	err    error
}

// globToKey converts a pattern and an excludes list into a globKey struct that is hashable and
// usable as a key in a map.
func globToKey(pattern string, excludes []string) globKey {
	return globKey{pattern, strings.Join(excludes, "|")}
}
