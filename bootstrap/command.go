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

package bootstrap

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"runtime/trace"
	"strings"

	"github.com/google/blueprint"
	"github.com/google/blueprint/pathtools"
	"github.com/google/blueprint/proptools"
)

type Args struct {
	ModuleListFile string
	OutFile        string

	EmptyNinjaFile bool

	NoGC       bool
	Cpuprofile string
	Memprofile string
	TraceFile  string

	// Debug data json file
	ModuleDebugFile            string
	IncrementalBuildActions    bool
	IncrementalDebugMissesOnly bool
	// ShardNinja enables reusable module and singleton Ninja shards for uni.
	ShardNinja              bool
	IncrementalProviderTest bool
	IncrementalDebugFile    string
	PartialAnalysisTargets  string
}

func useUniNinjaShards(args Args) bool {
	return args.ShardNinja && !args.EmptyNinjaFile && !strings.Contains(args.OutFile, "bootstrap.ninja")
}

// RegisterGoModuleTypes adds module types to build tools written in golang
func RegisterGoModuleTypes(ctx *blueprint.Context) {
	ctx.RegisterModuleType("bootstrap_go_package", newGoPackageModuleFactory())
	ctx.RegisterModuleType("blueprint_go_binary", newGoBinaryModuleFactory())
}

// GoModuleTypesAreWrapped is called by Soong before calling RunBlueprint to provide its own wrapped
// implementations of bootstrap_go_package and blueprint_go_bianry.
func GoModuleTypesAreWrapped() {
	goModuleTypesAreWrapped = true
}

var goModuleTypesAreWrapped = false

// RunBlueprint emits `args.OutFile` (a Ninja file) and returns the list of
// its dependencies. These can be written to a `${args.OutFile}.d` file
// so that it is correctly rebuilt when needed in case Blueprint is itself
// invoked from Ninja
func RunBlueprint(args Args, stopBefore StopBefore, ctx *blueprint.Context, config interface{}) ([]string, error) {
	runtime.GOMAXPROCS(runtime.NumCPU())

	if args.NoGC {
		debug.SetGCPercent(-1)
	}

	if args.Cpuprofile != "" {
		f, err := os.Create(blueprint.JoinPath(ctx.SrcDir(), args.Cpuprofile))
		if err != nil {
			return nil, fmt.Errorf("error opening cpuprofile: %s", err)
		}
		pprof.StartCPUProfile(f)
		defer f.Close()
		defer pprof.StopCPUProfile()
	}

	if args.TraceFile != "" {
		f, err := os.Create(blueprint.JoinPath(ctx.SrcDir(), args.TraceFile))
		if err != nil {
			return nil, fmt.Errorf("error opening trace: %s", err)
		}
		trace.Start(f)
		defer f.Close()
		defer trace.Stop()
	}

	if args.ModuleListFile == "" {
		return nil, fmt.Errorf("-l <moduleListFile> is required and must be nonempty")
	}
	ctx.SetModuleListFile(args.ModuleListFile)

	var ninjaDeps []string
	ninjaDeps = append(ninjaDeps, args.ModuleListFile)

	ctx.BeginEvent("list_modules")
	var filesToParse []string
	if f, err := ctx.ListModulePaths("."); err != nil {
		return nil, fmt.Errorf("could not enumerate files: %v\n", err.Error())
	} else {
		filesToParse = f
	}
	ctx.EndEvent("list_modules")

	ctx.RegisterFirstBottomUpMutator("bootstrap_deps", BootstrapDeps).UsesReverseDependencies()
	ctx.RegisterSingletonType("bootstrap", newSingletonFactory(), false)
	if !goModuleTypesAreWrapped {
		RegisterGoModuleTypes(ctx)
	}

	if ctx.GetIncrementalAnalysis() {
		err := ctx.RestoreGlobsFromCache(blueprint.JoinPath(ctx.SrcDir(), args.OutFile))
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to restore globs from cache, continuing: %s", err)
		}
	}

	ctx.BeginEvent("parse_bp")
	if blueprintFiles, errs := ctx.ParseFileList(".", filesToParse, config); len(errs) > 0 {
		return nil, colorizeErrs(errs)
	} else {
		ctx.EndEvent("parse_bp")
		ninjaDeps = append(ninjaDeps, blueprintFiles...)
	}
	if err := ctx.PrepareIncrementalAnalysis(); err != nil {
		return nil, colorizeErrs([]error{err})
	}

	if resolvedDeps, errs := ctx.ResolveDependencies(config); len(errs) > 0 {
		return nil, colorizeErrs(errs)
	} else {
		ninjaDeps = append(ninjaDeps, resolvedDeps...)
	}

	if stopBefore == StopBeforePrepareBuildActions {
		return ninjaDeps, nil
	}

	if ctx.BeforePrepareBuildActionsHook != nil {
		if err := ctx.BeforePrepareBuildActionsHook(); err != nil {
			return nil, colorizeErrs([]error{err})
		}
	}

	if ctx.GetIncrementalAnalysis() {
		ctx.BeginEvent("restore_build_actions")
		err := ctx.RestoreAllBuildActions(config.(BootstrapConfig).SoongOutDir())
		ctx.EndEvent("restore_build_actions")
		if err != nil {
			return nil, colorizeErrs([]error{err})
		}
	}

	if args.ModuleDebugFile != "" {
		finishFunc := ctx.InitializeModuleDebugInfoCollection(args.ModuleDebugFile)
		defer finishFunc()
	}

	if buildActionsDeps, errs := ctx.PrepareBuildActions(config); len(errs) > 0 {
		return nil, colorizeErrs(errs)
	} else {
		ninjaDeps = append(ninjaDeps, buildActionsDeps...)
	}

	if stopBefore == StopBeforeWriteNinja {
		return ninjaDeps, nil
	}

	providersValidationChan := make(chan []error, 1)
	go func() {
		providersValidationChan <- ctx.VerifyProvidersWereUnchanged()
	}()

	ctx.RecordSandboxMetrics(config)

	var out blueprint.StringWriterWriter
	var buf *bufio.Writer

	ctx.BeginEvent("write_files")
	defer ctx.EndEvent("write_files")
	if args.EmptyNinjaFile {
		if err := os.WriteFile(blueprint.JoinPath(ctx.SrcDir(), args.OutFile), []byte(nil), blueprint.OutFilePermissions); err != nil {
			return nil, fmt.Errorf("error writing empty Ninja file: %s", err)
		}
		out = io.Discard.(blueprint.StringWriterWriter)
	} else {
		var f io.WriteCloser
		var err error
		if useUniNinjaShards(args) {
			f, err = pathtools.OpenWithContentComparison(pathtools.OsFs, blueprint.JoinPath(ctx.SrcDir(), args.OutFile))
		} else {
			f, err = os.OpenFile(blueprint.JoinPath(ctx.SrcDir(), args.OutFile), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, blueprint.OutFilePermissions)
		}
		if err != nil {
			return nil, fmt.Errorf("error opening Ninja file: %s", err)
		}
		defer f.Close()
		buf = bufio.NewWriterSize(f, 16*1024*1024)
		out = buf
	}

	if err := ctx.WriteBuildFile(out, useUniNinjaShards(args), args.OutFile); err != nil {
		return nil, fmt.Errorf("error writing Ninja file contents: %s", err)
	}

	if buf != nil {
		if err := buf.Flush(); err != nil {
			return nil, fmt.Errorf("error flushing Ninja file contents: %s", err)
		}
	}

	// TODO(b/357140398): parallelize this with other ninja file writing work.
	if ctx.GetIncrementalEnabled() {
		ctx.BeginEvent("cache_build_actions")
		err := ctx.CacheAllBuildActions(config.(BootstrapConfig).SoongOutDir())
		ctx.EndEvent("cache_build_actions")
		if err != nil {
			return nil, fmt.Errorf("error cache build actions: %s", err)
		}
	}

	providerValidationErrors := <-providersValidationChan
	if providerValidationErrors != nil {
		return nil, proptools.MergeErrors(providerValidationErrors)
	}

	if args.Memprofile != "" {
		f, err := os.Create(blueprint.JoinPath(ctx.SrcDir(), args.Memprofile))
		if err != nil {
			return nil, fmt.Errorf("error opening memprofile: %s", err)
		}
		defer f.Close()
		pprof.WriteHeapProfile(f)
	}

	return ninjaDeps, nil
}

func colorizeErrs(errs []error) error {
	red := "\x1b[31m"
	unred := "\x1b[0m"

	var colorizedErrs []error
	for _, err := range errs {
		switch err := err.(type) {
		case *blueprint.BlueprintError,
			*blueprint.ModuleError,
			*blueprint.PropertyError:
			colorizedErrs = append(colorizedErrs, fmt.Errorf("%serror:%s %w", red, unred, err))
		default:
			colorizedErrs = append(colorizedErrs, fmt.Errorf("%sinternal error:%s %s", red, unred, err))
		}
	}

	return errors.Join(colorizedErrs...)
}
