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
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/blueprint/uniquelist"
)

// A Deps value indicates the dependency file format that Ninja should expect to
// be output by a compiler.
type Deps int

const (
	DepsNone Deps = iota
	DepsGCC
	DepsMSVC
)

func (d Deps) String() string {
	switch d {
	case DepsNone:
		return "none"
	case DepsGCC:
		return "gcc"
	case DepsMSVC:
		return "msvc"
	default:
		panic(fmt.Sprintf("unknown deps value: %d", d))
	}
}

// A PoolParams object contains the set of parameters that make up a Ninja pool
// definition.
type PoolParams struct {
	Comment string // The comment that will appear above the definition.
	Depth   int    // The Ninja pool depth.
}

// A RuleParams object contains the set of parameters that make up a Ninja rule
// definition.
type RuleParams struct {
	// These fields correspond to a Ninja variable of the same name.
	Command2        Command // The command that Ninja will run for the rule.
	Command         string  // The command that Ninja will run for the rule.
	Depfile         string  // The dependency file name.
	Deps            Deps    // The format of the dependency file.
	Description     string  // The description that Ninja will print for the rule.
	Generator       bool    // Whether the rule generates the Ninja manifest file.
	Pool            Pool    // The Ninja pool to which the rule belongs.
	Restat          bool    // Whether Ninja should re-stat the rule's outputs.
	Rspfile         string  // The response file.
	RspfileContent  string  // The response file content.
	SandboxDisabled bool    // Whether to disable sandboxing for this rule
	Source          string  // A string identify the source of this rule, used to dedup rules in the reporting of certain metrics

	// These fields are used internally in Blueprint
	CommandDeps      []string   // Command-specific implicit dependencies to prepend to builds
	CommandDepsTools []HostTool // Command-specific implicit dependencies, but host tools, used to update CommandDeps
	CommandOrderOnly []string   // Command-specific order-only dependencies to prepend to builds
	Comment          string     // The comment that will appear above the definition.
}

// A command that ninja will run. It's an interpolation of strings and references to host tools,
// created with the NewCommand() function.
type Command struct {
	command []any
}

func (c *Command) IsEmpty() bool {
	return len(c.command) == 0
}

func (c *Command) getCommandAndDeps(config any) (string, []string, error) {
	var result strings.Builder
	var deps []string
	for _, part := range c.command {
		switch p := part.(type) {
		case string:
			result.WriteString(p)
		case HostTool:
			cmd, newDeps, err := p.GetValueAndDeps(config)
			if err != nil {
				return "", nil, err
			}
			result.WriteString(cmd)
			deps = append(deps, newDeps...)
		default:
			panic(fmt.Sprintf("unexpected type in Command: %v", p))
		}
	}
	return result.String(), deps, nil
}

// A host tool that may be used an a RuleParam's Command2.
// The value is the value to put in the command line, and deps are values to put in CommandDeps.
// The tool must be listed in Deps even if it is simply the Value, as some tools like toybox
// may supply default arguments in Value as well, in which case Value couldn't be directly used
// as a dep.
type HostTool struct {
	// The inner pointer is so that the HostTool can be passed by value to NewCommand().
	// If the contents of inner were inlined here, the HostTool would be copied when passing
	// to NewCommand(), causing the once to not have an affect and the callback function to be
	// recalculated many times.
	inner *hostToolInner
}

type hostToolInner struct {
	once  sync.Once
	f     func(config any) (HostToolParams, error)
	value string
	deps  []string
	err   error
}

// Gets the value (the string to put in the command line) and deps of this host tool.
// Generally, you shouldn't call this, you should pass the HostTool to a blueprint.NewCommand().
// However in some cases where we build a command from strings it's necessary.
func (h *HostTool) GetValueAndDeps(config any) (string, []string, error) {
	h.inner.once.Do(func() {
		params, err := h.inner.f(config)
		h.inner.value = params.Value
		h.inner.deps = params.Deps
		h.inner.err = err
	})
	return h.inner.value, h.inner.deps, h.inner.err
}

type HostToolParams struct {
	Value string
	Deps  []string
}

func NewCommand(args ...any) Command {
	for _, arg := range args {
		switch arg.(type) {
		case string:
		case HostTool:
		default:
			panic(fmt.Sprintf("unexpected type in NewCommand: %v", arg))
		}
	}
	return Command{
		args,
	}
}

// A BuildParams object contains the set of parameters that make up a Ninja
// build statement.  Each field except for Args corresponds with a part of the
// Ninja build statement.  The Args field contains variable names and values
// that are set within the build statement's scope in the Ninja file.
type BuildParams struct {
	Comment         string            // The comment that will appear above the definition.
	Depfile         string            // The dependency file name.
	Deps            Deps              // The format of the dependency file.
	Description     string            // The description that Ninja will print for the build.
	Rule            Rule              // The rule to invoke.
	Outputs         []string          // The list of explicit output targets.
	ImplicitOutputs []string          // The list of implicit output targets.
	Inputs          []string          // The list of explicit input dependencies.
	Implicits       []string          // The list of implicit input dependencies.
	OrderOnly       []string          // The list of order-only dependencies.
	Validations     []string          // The list of validations to run when this rule runs.
	Args            map[string]string // The variable/value pairs to set.
	Variables       map[string]string // Additional edge-scoped Ninja bindings.
	Default         bool              // Output a ninja default statement
	PhonyOutput     bool              // This is a phony_output
}

// A poolDef describes a pool definition.  It does not include the name of the
// pool.
type poolDef struct {
	Comment string
	Depth   int
}

func parsePoolParams(scope scope, params *PoolParams) (*poolDef,
	error) {

	def := &poolDef{
		Comment: params.Comment,
		Depth:   params.Depth,
	}

	return def, nil
}

func (p *poolDef) WriteTo(nw *ninjaWriter, name string) error {
	if p.Comment != "" {
		err := nw.Comment(p.Comment)
		if err != nil {
			return err
		}
	}

	err := nw.Pool(name)
	if err != nil {
		return err
	}

	return nw.ScopedAssign("depth", strconv.Itoa(p.Depth))
}

// A ruleDef describes a rule definition.  It does not include the name of the
// rule.
type ruleDef struct {
	CommandDeps      []*ninjaString
	CommandOrderOnly []*ninjaString
	Comment          string
	Pool             Pool
	Variables        map[string]*ninjaString
	SandboxDisabled  bool
	Source           string
}

func parseRuleParams(config any, scope scope, params *RuleParams) (*ruleDef, error) {

	r := &ruleDef{
		Comment:   params.Comment,
		Pool:      params.Pool,
		Variables: make(map[string]*ninjaString),
	}

	if params.Command == "" && params.Command2.IsEmpty() {
		return nil, fmt.Errorf("encountered rule params with no command " +
			"specified")
	} else if params.Command != "" && !params.Command2.IsEmpty() {
		cmd2, _, _ := params.Command2.getCommandAndDeps(config)
		return nil, fmt.Errorf("cannot set both Command and Command2 in rule params: %q, %q", params.Command, cmd2)
	}

	if !params.Command2.IsEmpty() {
		cmd, deps, err := params.Command2.getCommandAndDeps(config)
		if err != nil {
			return nil, err
		}
		params.Command = cmd
		params.CommandDeps = append(params.CommandDeps, deps...)
	}

	if params.CommandDepsTools != nil {
		for _, hostTool := range params.CommandDepsTools {
			_, deps, err := hostTool.GetValueAndDeps(config)
			if err != nil {
				return nil, err
			}
			params.CommandDeps = append(params.CommandDeps, deps...)
		}
	}

	if r.Pool != nil && !scope.IsPoolVisible(r.Pool) {
		return nil, fmt.Errorf("Pool %s is not visible in this scope", r.Pool)
	}

	value, err := parseNinjaString(scope, params.Command)
	if err != nil {
		return nil, fmt.Errorf("error parsing Command param: %s", err)
	}
	r.Variables["command"] = value

	if params.Depfile != "" {
		value, err = parseNinjaString(scope, params.Depfile)
		if err != nil {
			return nil, fmt.Errorf("error parsing Depfile param: %s", err)
		}
		r.Variables["depfile"] = value
	}

	if params.Deps != DepsNone {
		r.Variables["deps"] = simpleNinjaString(params.Deps.String())
	}

	if params.Description != "" {
		value, err = parseNinjaString(scope, params.Description)
		if err != nil {
			return nil, fmt.Errorf("error parsing Description param: %s", err)
		}
		r.Variables["description"] = value
	}

	if params.Generator {
		r.Variables["generator"] = simpleNinjaString("true")
	}

	if params.Restat {
		r.Variables["restat"] = simpleNinjaString("true")
	}

	if params.Rspfile != "" {
		value, err = parseNinjaString(scope, params.Rspfile)
		if err != nil {
			return nil, fmt.Errorf("error parsing Rspfile param: %s", err)
		}
		r.Variables["rspfile"] = value
	}

	if params.RspfileContent != "" {
		value, err = parseNinjaString(scope, params.RspfileContent)
		if err != nil {
			return nil, fmt.Errorf("error parsing RspfileContent param: %s",
				err)
		}
		r.Variables["rspfile_content"] = value
	}

	r.SandboxDisabled = params.SandboxDisabled
	r.Source = params.Source
	// sandbox_disabled variable should be written to the ninja file only when
	// action sandboxing is enabled, to account for the executors that do not
	// support this variable.
	if config.(sandboxConfig).IsActionSandboxedBuild() {
		if params.SandboxDisabled {
			r.Variables["sandbox_disabled"] = simpleNinjaString("true")
		}
	}

	r.CommandDeps, err = parseNinjaStrings(scope, params.CommandDeps)
	if err != nil {
		return nil, fmt.Errorf("error parsing CommandDeps param: %s", err)
	}

	r.CommandOrderOnly, err = parseNinjaStrings(scope, params.CommandOrderOnly)
	if err != nil {
		return nil, fmt.Errorf("error parsing CommandDeps param: %s", err)
	}

	return r, nil
}

func (r *ruleDef) WriteTo(nw *ninjaWriter, name string, nameTracker *nameTracker) error {

	if r.Comment != "" {
		err := nw.Comment(r.Comment)
		if err != nil {
			return err
		}
	}

	err := nw.Rule(name)
	if err != nil {
		return err
	}

	if r.Pool != nil {
		err = nw.ScopedAssign("pool", nameTracker.Pool(r.Pool))
		if err != nil {
			return err
		}
	}

	err = writeVariables(nw, r.Variables, nameTracker)
	if err != nil {
		return err
	}

	return nil
}

// A buildDef describes a build target definition.
type buildDef struct {
	Comment               string
	Rule                  Rule
	RuleDef               *ruleDef
	Outputs               []*ninjaString
	OutputStrings         []string
	ImplicitOutputs       []*ninjaString
	ImplicitOutputStrings []string
	Inputs                []*ninjaString
	InputStrings          []string
	Implicits             []*ninjaString
	ImplicitStrings       []string
	OrderOnly             []*ninjaString
	OrderOnlyStrings      uniquelist.UniqueList[string]
	Validations           []*ninjaString
	ValidationStrings     []string
	Args                  map[Variable]*ninjaString
	Variables             map[string]*ninjaString
	Default               bool
}

func formatTags(tags map[string]string, rule Rule) string {
	// Maps in golang do not have a guaranteed iteration order, nor is there an
	// ordered map type in the stdlib, but we need to deterministically generate
	// the ninja file.
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+tags[k])
	}
	pairs = append(pairs, "rule_name="+rule.name())
	return strings.Join(pairs, ";")
}

func parseBuildParams(scope scope, params *BuildParams,
	tags map[string]string) (*buildDef, error) {

	comment := params.Comment
	rule := params.Rule

	b := &buildDef{
		Comment: comment,
		Rule:    rule,
	}

	setVariable := func(name string, value *ninjaString) {
		if b.Variables == nil {
			b.Variables = make(map[string]*ninjaString)
		}
		b.Variables[name] = value
	}

	if !scope.IsRuleVisible(rule) {
		return nil, fmt.Errorf("Rule %s is not visible in this scope", rule)
	}

	if len(params.Outputs) == 0 {
		return nil, errors.New("Outputs param has no elements")
	}

	var err error
	b.Outputs, b.OutputStrings, err = parseNinjaOrSimpleStrings(scope, params.Outputs)
	if err != nil {
		return nil, fmt.Errorf("error parsing Outputs param: %s", err)
	}

	b.ImplicitOutputs, b.ImplicitOutputStrings, err = parseNinjaOrSimpleStrings(scope, params.ImplicitOutputs)
	if err != nil {
		return nil, fmt.Errorf("error parsing ImplicitOutputs param: %s", err)
	}

	b.Inputs, b.InputStrings, err = parseNinjaOrSimpleStrings(scope, params.Inputs)
	if err != nil {
		return nil, fmt.Errorf("error parsing Inputs param: %s", err)
	}

	b.Implicits, b.ImplicitStrings, err = parseNinjaOrSimpleStrings(scope, params.Implicits)
	if err != nil {
		return nil, fmt.Errorf("error parsing Implicits param: %s", err)
	}

	var orderOnlyStrings []string
	b.OrderOnly, orderOnlyStrings, err = parseNinjaOrSimpleStrings(scope, params.OrderOnly)
	if err != nil {
		return nil, fmt.Errorf("error parsing OrderOnly param: %s", err)
	}

	b.OrderOnlyStrings = uniquelist.Make(orderOnlyStrings)

	b.Validations, b.ValidationStrings, err = parseNinjaOrSimpleStrings(scope, params.Validations)
	if err != nil {
		return nil, fmt.Errorf("error parsing Validations param: %s", err)
	}

	b.Default = params.Default

	if params.Depfile != "" {
		value, err := parseNinjaString(scope, params.Depfile)
		if err != nil {
			return nil, fmt.Errorf("error parsing Depfile param: %s", err)
		}
		setVariable("depfile", value)
	}

	if params.Deps != DepsNone {
		setVariable("deps", simpleNinjaString(params.Deps.String()))
	}

	if params.Description != "" {
		value, err := parseNinjaString(scope, params.Description)
		if err != nil {
			return nil, fmt.Errorf("error parsing Description param: %s", err)
		}
		setVariable("description", value)
	}

	if len(tags) > 0 {
		setVariable("tags", simpleNinjaString(formatTags(tags, rule)))
	}
	for name, value := range params.Variables {
		ninjaValue, err := parseNinjaString(scope, value)
		if err != nil {
			return nil, fmt.Errorf("error parsing variable %q: %s", name, err)
		}
		setVariable(name, ninjaValue)
	}

	argNameScope := rule.scope()

	if len(params.Args) > 0 {
		b.Args = make(map[Variable]*ninjaString)
		for name, value := range params.Args {
			if !rule.isArg(name) {
				return nil, fmt.Errorf("unknown argument %q", name)
			}

			argVar, err := argNameScope.LookupVariable(name)
			if err != nil {
				// This shouldn't happen.
				return nil, fmt.Errorf("argument lookup error: %s", err)
			}

			ninjaValue, err := parseNinjaString(scope, value)
			if err != nil {
				return nil, fmt.Errorf("error parsing variable %q: %s", name,
					err)
			}

			b.Args[argVar] = ninjaValue
		}
	}

	if params.PhonyOutput {
		setVariable("phony_output", simpleNinjaString("true"))
	}

	return b, nil
}

func (b *buildDef) WriteTo(nw *ninjaWriter, nameTracker *nameTracker) error {
	var (
		comment             = b.Comment
		rule                = nameTracker.Rule(b.Rule)
		outputs             = b.Outputs
		implicitOuts        = b.ImplicitOutputs
		explicitDeps        = b.Inputs
		implicitDeps        = b.Implicits
		orderOnlyDeps       = b.OrderOnly
		validations         = b.Validations
		outputStrings       = b.OutputStrings
		implicitOutStrings  = b.ImplicitOutputStrings
		explicitDepStrings  = b.InputStrings
		implicitDepStrings  = b.ImplicitStrings
		orderOnlyDepStrings = b.OrderOnlyStrings
		validationStrings   = b.ValidationStrings
	)

	if b.RuleDef != nil {
		implicitDeps = append(b.RuleDef.CommandDeps, implicitDeps...)
		orderOnlyDeps = append(b.RuleDef.CommandOrderOnly, orderOnlyDeps...)
	}

	err := nw.Build(comment, rule, outputs, implicitOuts, explicitDeps, implicitDeps, orderOnlyDeps, validations,
		outputStrings, implicitOutStrings, explicitDepStrings,
		implicitDepStrings, orderOnlyDepStrings.ToSlice(), validationStrings,
		nameTracker)
	if err != nil {
		return err
	}

	err = writeVariables(nw, b.Variables, nameTracker)
	if err != nil {
		return err
	}

	type nameValuePair struct {
		name, value string
	}

	args := make([]nameValuePair, 0, len(b.Args))

	for argVar, value := range b.Args {
		fullName := nameTracker.Variable(argVar)
		args = append(args, nameValuePair{fullName, value.Value(nameTracker)})
	}
	sort.Slice(args, func(i, j int) bool { return args[i].name < args[j].name })

	for _, pair := range args {
		err = nw.ScopedAssign(pair.name, pair.value)
		if err != nil {
			return err
		}
	}

	if b.Default {
		err = nw.Default(nameTracker, outputs, outputStrings)
		if err != nil {
			return err
		}
	}

	return nw.BlankLine()
}

func writeVariables(nw *ninjaWriter, variables map[string]*ninjaString, nameTracker *nameTracker) error {
	var keys []string
	for k := range variables {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, name := range keys {
		err := nw.ScopedAssign(name, variables[name].Value(nameTracker))
		if err != nil {
			return err
		}
	}
	return nil
}
