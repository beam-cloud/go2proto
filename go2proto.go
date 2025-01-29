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

	// Collect types and potential enum definitions (though we'll collapse them to strings).
	msgs, enums := getProtobufTypes(pkgs, strings.ToLower(*filter))

	if err = writeOutput(msgs, enums, *targetFile, *packageName); err != nil {
		log.Fatalf("error writing output: %s", err)
	}
	log.Printf("output file written to ===> %s\n", *targetFile)
}

// loadPackages loads one or more packages and returns a slice of them.
func loadPackages(pwd string, pkgs []string) ([]*packages.Package, error) {
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Dir:  pwd,
		Mode: packages.LoadAllSyntax,
		Fset: fset,
	}

	pkgsLoaded, err := packages.Load(cfg, pkgs...)
	if err != nil {
		return nil, err
	}
	var errs = ""

	for _, p := range pkgsLoaded {
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
	return pkgsLoaded, nil
}

// message represents a proto message (one Go struct).
type message struct {
	Name   string
	Fields []*field
}

// field represents a field in a proto message.
type field struct {
	Name       string
	TypeName   string
	Order      int
	IsRepeated bool
	EnumValues []string
}

// enumDef holds information about an enum name + all of its variants (as discovered in Go).
type enumDef struct {
	Name   string
	Values []string
}

// getProtobufTypes collects both struct-based messages and named types we treat as "enums".
func getProtobufTypes(pkgs []*packages.Package, filter string) ([]*message, []*enumDef) {
	var messages []*message
	var enums []*enumDef

	// Map for enumerations: fullyQualifiedName -> *enumDef
	enumMap := make(map[string]*enumDef)

	for _, p := range pkgs {
		fset := p.Fset
		// gatherConstValues: discover which named constants belong to which type
		packageConstMap := gatherConstValues(p.Syntax)

		// check each definition in the package
		for _, def := range p.TypesInfo.Defs {
			if def == nil {
				continue
			}
			if !hasGo2ProtoComment(fset, def) {
				continue
			}
			if filter != "" && !strings.Contains(strings.ToLower(def.Name()), filter) {
				continue
			}

			switch under := def.Type().Underlying().(type) {

			case *types.Struct:
				// It's a struct => build a proto message
				s := under
				msg := appendMessage(def, s)
				messages = append(messages, msg)

			case *types.Basic:
				// A named type that might be an enum-like type: "type X string", "type X int", etc.
				named, ok := def.Type().(*types.Named)
				if !ok {
					continue
				}

				// gather any associated constants
				enumValues := packageConstMap[def.Name()]
				if len(enumValues) == 0 {
					continue
				}

				ed := &enumDef{
					Name:   named.Obj().Name(),
					Values: enumValues,
				}
				fullyQualified := def.Type().String()
				enumMap[fullyQualified] = ed
				enums = append(enums, ed)
			}
		}
	}

	// sort for stable output
	sort.Slice(messages, func(i, j int) bool { return messages[i].Name < messages[j].Name })
	sort.Slice(enums, func(i, j int) bool { return enums[i].Name < enums[j].Name })

	collectEnumMap(enumMap)
	return messages, enums
}

// gatherConstValues scans AST for const blocks, collecting any constants declared with a named type.
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
					if ident, ok := vspec.Type.(*ast.Ident); ok {
						typeName = ident.Name
					}
				}
				for _, name := range vspec.Names {
					// if no typeName, we skip
					if typeName != "" {
						result[typeName] = append(result[typeName], name.Name)
					}
				}
			}
		}
	}
	return result
}

// globalEnumMap allows us to detect if a field belongs to a recognized enum type.
var globalEnumMap = make(map[string]*enumDef)

// collectEnumMap merges the discovered "enumMap" into our global map for toProtoFieldTypeName.
func collectEnumMap(enumMap map[string]*enumDef) {
	for k, v := range enumMap {
		globalEnumMap[k] = v
	}
}

// hasGo2ProtoComment checks if the type has a "@go2proto" annotation above it.
func hasGo2ProtoComment(fset *token.FileSet, t types.Object) bool {
	pos := t.Pos()
	if !pos.IsValid() {
		return false
	}
	position := fset.Position(pos)
	if position.Filename == "" {
		return false
	}

	file, err := parser.ParseFile(fset, position.Filename, nil, parser.ParseComments)
	if err != nil {
		return false
	}

	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		if genDecl.Tok == token.TYPE {
			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if typeSpec.Name.Name == t.Name() && genDecl.Doc != nil {
					for _, comment := range genDecl.Doc.List {
						if strings.Contains(comment.Text, "@go2proto") {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// appendMessage builds a "message" object from a struct.
func appendMessage(def types.Object, s *types.Struct) *message {
	msg := &message{
		Name:   def.Name(),
		Fields: make([]*field, 0, s.NumFields()),
	}
	for i := 0; i < s.NumFields(); i++ {
		fld := s.Field(i)
		if !fld.Exported() {
			continue
		}
		fd := &field{
			Name:       toProtoFieldName(fld.Name()),
			Order:      i + 1,
			IsRepeated: isRepeated(fld),
		}

		// determine the type name (may become "string" if recognized as an enum)
		fd.TypeName = toProtoFieldTypeName(fld, fd)

		msg.Fields = append(msg.Fields, fd)
	}
	return msg
}

// isRepeated returns true if the field is a slice.
func isRepeated(f *types.Var) bool {
	_, ok := f.Type().Underlying().(*types.Slice)
	return ok
}

// toProtoFieldName transforms the Go field name into a proto-like camel-case.
func toProtoFieldName(name string) string {
	if len(name) == 2 {
		return strings.ToLower(name)
	}
	r, n := utf8.DecodeRuneInString(name)
	return string(unicode.ToLower(r)) + name[n:]
}

// processEnumIfAny returns the possible values of the field's type if it's recognized as an enum.
func processEnumIfAny(t types.Type) []string {
	fullyQualified := t.String()
	if enumDef, ok := globalEnumMap[fullyQualified]; ok {
		return enumDef.Values
	}
	return nil
}

// toProtoFieldTypeName checks the field's type; if it's recognized as an enum, treat it as a string.
func toProtoFieldTypeName(f *types.Var, fd *field) string {
	t := f.Type()

	if vals := processEnumIfAny(t); vals != nil {
		// store recognized enum values for reference
		fd.EnumValues = vals
		// but collapse the actual type to "string"
		return "string"
	}

	switch under := t.Underlying().(type) {
	case *types.Basic:
		return normalizeType(under.String())
	case *types.Slice:
		name := splitNameHelper(f)
		return normalizeType(strings.TrimLeft(name, "[]"))
	case *types.Pointer, *types.Struct:
		name := splitNameHelper(f)
		return normalizeType(name)
	default:
		return t.String()
	}
}

// splitNameHelper extracts the final portion of the type by splitting on "." and trimming "*" or "[]".
func splitNameHelper(f *types.Var) string {
	parts := strings.Split(f.Type().String(), ".")
	name := parts[len(parts)-1]
	name = strings.TrimPrefix(name, "*")
	name = strings.TrimPrefix(name, "[]")
	return name
}

// normalizeType shrinks certain root types into their proto equivalents.
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

// writeOutput produces the .proto file, omitting actual enum blocks, but adding comments above fields.
func writeOutput(msgs []*message, _ []*enumDef, path string, packageName string) error {
	const msgTemplate = `syntax = "proto3";
package {{.PackageName}};

// We are collapsing any enum-like types into plain strings.
// If a field previously recognized as an enum has known values,
// we'll show them in a comment.

{{range .Messages}}
message {{.Name}} {
{{- range .Fields}}
{{- if .EnumValues}}
// possible values: {{ range $i, $val := .EnumValues }}{{if $i}}, {{end}}{{ $val }}{{ end }}
{{- end}}
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

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create directory structure: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("unable to create file %s: %w", path, err)
	}
	defer f.Close()

	data := map[string]interface{}{
		"PackageName": packageName,
		"Messages":    msgs,
	}

	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("template.Execute error: %w", err)
	}
	return nil
}
