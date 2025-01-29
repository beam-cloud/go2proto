package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"

	"golang.org/x/tools/go/packages"
)

type arrFlags []string

func (i *arrFlags) String() string {
	return ""
}

func (i *arrFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

var (
	filter      = flag.String("filter", "", "Filter by struct (or type) names. Case insensitive.")
	targetFile  = flag.String("f", ".", "Protobuf output file path.")
	packageName = flag.String("n", "proto", "Package name")
	pkgFlags    arrFlags
)

func main() {
	flag.Var(&pkgFlags, "p", `Fully qualified path of packages to analyse. Relative paths ("./example/in") are allowed.`)
	flag.Parse()

	pwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("error getting working directory: %s", err)
	}

	if len(pkgFlags) == 0 {
		flag.PrintDefaults()
		os.Exit(1)
	}

	pkgs, err := loadPackages(pwd, pkgFlags)
	if err != nil {
		log.Fatalf("error fetching packages: %s", err)
	}

	// Collect both messages (from structs) and enums (from named string/int types).
	msgs, enums := getProtobufTypes(pkgs, strings.ToLower(*filter))

	if err = writeOutput(msgs, enums, *targetFile, *packageName); err != nil {
		log.Fatalf("error writing output: %s", err)
	}

	log.Printf("output file written to ===> %s\n", *targetFile)
}

// attempt to load all packages
func loadPackages(pwd string, pkgs []string) ([]*packages.Package, error) {
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Dir:  pwd,
		Mode: packages.LoadAllSyntax, // Make sure to include syntax so we have AST info
		Fset: fset,
	}
	packages, err := packages.Load(cfg, pkgs...)
	if err != nil {
		return nil, err
	}
	var errs = ""

	// check each loaded package for errors during loading
	for _, p := range packages {
		if len(p.Errors) > 0 {
			errs += fmt.Sprintf("error fetching package %s: ", p.String())
			for _, e := range p.Errors {
				errs += e.Error()
			}
			errs += "; "
		}
	}
	if errs != "" {
		return nil, errors.New(errs)
	}
	return packages, nil
}

// ----------------------------------------------------------
// Data structures for messages & enums
// ----------------------------------------------------------

type message struct {
	Name   string
	Fields []*field
}

type field struct {
	Name       string
	TypeName   string
	Order      int
	IsRepeated bool
}

// enumDef holds information about an enum name + all of its variants
type enumDef struct {
	Name   string
	Values []string
}

// ----------------------------------------------------------
// getProtobufTypes: collects messages (structs) and enums
// ----------------------------------------------------------

func getProtobufTypes(pkgs []*packages.Package, filter string) ([]*message, []*enumDef) {
	var messages []*message
	var enums []*enumDef

	// This map will track enumerations by their fully qualified name,
	// e.g. "github.com/foo/bar/pkg.ContainerStatus" => &enumDef{...}
	enumMap := make(map[string]*enumDef)

	// We do a single pass over all definitions:
	for _, p := range pkgs {
		fset := p.Fset // use the same FileSet used to parse the package

		// We need to gather constants in each package so we can match them to named types
		// We'll do this by scanning the AST (p.Syntax).
		packageConstMap := gatherConstValues(p.Syntax)

		// Now go through all definitions of named objects
		for _, def := range p.TypesInfo.Defs {
			if def == nil {
				continue
			}

			// Filter by @go2proto comment (struct or named type).
			if !hasGo2ProtoComment(fset, def) {
				continue
			}

			// If a name filter is specified, check it
			if filter != "" && !strings.Contains(strings.ToLower(def.Name()), filter) {
				continue
			}

			switch under := def.Type().Underlying().(type) {

			case *types.Struct:
				// We have a struct -> treat as a proto message
				s := under
				msg := appendMessage(def, s)
				messages = append(messages, msg)

			case *types.Basic:
				// Possibly an enum if it's a named type with underlying basic. E.g. "type X string"
				// or "type X int" or "type X int32", etc.

				// The named type is def.Type() (which is *types.Named).
				named, ok := def.Type().(*types.Named)
				if !ok {
					continue
				}

				// For an enum, gather the constants that reference this type
				enumValues := packageConstMap[def.Name()] // e.g. all constants with type ContainerStatus

				if len(enumValues) == 0 {
					// If there are no constants, we skip or just treat it as a string field.
					// But user specifically wants an enum, so we skip if no constants found.
					continue
				}

				// Create an enum definition
				ed := &enumDef{
					Name:   named.Obj().Name(), // The type's name, e.g. "ContainerStatus"
					Values: enumValues,
				}

				fullyQualified := def.Type().String() // e.g. "github.com/foo/bar/pkg.ContainerStatus"
				enumMap[fullyQualified] = ed
				enums = append(enums, ed)

			default:
				// Other underlying types could appear, but if it has @go2proto,
				// maybe user wants it as something else. For simplicity, do nothing here.
			}
		}
	}

	// Sort messages/enums for stable output
	sort.Slice(messages, func(i, j int) bool { return messages[i].Name < messages[j].Name })
	sort.Slice(enums, func(i, j int) bool { return enums[i].Name < enums[j].Name })

	// We also want to ensure that when we produce message fields referencing these enumerations,
	// they come out as the proto enum name. So we need a global reference in toProtoFieldTypeName.
	collectEnumMap(enumMap)

	return messages, enums
}

// gatherConstValues scans AST files for const blocks, capturing constants for each named type.
// For example:
//
// const (
//
//	ContainerStatusPending  ContainerStatus = "PENDING"
//	ContainerStatusRunning  ContainerStatus = "RUNNING"
//
// )
//
// This will return: map["ContainerStatus"]{"PENDING", "RUNNING"}
func gatherConstValues(files []*ast.File) map[string][]string {
	result := make(map[string][]string)
	for _, f := range files {
		for _, decl := range f.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.CONST {
				continue
			}
			for _, spec := range genDecl.Specs {
				vspec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				var typeName string
				if vspec.Type != nil {
					// If the const has an explicit type like 'ContainerStatus'
					if ident, ok := vspec.Type.(*ast.Ident); ok {
						typeName = ident.Name // e.g. "ContainerStatus"
					}
				}
				// If we have multiple names in that block, each might share the same type.
				for _, name := range vspec.Names {
					if typeName == "" && vspec.Type == nil {
						// Could be a "typed" constant from iota block, or an untyped constant.
						// If so, we can't reliably detect the enumerated type unless we do more type-checking.
						continue
					}
					// If we found a typeName, store the constant name (e.g. "ContainerStatusPending")
					// This might be "PENDING" or something else, depending on your style.
					if typeName != "" {
						result[typeName] = append(result[typeName], name.Name)
					}
				}
			}
		}
	}
	return result
}

// ----------------------------------------------------------
// We store a global map from fully-qualified name => *enumDef
// so that toProtoFieldTypeName can swap them out when needed
// ----------------------------------------------------------

var globalEnumMap = make(map[string]*enumDef)

func collectEnumMap(enumMap map[string]*enumDef) {
	for k, v := range enumMap {
		globalEnumMap[k] = v
	}
}

// ----------------------------------------------------------
// hasGo2ProtoComment uses the package's FileSet to parse AST
// for comments
// ----------------------------------------------------------

func hasGo2ProtoComment(fset *token.FileSet, t types.Object) bool {
	pos := t.Pos()
	if !pos.IsValid() {
		return false
	}

	position := fset.Position(pos)
	if position.Filename == "" {
		return false
	}

	// Parse the file containing the object
	file, err := parser.ParseFile(fset, position.Filename, nil, parser.ParseComments)
	if err != nil {
		return false
	}

	// Traverse the AST to find a declaration for t.Name()
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		switch genDecl.Tok {
		case token.TYPE:
			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if typeSpec.Name.Name == t.Name() {
					// Check if @go2proto is in the doc
					if genDecl.Doc != nil {
						for _, comment := range genDecl.Doc.List {
							if strings.Contains(comment.Text, "@go2proto") {
								return true
							}
						}
					}
				}
			}
		}
	}
	return false
}

// ----------------------------------------------------------
// Building messages from struct definitions
// ----------------------------------------------------------

func appendMessage(def types.Object, s *types.Struct) *message {
	msg := &message{
		Name:   def.Name(),
		Fields: make([]*field, 0, s.NumFields()),
	}
	for i := 0; i < s.NumFields(); i++ {
		f := s.Field(i)
		if !f.Exported() {
			continue
		}
		newField := &field{
			Name:       toProtoFieldName(f.Name()),
			TypeName:   toProtoFieldTypeName(f),
			IsRepeated: isRepeated(f),
			Order:      i + 1,
		}
		msg.Fields = append(msg.Fields, newField)
	}
	return msg
}

// ----------------------------------------------------------
// Type name resolution: Repeated logic + field type logic
// ----------------------------------------------------------

func isRepeated(f *types.Var) bool {
	_, ok := f.Type().Underlying().(*types.Slice)
	return ok
}

func toProtoFieldName(name string) string {
	if len(name) == 2 {
		return strings.ToLower(name)
	}
	r, n := utf8.DecodeRuneInString(name)
	return string(unicode.ToLower(r)) + name[n:]
}

// toProtoFieldTypeName determines how a Go type maps to a proto type name.
// This is extended to handle known enumerations in globalEnumMap.
func toProtoFieldTypeName(f *types.Var) string {
	t := f.Type()
	fullyQualified := t.String() // e.g. "github.com/beam-cloud/beta9/pkg/types.ContainerStatus"

	// If this type is a known enum, just return the enum's short name.
	if enumDef, ok := globalEnumMap[fullyQualified]; ok {
		return enumDef.Name // e.g. "ContainerStatus"
	}

	switch under := t.Underlying().(type) {
	case *types.Basic:
		// Normal int, float, string, etc.
		return normalizeType(under.String())

	case *types.Slice:
		// repeated type
		name := splitNameHelper(f)
		return normalizeType(strings.TrimLeft(name, "[]"))

	case *types.Pointer, *types.Struct:
		// pointers or embedded struct references
		name := splitNameHelper(f)
		return normalizeType(name)

	default:
		return t.String()
	}
}

// splitNameHelper extracts the last component of the type's string() representation,
// e.g. "github.com/foo/pkg.MyType" -> "MyType", removing '*' or '[]' if present.
func splitNameHelper(f *types.Var) string {
	parts := strings.Split(f.Type().String(), ".")
	name := parts[len(parts)-1]
	name = strings.TrimPrefix(name, "*")
	name = strings.TrimPrefix(name, "[]")
	return name
}

// normalizeType handles standard conversions.
func normalizeType(name string) string {
	switch name {
	case "int":
		return "int64"
	case "float32":
		return "float"
	case "float64":
		return "double"
	case "string":
		return "string"
	default:
		return name
	}
}

func writeOutput(msgs []*message, enums []*enumDef, path string, packageName string) error {
	const msgTemplate = `syntax = "proto3";
package {{.PackageName}};

// Enums
{{range .Enums}}
enum {{.Name}} {
{{- range $i, $val := .Values}}
  {{ $val }} = {{ $i }};
{{- end}}
}
{{end}}

// Messages
{{range .Messages}}
message {{.Name}} {
{{- range .Fields}}
{{- if .IsRepeated}}
  repeated {{.TypeName}} {{.Name}} = {{.Order}};
{{- else}}
  {{.TypeName}} {{.Name}} = {{.Order}};
{{- end}}
{{- end}}
}
{{end}}
`

	tmpl, err := template.New("proto-tmpl").Parse(msgTemplate)
	if err != nil {
		return fmt.Errorf("unable to parse template: %w", err)
	}

	os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("unable to create file %s: %w", path, err)
	}
	defer f.Close()

	data := map[string]interface{}{
		"PackageName": packageName,
		"Messages":    msgs,
		"Enums":       enums,
	}

	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("template.Execute error: %w", err)
	}
	return nil
}
