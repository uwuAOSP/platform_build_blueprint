package bootstrap

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/blueprint"
	"github.com/google/blueprint/pathtools"
)

type testConfig struct {
	outDir string
}

func (t *testConfig) HostToolDir() string {
	return filepath.Join(t.outDir, "host")
}

func (t *testConfig) SoongOutDir() string {
	panic("implement me")
}

func (t *testConfig) OutDir() string {
	return t.outDir
}

func (t *testConfig) DebugCompilation() bool {
	return false
}

func (t *testConfig) RunGoTests() bool {
	return true
}

func (t *testConfig) Subninjas() []string {
	panic("implement me")
}

func (t *testConfig) PrimaryBuilderInvocations() []PrimaryBuilderInvocation {
	panic("implement me")
}

func (t *testConfig) IsBootstrap() bool {
	return true
}

func (t *testConfig) IsActionSandboxedBuild() bool {
	return false
}

func (t *testConfig) ActionSandboxMetrics() *blueprint.SandboxMetrics {
	return nil
}

var _ BootstrapConfig = &testConfig{}

func TestUseUniNinjaShards(t *testing.T) {
	tests := []struct {
		name string
		args Args
		want bool
	}{
		{name: "traditional Soong graph", args: Args{OutFile: "build.product.ninja"}},
		{name: "uni Soong graph", args: Args{OutFile: "build.product.ninja", ShardNinja: true}, want: true},
		{name: "bootstrap graph", args: Args{OutFile: "bootstrap.ninja", ShardNinja: true}},
		{name: "empty graph", args: Args{OutFile: "build.product.ninja", ShardNinja: true, EmptyNinjaFile: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := useUniNinjaShards(test.args); got != test.want {
				t.Fatalf("useUniNinjaShards(%+v) = %t, want %t", test.args, got, test.want)
			}
		})
	}
}

func TestBootstrap(t *testing.T) {
	ctx := blueprint.NewContext()
	ctx.CaptureBuildParams()
	ctx.MockFileSystem(map[string][]byte{
		"Android.bp": []byte(`
			bootstrap_go_package {
				name: "a",
				pkgPath: "a",
				srcs: ["a.go"],
				testSrcs: ["a_test.go"],
			}

			bootstrap_go_package {
				name: "b",
				pkgPath: "b",
				srcs: ["b.go"],
				deps: ["a"],
				testSrcs: ["b_test.go"],
			}

			bootstrap_go_package {
				name: "c",
				pkgPath: "c",
				srcs: ["c.go"],
				deps: ["a"],
				// c has no tests
			}

			bootstrap_go_package {
				name: "d",
				pkgPath: "d",
				srcs: ["d.go"],
				testSrcs: ["d_test.go"],
			}

			bootstrap_go_package {
				name: "e",
				pkgPath: "e",
				srcs: ["e.go"],
				testSrcs: ["e_test.go"],
				deps: ["d"],
			}

			blueprint_go_binary {
				name: "bin",
				srcs: ["bin.go"],
				deps: ["b", "c", "d", "e"],
				testSrcs: ["bin_test.go"],
			}
		`),
	})

	ctx.RegisterModuleType("bootstrap_go_package", newGoPackageModuleFactory())
	ctx.RegisterModuleType("blueprint_go_binary", newGoBinaryModuleFactory())

	config := &testConfig{
		outDir: t.TempDir(),
	}

	_, errs := ctx.ParseBlueprintsFiles("Android.bp", config)
	if len(errs) > 0 {
		t.Errorf("unexpected parse errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	_, errs = ctx.ResolveDependencies(config)
	if len(errs) > 0 {
		t.Errorf("unexpected dep errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	_, errs = ctx.PrepareBuildActions(config)
	if len(errs) > 0 {
		t.Errorf("unexpected prepare actions errors:")
		for _, err := range errs {
			t.Errorf("  %s", err)
		}
		t.FailNow()
	}

	assertModule(t, ctx, config, expectedModuleInfo{
		name: "a",
	})
	assertModule(t, ctx, config, expectedModuleInfo{
		name:               "b",
		incDirs:            []string{"a/pkg"},
		compileDeps:        []string{"a/pkg/a.a"},
		testValidationDeps: []string{"a/test/test.passed"},
	})
	assertModule(t, ctx, config, expectedModuleInfo{
		name:        "c",
		incDirs:     []string{"a/pkg"},
		compileDeps: []string{"a/pkg/a.a"},
		// c has no tests so validation dependencies on dependency tests is via
		// the test.dependencies output
		testDependenciesValidationDeps: []string{"a/test/test.passed"},
	})
	assertModule(t, ctx, config, expectedModuleInfo{
		name: "d",
	})
	assertModule(t, ctx, config, expectedModuleInfo{
		name:               "e",
		incDirs:            []string{"d/pkg"},
		compileDeps:        []string{"d/pkg/d.a"},
		testValidationDeps: []string{"d/test/test.passed"},
	})
	assertModule(t, ctx, config, expectedModuleInfo{
		name: "bin",
		// incDirs and compileDeps should only include direct dependencies so that an explicit
		// import in a go source file of a transitive dependency fails.
		incDirs:     []string{"b/pkg", "c/pkg", "d/pkg", "e/pkg"},
		compileDeps: []string{"b/pkg/b.a", "c/pkg/c.a", "d/pkg/d.a", "e/pkg/e.a"},
		// linkDirs and linkDeps should include all transitive dependencies.
		linkDirs: []string{"a/pkg", "d/pkg", "b/pkg", "c/pkg", "e/pkg"},
		linkDeps: []string{"a/pkg/a.a", "d/pkg/d.a", "b/pkg/b.a", "c/pkg/c.a", "e/pkg/e.a"},
		// test validation deps contains only the direct deps because the test rule of the direct
		// deps depends on the test rules of the transitive deps.
		testValidationDeps: []string{"b/test/test.passed", "c/test/test.dependencies",
			"d/test/test.passed", "e/test/test.passed"},
		installValidationDeps: []string{"bin/test/test.passed"},
	})
}

type expectedModuleInfo struct {
	name                           string
	incDirs                        []string
	compileDeps                    []string
	linkDirs                       []string
	linkDeps                       []string
	testValidationDeps             []string
	testDependenciesValidationDeps []string
	installValidationDeps          []string
}

func assertModule(t *testing.T, ctx *blueprint.Context, config *testConfig, expected expectedModuleInfo) {
	t.Helper()
	m := moduleByName(ctx, expected.name)
	goDir := filepath.Join(config.HostToolDir(), "go")
	compile := buildParamsForOutput(ctx, m, filepath.Join(goDir, expected.name, "pkg", expected.name+".a"))
	link := buildParamsForOutput(ctx, m, filepath.Join(goDir, expected.name, "pkg", expected.name))
	test := buildParamsForOutput(ctx, m, filepath.Join(goDir, expected.name, "test", "test.passed"))
	testDependencies := buildParamsForOutput(ctx, m, filepath.Join(goDir, expected.name, "test", "test.dependencies"))

	install := buildParamsForOutput(ctx, m, filepath.Join(config.HostToolDir(), expected.name))

	if g, w := compile.Args["incFlags"], joinWithPrefix(expected.incDirs, "-I "+goDir+"/"); g != w {
		t.Errorf("expected %s incFlags %q, got %q", expected.name, w, g)
	}

	if g, w := compile.Implicits, append(pathtools.PrefixPaths(expected.compileDeps, goDir), "go_toolchain"); !slices.Equal(g, w) {
		t.Errorf("expected %s compile deps %q, got %q", expected.name, w, g)
	}

	if g, w := link.Args["libDirFlags"], joinWithPrefix(expected.linkDirs, "-L "+goDir+"/"); g != w {
		t.Errorf("expected %s link flags %q, got %q", expected.name, w, g)
	}

	if link.Rule != nil {
		if g, w := link.Implicits, append(pathtools.PrefixPaths(expected.linkDeps, goDir), "go_toolchain"); !slices.Equal(g, w) {
			t.Errorf("expected %s link deps %q, got %q", expected.name, w, g)
		}
	}

	if g, w := test.Validations, pathtools.PrefixPaths(expected.testValidationDeps, goDir); !slices.Equal(g, w) {
		t.Errorf("expected %s test validation deps %q, got %q", expected.name, w, g)
	}

	if g, w := testDependencies.Validations, pathtools.PrefixPaths(expected.testDependenciesValidationDeps, goDir); !slices.Equal(g, w) {
		t.Errorf("expected %s test dependencies validation deps %q, got %q", expected.name, w, g)
	}

	if g, w := install.Validations, pathtools.PrefixPaths(expected.installValidationDeps, goDir); !slices.Equal(g, w) {
		t.Errorf("expected %s install validation deps %q, got %q", expected.name, w, g)
	}

}

func joinWithPrefix(list []string, prefix string) string {
	sb := strings.Builder{}
	for i, s := range list {
		if i != 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(prefix)
		sb.WriteString(s)
	}
	return sb.String()
}

func moduleByName(ctx *blueprint.Context, name string) blueprint.ModuleProxy {
	var ret blueprint.ModuleProxy
	ctx.VisitAllModulesProxies(func(module blueprint.ModuleProxy) {
		if ctx.ModuleName(module) == name {
			ret = module
		}
	})
	return ret
}

func buildParamsForOutput(ctx *blueprint.Context, module blueprint.ModuleProxy, output string) blueprint.BuildParams {
	buildParams := ctx.BuildParamsForModule(module)
	for _, b := range buildParams {
		if slices.Contains(b.Outputs, output) {
			return b
		}
	}
	return blueprint.BuildParams{}
}
