package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

const (
	inventorySchemaVersion = 1
	generatorPath          = "scripts/plugin-migration-inventory"
	coreImportPath         = "github.com/xbcio/xbc"
	pluginImportPath       = coreImportPath + "/plugin"
	webImportPath          = coreImportPath + "/transport/web"
	catalogImportPath      = pluginImportPath + "/catalog"
)

var retiredSymbols = map[string]map[string]bool{
	coreImportPath: {
		"WithDefinitions": true,
	},
	pluginImportPath: {
		"Always":               true,
		"Base":                 true,
		"Configurable":         true,
		"Configured":           true,
		"Declarer":             true,
		"Dep":                  true,
		"Deps":                 true,
		"Extension":            true,
		"Extensions":           true,
		"Get":                  true,
		"GetNamed":             true,
		"MustGet":              true,
		"MustGetNamed":         true,
		"Need":                 true,
		"NeedNamed":            true,
		"Offer":                true,
		"Opt":                  true,
		"Plugin":               true,
		"Provide":              true,
		"Provider":             true,
		"Registry":             true,
		"BindLifecycleContext": true,
	},
	webImportPath: {
		"MiddlewareProvider":   true,
		"RouteCatalogConsumer": true,
		"RouteProvider":        true,
	},
}

var retiredImportPaths = map[string]bool{
	catalogImportPath:                   true,
	coreImportPath + "/assembly":        true,
	coreImportPath + "/assembly/inject": true,
	coreImportPath + "/runtime":         true,
}

var retiredMethodNames = map[string]bool{
	"ConfigPtr":          true,
	"Dependencies":       true,
	"GoManaged":          true,
	"InitializedPlugins": true,
	"LookupValue":        true,
	"Middlewares":        true,
	"ProvideValue":       true,
	"Provides":           true,
}

// Inventory is deliberately timestamp-free: identical source trees produce
// byte-for-byte identical checked artifacts.
type Inventory struct {
	SchemaVersion             int                  `json:"schema_version"`
	Generator                 string               `json:"generator"`
	Workspace                 Workspace            `json:"workspace"`
	Summary                   Summary              `json:"summary"`
	ModuleTotals              []ModuleTotal        `json:"module_totals"`
	RetiredAPITotals          []APITotal           `json:"retired_api_totals"`
	RetiredAPIUses            []Finding            `json:"retired_api_uses"`
	ContextRetentions         []ContextRetention   `json:"context_retentions"`
	DefinitionAccessors       []DefinitionAccessor `json:"definition_accessors"`
	GenericOrderReferences    []OrderReference     `json:"generic_order_references"`
	WebLiteralOrderReferences []OrderReference     `json:"web_literal_order_references"`
	PluginPackages            []PluginPackage      `json:"plugin_packages"`
}

type Workspace struct {
	GoWork  string            `json:"go_work"`
	Modules []WorkspaceModule `json:"modules"`
}

type WorkspaceModule struct {
	Path string `json:"path"`
	Root string `json:"root"`
}

type Summary struct {
	Modules                         int `json:"modules"`
	GoFiles                         int `json:"go_files"`
	MigrationBlockers               int `json:"migration_blockers"`
	RetiredAPIUses                  int `json:"retired_api_uses"`
	ContextRetentions               int `json:"context_retentions"`
	DefinitionAccessors             int `json:"definition_accessors"`
	NoncanonicalDefinitionAccessors int `json:"noncanonical_definition_accessors"`
	GenericOrderReferences          int `json:"generic_order_references"`
	WebLiteralOrderReferences       int `json:"web_literal_order_references"`
	PluginPackages                  int `json:"plugin_packages"`
}

type ModuleTotal struct {
	Module                          string `json:"module"`
	Root                            string `json:"root"`
	GoFiles                         int    `json:"go_files"`
	MigrationBlockers               int    `json:"migration_blockers"`
	RetiredAPIUses                  int    `json:"retired_api_uses"`
	ContextRetentions               int    `json:"context_retentions"`
	DefinitionAccessors             int    `json:"definition_accessors"`
	NoncanonicalDefinitionAccessors int    `json:"noncanonical_definition_accessors"`
	GenericOrderReferences          int    `json:"generic_order_references"`
	WebLiteralOrderReferences       int    `json:"web_literal_order_references"`
	PluginPackages                  int    `json:"plugin_packages"`
}

type APITotal struct {
	API   string `json:"api"`
	Count int    `json:"count"`
}

type Finding struct {
	Module     string `json:"module"`
	Package    string `json:"package"`
	Definition string `json:"definition"`
	API        string `json:"api"`
	Kind       string `json:"kind"`
	File       string `json:"file"`
	Scope      string `json:"scope"`
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	Expression string `json:"expression,omitempty"`
}

type ContextRetention struct {
	Module     string `json:"module"`
	Package    string `json:"package"`
	Definition string `json:"definition"`
	Kind       string `json:"kind"`
	Type       string `json:"type"`
	Field      string `json:"field,omitempty"`
	File       string `json:"file"`
	Scope      string `json:"scope"`
	Line       int    `json:"line"`
	Column     int    `json:"column"`
}

type DefinitionAccessor struct {
	Module          string `json:"module"`
	Package         string `json:"package"`
	Definition      string `json:"definition"`
	File            string `json:"file"`
	Scope           string `json:"scope"`
	Line            int    `json:"line"`
	Column          int    `json:"column"`
	Canonical       bool   `json:"canonical"`
	Shape           string `json:"shape"`
	DeclarationKind string `json:"declaration_kind"`
	Expression      string `json:"expression,omitempty"`
}

type OrderReference struct {
	Module     string `json:"module"`
	Package    string `json:"package"`
	Definition string `json:"definition"`
	Domain     string `json:"domain"`
	Owner      string `json:"owner"`
	Direction  string `json:"direction"`
	Target     string `json:"target"`
	Literal    bool   `json:"literal"`
	File       string `json:"file"`
	Scope      string `json:"scope"`
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	Expression string `json:"expression"`
}

type PluginPackage struct {
	Module              string   `json:"module"`
	Package             string   `json:"package"`
	Directory           string   `json:"directory"`
	Definitions         []string `json:"definitions"`
	DefinitionAccessors []string `json:"definition_accessors"`
	Signals             []string `json:"signals"`
	Status              string   `json:"status"`
}

type moduleInfo struct {
	Path    string
	Root    string
	AbsRoot string
}

type sourceFile struct {
	module      moduleInfo
	packagePath string
	packageName string
	relativeDir string
	relative    string
	absolute    string
	test        bool
	ast         *ast.File
	imports     map[string]string
	dotImports  map[string]bool
	parents     map[ast.Node]ast.Node
}

type packageUnit struct {
	module          moduleInfo
	packagePath     string
	packageName     string
	relativeDir     string
	files           []*sourceFile
	topValues       map[string]ast.Expr
	topValueFiles   map[string]*sourceFile
	topKinds        map[string]string
	definitionKeys  map[string]bool
	definitionByVar map[string]string
	productionOwned bool
	signals         map[string]bool
}

func generateInventory(root string) (Inventory, error) {
	modules, err := discoverWorkspaceModules(root)
	if err != nil {
		return Inventory{}, err
	}
	set := token.NewFileSet()
	files, err := parseWorkspaceGoFiles(root, modules, set)
	if err != nil {
		return Inventory{}, err
	}
	inventory, err := scanWorkspace(root, modules, files, set)
	if err != nil {
		return Inventory{}, err
	}
	finalizeInventory(&inventory, modules, files)
	return inventory, nil
}

func scanWorkspace(_ string, _ []moduleInfo, files []*sourceFile, set *token.FileSet) (Inventory, error) {
	units := makePackageUnits(files)
	for _, unit := range sortedPackageUnits(units) {
		collectPackageMetadata(unit)
	}

	inventory := Inventory{}
	for _, file := range files {
		unit := units[packageUnitKey(file)]
		scanDefinitionAccessors(&inventory, file, unit, set)
		scanSourceFile(&inventory, file, unit, set)
	}
	appendPluginPackages(&inventory, sortedPackageUnits(units))
	return inventory, nil
}

func sortedPackageUnits(units map[string]*packageUnit) []*packageUnit {
	out := make([]*packageUnit, 0, len(units))
	for _, unit := range units {
		out = append(out, unit)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].module.Path != out[j].module.Path {
			return out[i].module.Path < out[j].module.Path
		}
		if out[i].packagePath != out[j].packagePath {
			return out[i].packagePath < out[j].packagePath
		}
		return out[i].packageName < out[j].packageName
	})
	return out
}

func collectPackageMetadata(unit *packageUnit) {
	for _, file := range unit.files {
		if file.test {
			continue
		}
		unit.productionOwned = true
		if file.relativeDir == "autoload" || strings.HasSuffix(file.relativeDir, "/autoload") {
			unit.signals["autoload-adapter"] = true
		}
		for _, declaration := range file.ast.Decls {
			switch declaration := declaration.(type) {
			case *ast.GenDecl:
				if declaration.Tok != token.VAR {
					continue
				}
				for _, raw := range declaration.Specs {
					value, ok := raw.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for index, name := range value.Names {
						initializer := valueForIndex(value.Values, index)
						if initializer == nil || !isDefinitionConstructor(file, initializer) {
							continue
						}
						key := definitionKey(unit, file, initializer)
						unit.definitionKeys[key] = true
						unit.definitionByVar[name.Name] = key
						unit.signals["canonical-definition"] = true
					}
				}
			case *ast.FuncDecl:
				if declaration.Recv == nil && declaration.Name.Name == "Definition" && returnsDefinition(file, declaration.Type) {
					unit.signals["definition-accessor"] = true
				}
				if declaration.Recv == nil && declaration.Name.Name == "Bundle" && returnsBundle(file, declaration.Type) {
					unit.signals["bundle"] = true
				}
			}
		}
	}
}

func valueForIndex(values []ast.Expr, index int) ast.Expr {
	if index < len(values) {
		return values[index]
	}
	if len(values) == 1 {
		return values[0]
	}
	return nil
}

func scanDefinitionAccessors(inventory *Inventory, file *sourceFile, unit *packageUnit, set *token.FileSet) {
	for _, declaration := range file.ast.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil || function.Name.Name != "Definition" || !returnsDefinition(file, function.Type) {
			continue
		}

		accessor := DefinitionAccessor{
			Module:     file.module.Path,
			Package:    file.packagePath,
			Definition: packageDefinition(unit),
			File:       file.relative,
			Scope:      scope(file.test),
			Line:       set.Position(function.Name.Pos()).Line,
			Column:     set.Position(function.Name.Pos()).Column,
			Shape:      "invalid-body",
		}
		if function.Body == nil || len(function.Body.List) != 1 {
			inventory.DefinitionAccessors = append(inventory.DefinitionAccessors, accessor)
			continue
		}
		result, ok := function.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(result.Results) != 1 {
			inventory.DefinitionAccessors = append(inventory.DefinitionAccessors, accessor)
			continue
		}

		expression := unwrapExpression(result.Results[0])
		accessor.Expression = renderNode(set, expression)
		switch expression := expression.(type) {
		case *ast.Ident:
			accessor.Shape = "package-level-identifier"
			accessor.DeclarationKind = unit.topKinds[expression.Name]
			initializer := unit.topValues[expression.Name]
			initializerFile := unit.topValueFiles[expression.Name]
			accessor.Canonical = accessor.DeclarationKind == "var" && initializer != nil && initializerFile != nil && isDefinitionConstructor(initializerFile, initializer)
			if key := unit.definitionByVar[expression.Name]; key != "" {
				accessor.Definition = key
			}
		case *ast.CallExpr:
			accessor.Shape = "constructor-call"
			accessor.DeclarationKind = "expression"
			if isDefinitionConstructor(file, expression) {
				accessor.Definition = definitionKey(unit, file, expression)
			}
		default:
			accessor.Shape = "expression"
			accessor.DeclarationKind = "expression"
		}
		inventory.DefinitionAccessors = append(inventory.DefinitionAccessors, accessor)
	}
}

func scanSourceFile(inventory *Inventory, file *sourceFile, unit *packageUnit, set *token.FileSet) {
	ast.Inspect(file.ast, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.ImportSpec:
			importPath, err := strconv.Unquote(node.Path.Value)
			if err == nil && retiredImportPaths[importPath] {
				addFinding(inventory, file, unit, set, node, importPath, "import", importPath)
			}
		case *ast.SelectorExpr:
			if importPath, symbol, ok := selectorSymbol(file, node); ok {
				switch {
				case retiredSymbols[importPath][symbol]:
					addFinding(inventory, file, unit, set, node, shortImportName(importPath)+"."+symbol, "symbol", renderNode(set, node))
				case importPath == catalogImportPath:
					addFinding(inventory, file, unit, set, node, "plugin/catalog."+symbol, "symbol", renderNode(set, node))
				}
			}
		case *ast.Ident:
			if !isBareIdentifierUse(file, node) {
				break
			}
			for importPath := range file.dotImports {
				if retiredSymbols[importPath][node.Name] {
					addFinding(inventory, file, unit, set, node, shortImportName(importPath)+"."+node.Name, "symbol", node.Name)
				}
			}
		case *ast.FuncDecl:
			if node.Recv != nil && retiredMethodNames[node.Name.Name] {
				addFinding(inventory, file, unit, set, node.Name, node.Name.Name, "method", node.Name.Name)
			}
		case *ast.Field:
			if isStructField(file, node) {
				scanStructField(inventory, file, unit, set, node)
			}
		case *ast.CompositeLit:
			if expressionIs(file, node.Type, pluginImportPath, "Definition") {
				addFinding(inventory, file, unit, set, node, "plugin.Definition literal", "definition-literal", renderNode(set, node.Type))
			}
		case *ast.KeyValueExpr:
			scanOrderField(inventory, file, unit, set, node)
		case *ast.CallExpr:
			scanWebOrderCall(inventory, file, unit, set, node)
		}
		return true
	})
}

func addFinding(inventory *Inventory, file *sourceFile, unit *packageUnit, set *token.FileSet, node ast.Node, api, kind, expression string) {
	position := set.Position(node.Pos())
	inventory.RetiredAPIUses = append(inventory.RetiredAPIUses, Finding{
		Module:     file.module.Path,
		Package:    file.packagePath,
		Definition: packageDefinition(unit),
		API:        api,
		Kind:       kind,
		File:       file.relative,
		Scope:      scope(file.test),
		Line:       position.Line,
		Column:     position.Column,
		Expression: expression,
	})
	unit.signals["retired-api"] = true
}

func scanStructField(inventory *Inventory, file *sourceFile, unit *packageUnit, set *token.FileSet, field *ast.Field) {
	fieldType := unwrapExpression(field.Type)
	if star, ok := fieldType.(*ast.StarExpr); ok {
		fieldType = unwrapExpression(star.X)
	}
	// A lifecycle Context is framework-owned state and is legitimately retained
	// by the assembly/runtime transaction. The migration invariant applies to
	// Plugin implementations: primary values must receive lifecycle context at
	// the call boundary rather than capture it in their own fields. Restrict the
	// finding to packages that declare a Definition so runtime infrastructure is
	// not mislabeled as a Plugin retention.
	pluginImplementation := unit.signals["canonical-definition"] || unit.signals["definition-accessor"]
	if pluginImplementation && (expressionIs(file, fieldType, pluginImportPath, "Context") || expressionIs(file, fieldType, pluginImportPath, "Base")) {
		kind := "plugin-context-field"
		if expressionIs(file, fieldType, pluginImportPath, "Base") {
			kind = "plugin-base-field"
		}
		name := ""
		if len(field.Names) != 0 {
			names := make([]string, len(field.Names))
			for index, fieldName := range field.Names {
				names[index] = fieldName.Name
			}
			name = strings.Join(names, ",")
		} else {
			kind = strings.TrimSuffix(kind, "-field") + "-embed"
		}
		position := set.Position(field.Pos())
		inventory.ContextRetentions = append(inventory.ContextRetentions, ContextRetention{
			Module:     file.module.Path,
			Package:    file.packagePath,
			Definition: packageDefinition(unit),
			Kind:       kind,
			Type:       renderNode(set, field.Type),
			Field:      name,
			File:       file.relative,
			Scope:      scope(file.test),
			Line:       position.Line,
			Column:     position.Column,
		})
		unit.signals["context-retention"] = true
	}

	if field.Tag == nil {
		return
	}
	raw, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return
	}
	value, ok := reflect.StructTag(raw).Lookup("xbc")
	if !ok {
		return
	}
	action := strings.TrimSpace(strings.Split(value, ",")[0])
	if action == "inject" || action == "provide" {
		addFinding(inventory, file, unit, set, field.Tag, "xbc:\""+action+"\"", "struct-tag", field.Tag.Value)
	}
}

func scanOrderField(inventory *Inventory, file *sourceFile, unit *packageUnit, set *token.FileSet, field *ast.KeyValueExpr) {
	key, ok := field.Key.(*ast.Ident)
	if !ok || key.Name != "After" && key.Name != "Before" {
		return
	}
	values, ok := unwrapExpression(field.Value).(*ast.CompositeLit)
	if !ok {
		return
	}
	direction := strings.ToLower(key.Name)

	if genericOrderField(file, field, values) {
		for _, target := range values.Elts {
			appendOrderReference(&inventory.GenericOrderReferences, file, unit, set, target, "plugin", direction)
		}
		unit.signals["generic-order"] = true
		return
	}
	if webLiteralOrderField(file, field, values) {
		for _, target := range values.Elts {
			if _, literal := stringLiteral(target); literal {
				appendOrderReference(&inventory.WebLiteralOrderReferences, file, unit, set, target, "web", direction)
			}
		}
	}
}

func genericOrderField(file *sourceFile, field *ast.KeyValueExpr, values *ast.CompositeLit) bool {
	if sliceElementIs(file, values.Type, pluginImportPath, "Key") {
		return true
	}
	for parent := file.parents[field]; parent != nil; parent = file.parents[parent] {
		literal, ok := parent.(*ast.CompositeLit)
		if !ok {
			continue
		}
		return expressionIs(file, literal.Type, pluginImportPath, "Deps")
	}
	return false
}

func webLiteralOrderField(file *sourceFile, field *ast.KeyValueExpr, values *ast.CompositeLit) bool {
	if !sliceElementIsBuiltin(values.Type, "string") {
		return false
	}
	if file.packagePath == webImportPath || importsPath(file, webImportPath) {
		return true
	}
	for parent := file.parents[field]; parent != nil; parent = file.parents[parent] {
		literal, ok := parent.(*ast.CompositeLit)
		if !ok {
			continue
		}
		if expressionIs(file, literal.Type, webImportPath, "Order") || expressionIs(file, literal.Type, webImportPath, "Middleware") || sliceElementIs(file, literal.Type, webImportPath, "Middleware") {
			return true
		}
	}
	return false
}

func scanWebOrderCall(inventory *Inventory, file *sourceFile, unit *packageUnit, set *token.FileSet, call *ast.CallExpr) {
	importPath, symbol, ok := calledSymbol(file, call.Fun)
	if !ok || importPath != webImportPath {
		return
	}
	switch symbol {
	case "Prefer", "PreferInstance", "Require", "RequireInstance":
	default:
		return
	}
	if len(call.Args) == 0 {
		return
	}
	if _, literal := stringLiteral(call.Args[0]); !literal {
		return
	}
	appendOrderReference(&inventory.WebLiteralOrderReferences, file, unit, set, call.Args[0], "web", orderDirection(file, call))
}

func appendOrderReference(targets *[]OrderReference, file *sourceFile, unit *packageUnit, set *token.FileSet, expression ast.Expr, domain, direction string) {
	position := set.Position(expression.Pos())
	target, literal := stringLiteral(expression)
	if !literal {
		target = renderNode(set, expression)
	}
	owner := packageDefinition(unit)
	*targets = append(*targets, OrderReference{
		Module:     file.module.Path,
		Package:    file.packagePath,
		Definition: owner,
		Domain:     domain,
		Owner:      owner,
		Direction:  direction,
		Target:     target,
		Literal:    literal,
		File:       file.relative,
		Scope:      scope(file.test),
		Line:       position.Line,
		Column:     position.Column,
		Expression: renderNode(set, expression),
	})
}

func orderDirection(file *sourceFile, call *ast.CallExpr) string {
	for parent := file.parents[call]; parent != nil; parent = file.parents[parent] {
		keyValue, ok := parent.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := keyValue.Key.(*ast.Ident); ok && (key.Name == "Before" || key.Name == "After") {
			return strings.ToLower(key.Name)
		}
	}
	return "unknown"
}

func appendPluginPackages(inventory *Inventory, units []*packageUnit) {
	canonicalByPackage := make(map[string][]string)
	for _, accessor := range inventory.DefinitionAccessors {
		if accessor.Scope == "production" {
			canonicalByPackage[accessor.Package] = append(canonicalByPackage[accessor.Package], "Definition")
		}
	}
	for _, unit := range units {
		if !unit.productionOwned || len(unit.signals) == 0 {
			continue
		}
		if !unit.signals["canonical-definition"] && !unit.signals["definition-accessor"] && !unit.signals["retired-api"] && !unit.signals["context-retention"] && !unit.signals["autoload-adapter"] {
			continue
		}
		status := "migrated"
		switch {
		case unit.signals["autoload-adapter"] && !unit.signals["canonical-definition"] && !unit.signals["retired-api"]:
			status = "autoload-adapter"
		case unit.signals["retired-api"] && unit.signals["canonical-definition"]:
			status = "mixed"
		case unit.signals["retired-api"] || unit.signals["context-retention"]:
			status = "legacy"
		}
		accessors := canonicalByPackage[unit.packagePath]
		sort.Strings(accessors)
		inventory.PluginPackages = append(inventory.PluginPackages, PluginPackage{
			Module:              unit.module.Path,
			Package:             unit.packagePath,
			Directory:           joinDirectory(unit.module.Root, unit.relativeDir),
			Definitions:         uniqueSorted(unit.definitionKeys),
			DefinitionAccessors: accessors,
			Signals:             uniqueSorted(unit.signals),
			Status:              status,
		})
	}
}

func returnsDefinition(file *sourceFile, function *ast.FuncType) bool {
	return function.Results != nil && len(function.Results.List) == 1 && expressionIs(file, function.Results.List[0].Type, pluginImportPath, "Definition")
}

func returnsBundle(file *sourceFile, function *ast.FuncType) bool {
	return function.Results != nil && len(function.Results.List) == 1 && expressionIs(file, function.Results.List[0].Type, pluginImportPath, "Bundle")
}

func isDefinitionConstructor(file *sourceFile, expression ast.Expr) bool {
	call, ok := unwrapExpression(expression).(*ast.CallExpr)
	if !ok {
		return false
	}
	importPath, symbol, ok := calledSymbol(file, call.Fun)
	if !ok || importPath != pluginImportPath {
		return false
	}
	return symbol == "Define" || symbol == "DefineConfigured" || symbol == "DefinePlanned"
}

func definitionKey(unit *packageUnit, file *sourceFile, expression ast.Expr) string {
	call, ok := unwrapExpression(expression).(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return file.packagePath
	}
	return resolveKeyExpression(unit, file, call.Args[0], make(map[string]bool))
}

func resolveKeyExpression(unit *packageUnit, file *sourceFile, expression ast.Expr, seen map[string]bool) string {
	expression = unwrapExpression(expression)
	if value, ok := stringLiteral(expression); ok {
		return value
	}
	if identifier, ok := expression.(*ast.Ident); ok {
		if seen[identifier.Name] {
			return identifier.Name
		}
		seen[identifier.Name] = true
		if value := unit.topValues[identifier.Name]; value != nil {
			valueFile := unit.topValueFiles[identifier.Name]
			if valueFile == nil {
				valueFile = file
			}
			return resolveKeyExpression(unit, valueFile, value, seen)
		}
	}
	if call, ok := expression.(*ast.CallExpr); ok && len(call.Args) == 1 {
		return resolveKeyExpression(unit, file, call.Args[0], seen)
	}
	return renderNode(token.NewFileSet(), expression)
}

func packageDefinition(unit *packageUnit) string {
	keys := uniqueSorted(unit.definitionKeys)
	switch len(keys) {
	case 0:
		return unit.packagePath
	case 1:
		return keys[0]
	default:
		return strings.Join(keys, ",")
	}
}

func selectorSymbol(file *sourceFile, selector *ast.SelectorExpr) (string, string, bool) {
	identifier, ok := selector.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	importPath := file.imports[identifier.Name]
	if importPath == "" {
		return "", "", false
	}
	return importPath, selector.Sel.Name, true
}

func calledSymbol(file *sourceFile, expression ast.Expr) (string, string, bool) {
	expression = unwrapExpression(expression)
	switch expression := expression.(type) {
	case *ast.IndexExpr:
		return calledSymbol(file, expression.X)
	case *ast.IndexListExpr:
		return calledSymbol(file, expression.X)
	case *ast.SelectorExpr:
		return selectorSymbol(file, expression)
	case *ast.Ident:
		if file.packagePath == pluginImportPath || file.packagePath == webImportPath || file.packagePath == coreImportPath {
			return file.packagePath, expression.Name, true
		}
		for importPath := range file.dotImports {
			if importPath == pluginImportPath || importPath == webImportPath || importPath == coreImportPath {
				return importPath, expression.Name, true
			}
		}
	}
	return "", "", false
}

func expressionIs(file *sourceFile, expression ast.Expr, importPath, name string) bool {
	expression = unwrapExpression(expression)
	switch expression := expression.(type) {
	case *ast.SelectorExpr:
		actualPath, symbol, ok := selectorSymbol(file, expression)
		return ok && actualPath == importPath && symbol == name
	case *ast.Ident:
		return expression.Name == name && (file.packagePath == importPath || file.dotImports[importPath])
	}
	return false
}

func sliceElementIs(file *sourceFile, expression ast.Expr, importPath, name string) bool {
	array, ok := unwrapExpression(expression).(*ast.ArrayType)
	return ok && expressionIs(file, array.Elt, importPath, name)
}

func sliceElementIsBuiltin(expression ast.Expr, name string) bool {
	array, ok := unwrapExpression(expression).(*ast.ArrayType)
	if !ok {
		return false
	}
	identifier, ok := unwrapExpression(array.Elt).(*ast.Ident)
	return ok && identifier.Name == name
}

func unwrapExpression(expression ast.Expr) ast.Expr {
	for {
		parenthesized, ok := expression.(*ast.ParenExpr)
		if !ok {
			return expression
		}
		expression = parenthesized.X
	}
}

func stringLiteral(expression ast.Expr) (string, bool) {
	expression = unwrapExpression(expression)
	if literal, ok := expression.(*ast.BasicLit); ok && literal.Kind == token.STRING {
		value, err := strconv.Unquote(literal.Value)
		return value, err == nil
	}
	if call, ok := expression.(*ast.CallExpr); ok && len(call.Args) == 1 {
		return stringLiteral(call.Args[0])
	}
	return "", false
}

func renderNode(set *token.FileSet, node any) string {
	var out bytes.Buffer
	if err := format.Node(&out, set, node); err != nil {
		return fmt.Sprintf("%T", node)
	}
	return out.String()
}

func isStructField(file *sourceFile, field *ast.Field) bool {
	parent := file.parents[field]
	fieldList, ok := parent.(*ast.FieldList)
	if !ok {
		return false
	}
	_, ok = file.parents[fieldList].(*ast.StructType)
	return ok
}

func isBareIdentifierUse(file *sourceFile, identifier *ast.Ident) bool {
	if identifier.Obj != nil {
		return false
	}
	switch parent := file.parents[identifier].(type) {
	case *ast.SelectorExpr:
		return parent.X != identifier && parent.Sel != identifier
	case *ast.ImportSpec, *ast.File, *ast.FuncDecl, *ast.TypeSpec, *ast.ValueSpec:
		return false
	}
	return true
}

func importsPath(file *sourceFile, target string) bool {
	if file.dotImports[target] {
		return true
	}
	for _, importPath := range file.imports {
		if importPath == target {
			return true
		}
	}
	return false
}

func shortImportName(importPath string) string {
	switch importPath {
	case coreImportPath:
		return "xbc"
	case pluginImportPath:
		return "plugin"
	case webImportPath:
		return "web"
	default:
		return path.Base(importPath)
	}
}

func discoverWorkspaceModules(root string) ([]moduleInfo, error) {
	// The workspace file is passed explicitly rather than relying on cmd.Dir
	// plus ambient discovery: a GOWORK environment variable inherited from an
	// enclosing `go test` invocation (e.g. one running against this
	// repository's own go.work) would otherwise override cmd.Dir and read the
	// wrong workspace entirely.
	workspace := filepath.Join(root, "go.work")
	command := exec.Command("go", "work", "edit", "-json", workspace)
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("go work edit -json: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("go work edit -json: %w", err)
	}
	var work struct {
		Use []struct {
			DiskPath string
		}
	}
	if err := json.Unmarshal(output, &work); err != nil {
		return nil, fmt.Errorf("decode go work edit -json: %w", err)
	}
	if len(work.Use) == 0 {
		return nil, fmt.Errorf("go.work contains no use directives")
	}
	modules := make([]moduleInfo, 0, len(work.Use))
	seenPaths := make(map[string]string, len(work.Use))
	for _, use := range work.Use {
		absolute := use.DiskPath
		if !filepath.IsAbs(absolute) {
			absolute = filepath.Join(root, filepath.FromSlash(use.DiskPath))
		}
		absolute, err = filepath.Abs(absolute)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace module %q: %w", use.DiskPath, err)
		}
		absolute = filepath.Clean(absolute)
		relative, err := filepath.Rel(root, absolute)
		if err != nil {
			return nil, fmt.Errorf("make workspace module %q relative: %w", absolute, err)
		}
		if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("workspace module %q is outside repository root; checked artifacts require repository-relative paths", absolute)
		}
		modulePath, err := readModulePath(filepath.Join(absolute, "go.mod"))
		if err != nil {
			return nil, err
		}
		if previous, duplicate := seenPaths[modulePath]; duplicate {
			return nil, fmt.Errorf("module path %q is used by both %s and %s", modulePath, previous, absolute)
		}
		seenPaths[modulePath] = absolute
		rootPath := filepath.ToSlash(relative)
		if rootPath == "" {
			rootPath = "."
		}
		modules = append(modules, moduleInfo{Path: modulePath, Root: rootPath, AbsRoot: absolute})
	}
	sort.Slice(modules, func(left, right int) bool {
		if modules[left].Path != modules[right].Path {
			return modules[left].Path < modules[right].Path
		}
		return modules[left].Root < modules[right].Root
	})
	return modules, nil
}

func readModulePath(goMod string) (string, error) {
	file, err := os.Open(goMod)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", goMod, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "module") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		modulePath := fields[1]
		if unquoted, err := strconv.Unquote(modulePath); err == nil {
			modulePath = unquoted
		}
		if modulePath == "" {
			break
		}
		return modulePath, nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan %s: %w", goMod, err)
	}
	return "", fmt.Errorf("%s has no module directive", goMod)
}

func parseWorkspaceGoFiles(root string, modules []moduleInfo, set *token.FileSet) ([]*sourceFile, error) {
	moduleRoots := make(map[string]bool, len(modules))
	for _, module := range modules {
		moduleRoots[filepath.Clean(module.AbsRoot)] = true
	}
	var files []*sourceFile
	for _, module := range modules {
		var paths []string
		err := filepath.WalkDir(module.AbsRoot, func(filePath string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if filePath != module.AbsRoot && moduleRoots[filepath.Clean(filePath)] {
					return filepath.SkipDir
				}
				switch entry.Name() {
				case ".git", "vendor":
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(entry.Name()) == ".go" {
				paths = append(paths, filePath)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk module %s: %w", module.Path, err)
		}
		sort.Strings(paths)
		for _, filePath := range paths {
			parsed, err := parser.ParseFile(set, filePath, nil, parser.ParseComments|parser.AllErrors)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", displayPath(root, filePath), err)
			}
			directory := filepath.Dir(filePath)
			relativeDir, err := filepath.Rel(module.AbsRoot, directory)
			if err != nil {
				return nil, fmt.Errorf("resolve package directory for %s: %w", filePath, err)
			}
			packagePath := module.Path
			if relativeDir != "." {
				packagePath = path.Join(module.Path, filepath.ToSlash(relativeDir))
			}
			relative, err := filepath.Rel(root, filePath)
			if err != nil {
				return nil, fmt.Errorf("resolve source path %s: %w", filePath, err)
			}
			imports, dotImports, err := importAliases(parsed)
			if err != nil {
				return nil, fmt.Errorf("read imports in %s: %w", filepath.ToSlash(relative), err)
			}
			files = append(files, &sourceFile{
				module:      module,
				packagePath: packagePath,
				packageName: parsed.Name.Name,
				relativeDir: filepath.ToSlash(relativeDir),
				relative:    filepath.ToSlash(relative),
				absolute:    filePath,
				test:        strings.HasSuffix(filePath, "_test.go"),
				ast:         parsed,
				imports:     imports,
				dotImports:  dotImports,
				parents:     buildParentMap(parsed),
			})
		}
	}
	sort.Slice(files, func(left, right int) bool { return files[left].relative < files[right].relative })
	return files, nil
}

func importAliases(file *ast.File) (map[string]string, map[string]bool, error) {
	aliases := make(map[string]string)
	dotImports := make(map[string]bool)
	for _, specification := range file.Imports {
		importPath, err := strconv.Unquote(specification.Path.Value)
		if err != nil {
			return nil, nil, err
		}
		name := path.Base(importPath)
		if specification.Name != nil {
			name = specification.Name.Name
		}
		switch name {
		case "_":
			continue
		case ".":
			dotImports[importPath] = true
		default:
			aliases[name] = importPath
		}
	}
	return aliases, dotImports, nil
}

func buildParentMap(root ast.Node) map[ast.Node]ast.Node {
	parents := make(map[ast.Node]ast.Node)
	stack := make([]ast.Node, 0, 32)
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) != 0 {
			parents[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})
	return parents
}

func marshalInventory(inventory Inventory) ([]byte, error) {
	contents, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(contents, '\n'), nil
}

func finalizeInventory(inventory *Inventory, modules []moduleInfo, files []*sourceFile) {
	sortFindings(inventory.RetiredAPIUses)
	sortContextRetentions(inventory.ContextRetentions)
	sortDefinitionAccessors(inventory.DefinitionAccessors)
	sortOrderReferences(inventory.GenericOrderReferences)
	sortOrderReferences(inventory.WebLiteralOrderReferences)
	sortPluginPackages(inventory.PluginPackages)

	apiCounts := make(map[string]int)
	for _, finding := range inventory.RetiredAPIUses {
		apiCounts[finding.API]++
	}
	apis := make([]string, 0, len(apiCounts))
	for api := range apiCounts {
		apis = append(apis, api)
	}
	sort.Strings(apis)
	inventory.RetiredAPITotals = make([]APITotal, 0, len(apis))
	for _, api := range apis {
		inventory.RetiredAPITotals = append(inventory.RetiredAPITotals, APITotal{API: api, Count: apiCounts[api]})
	}

	inventory.Workspace = Workspace{GoWork: "go.work", Modules: make([]WorkspaceModule, 0, len(modules))}
	for _, module := range modules {
		inventory.Workspace.Modules = append(inventory.Workspace.Modules, WorkspaceModule{Path: module.Path, Root: module.Root})
	}
	inventory.SchemaVersion = inventorySchemaVersion
	inventory.Generator = generatorPath
	inventory.Summary = summarizeInventory(*inventory, len(modules), len(files))
	inventory.ModuleTotals = summarizeModules(*inventory, modules, files)
}

func summarizeInventory(inventory Inventory, modules, files int) Summary {
	noncanonical := 0
	for _, accessor := range inventory.DefinitionAccessors {
		if !accessor.Canonical {
			noncanonical++
		}
	}
	blockers := len(inventory.RetiredAPIUses) + len(inventory.ContextRetentions) + noncanonical + len(inventory.GenericOrderReferences) + len(inventory.WebLiteralOrderReferences)
	return Summary{
		Modules:                         modules,
		GoFiles:                         files,
		MigrationBlockers:               blockers,
		RetiredAPIUses:                  len(inventory.RetiredAPIUses),
		ContextRetentions:               len(inventory.ContextRetentions),
		DefinitionAccessors:             len(inventory.DefinitionAccessors),
		NoncanonicalDefinitionAccessors: noncanonical,
		GenericOrderReferences:          len(inventory.GenericOrderReferences),
		WebLiteralOrderReferences:       len(inventory.WebLiteralOrderReferences),
		PluginPackages:                  len(inventory.PluginPackages),
	}
}

func summarizeModules(inventory Inventory, modules []moduleInfo, files []*sourceFile) []ModuleTotal {
	byModule := make(map[string]*ModuleTotal, len(modules))
	for _, module := range modules {
		byModule[module.Path] = &ModuleTotal{Module: module.Path, Root: module.Root}
	}
	for _, file := range files {
		byModule[file.module.Path].GoFiles++
	}
	for _, finding := range inventory.RetiredAPIUses {
		byModule[finding.Module].RetiredAPIUses++
	}
	for _, retention := range inventory.ContextRetentions {
		byModule[retention.Module].ContextRetentions++
	}
	for _, accessor := range inventory.DefinitionAccessors {
		total := byModule[accessor.Module]
		total.DefinitionAccessors++
		if !accessor.Canonical {
			total.NoncanonicalDefinitionAccessors++
		}
	}
	for _, reference := range inventory.GenericOrderReferences {
		byModule[reference.Module].GenericOrderReferences++
	}
	for _, reference := range inventory.WebLiteralOrderReferences {
		byModule[reference.Module].WebLiteralOrderReferences++
	}
	for _, pluginPackage := range inventory.PluginPackages {
		byModule[pluginPackage.Module].PluginPackages++
	}
	totals := make([]ModuleTotal, 0, len(modules))
	for _, module := range modules {
		total := byModule[module.Path]
		total.MigrationBlockers = total.RetiredAPIUses + total.ContextRetentions + total.NoncanonicalDefinitionAccessors + total.GenericOrderReferences + total.WebLiteralOrderReferences
		totals = append(totals, *total)
	}
	return totals
}

func sortFindings(findings []Finding) {
	sort.Slice(findings, func(left, right int) bool {
		a, b := findings[left], findings[right]
		return lessStrings(
			[]string{a.Module, a.Definition, a.API, a.Package, a.File, paddedInt(a.Line), paddedInt(a.Column), a.Kind, a.Expression},
			[]string{b.Module, b.Definition, b.API, b.Package, b.File, paddedInt(b.Line), paddedInt(b.Column), b.Kind, b.Expression},
		)
	})
}

func sortContextRetentions(retentions []ContextRetention) {
	sort.Slice(retentions, func(left, right int) bool {
		a, b := retentions[left], retentions[right]
		return lessStrings(
			[]string{a.Module, a.Definition, a.Package, a.File, paddedInt(a.Line), paddedInt(a.Column), a.Kind, a.Type, a.Field},
			[]string{b.Module, b.Definition, b.Package, b.File, paddedInt(b.Line), paddedInt(b.Column), b.Kind, b.Type, b.Field},
		)
	})
}

func sortDefinitionAccessors(accessors []DefinitionAccessor) {
	sort.Slice(accessors, func(left, right int) bool {
		a, b := accessors[left], accessors[right]
		return lessStrings(
			[]string{a.Module, a.Definition, a.Package, a.File, paddedInt(a.Line), paddedInt(a.Column)},
			[]string{b.Module, b.Definition, b.Package, b.File, paddedInt(b.Line), paddedInt(b.Column)},
		)
	})
}

func sortOrderReferences(references []OrderReference) {
	sort.Slice(references, func(left, right int) bool {
		a, b := references[left], references[right]
		return lessStrings(
			[]string{a.Module, a.Definition, a.Domain, a.Owner, a.Direction, a.Target, a.Package, a.File, paddedInt(a.Line), paddedInt(a.Column), a.Expression},
			[]string{b.Module, b.Definition, b.Domain, b.Owner, b.Direction, b.Target, b.Package, b.File, paddedInt(b.Line), paddedInt(b.Column), b.Expression},
		)
	})
}

func sortPluginPackages(packages []PluginPackage) {
	sort.Slice(packages, func(left, right int) bool {
		if packages[left].Module != packages[right].Module {
			return packages[left].Module < packages[right].Module
		}
		return packages[left].Package < packages[right].Package
	})
}

func lessStrings(left, right []string) bool {
	for index := range left {
		if left[index] == right[index] {
			continue
		}
		return left[index] < right[index]
	}
	return false
}

func paddedInt(value int) string { return fmt.Sprintf("%012d", value) }

func scope(test bool) string {
	if test {
		return "test"
	}
	return "production"
}

func packageUnitKey(file *sourceFile) string {
	return file.module.Path + "\x00" + file.relativeDir + "\x00" + file.packageName
}

func makePackageUnits(files []*sourceFile) map[string]*packageUnit {
	units := make(map[string]*packageUnit)
	for _, file := range files {
		key := packageUnitKey(file)
		unit := units[key]
		if unit == nil {
			unit = &packageUnit{
				module:          file.module,
				packagePath:     file.packagePath,
				packageName:     file.packageName,
				relativeDir:     file.relativeDir,
				topValues:       make(map[string]ast.Expr),
				topValueFiles:   make(map[string]*sourceFile),
				topKinds:        make(map[string]string),
				definitionKeys:  make(map[string]bool),
				definitionByVar: make(map[string]string),
				signals:         make(map[string]bool),
			}
			units[key] = unit
		}
		unit.files = append(unit.files, file)
	}
	for _, unit := range units {
		for _, file := range unit.files {
			for _, declaration := range file.ast.Decls {
				general, ok := declaration.(*ast.GenDecl)
				if !ok || general.Tok != token.CONST && general.Tok != token.VAR {
					continue
				}
				for _, raw := range general.Specs {
					value, ok := raw.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for index, name := range value.Names {
						unit.topKinds[name.Name] = strings.ToLower(general.Tok.String())
						if index < len(value.Values) {
							unit.topValues[name.Name] = value.Values[index]
							unit.topValueFiles[name.Name] = file
						} else if len(value.Values) == 1 {
							unit.topValues[name.Name] = value.Values[0]
							unit.topValueFiles[name.Name] = file
						}
					}
				}
			}
		}
	}
	return units
}

func uniqueSorted(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func containsString(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}

func joinDirectory(root, relative string) string {
	if relative == "." || relative == "" {
		return root
	}
	if root == "." {
		return relative
	}
	return path.Join(root, relative)
}

func cloneBytes(value []byte) []byte { return bytes.Clone(value) }
