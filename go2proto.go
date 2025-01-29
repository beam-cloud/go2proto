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

	"github.com/iancoleman/strcase"
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
	msgs, _ := getProtobufTypes(pkgs, strings.ToLower(*filter))
	if err = writeOutput(msgs, *targetFile, *packageName); err != nil {
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

	// Map for enumerations: typeName -> *enumDef
	enumMap := make(map[string]*enumDef)

	// Map to track seen messages
	seenMessages := make(map[string]bool)

	// **First Pass: Collect all enum-like types**
	for _, p := range pkgs {
		fset := p.Fset
		packageConstMap := gatherConstValues(p.Syntax)

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

			// **Check if the type is a named type with a basic underlying type**
			if named, ok := def.Type().(*types.Named); ok {
				if _, ok := named.Underlying().(*types.Basic); ok {
					enumValues := packageConstMap[def.Name()]
					if len(enumValues) == 0 {
						continue
					}

					ed := &enumDef{
						Name:   named.Obj().Name(),
						Values: enumValues,
					}
					typeName := named.Obj().Name() // Use type name as the key
					enumMap[typeName] = ed
					enums = append(enums, ed)
				}
			}
		}
	}

	// **Populate the globalEnumMap with collected enums**
	collectEnumMap(enumMap)

	// **Second Pass: Process structs and their fields**
	for _, p := range pkgs {
		for _, def := range p.TypesInfo.Defs {
			if def == nil {
				continue
			}
			if !hasGo2ProtoComment(p.Fset, def) {
				continue
			}
			if filter != "" && !strings.Contains(strings.ToLower(def.Name()), filter) {
				continue
			}

			if s, ok := def.Type().Underlying().(*types.Struct); ok {
				if seenMessages[def.Name()] {
					continue
				}
				msg := appendMessage(def, s)
				messages = append(messages, msg)
				seenMessages[def.Name()] = true
			}
		}
	}

	// Sort for stable output
	sort.Slice(messages, func(i, j int) bool { return messages[i].Name < messages[j].Name })
	sort.Slice(enums, func(i, j int) bool { return enums[i].Name < enums[j].Name })

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
				if typeName == "" {
					// If there's no explicit type, skip
					continue
				}
				for i, name := range vspec.Names {
					// Default to the identifier name in case there's no assigned value
					valStr := name.Name

					// If we have a literal and it's a string, take that as the actual value
					if i < len(vspec.Values) {
						if lit, ok := vspec.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							valStr = strings.Trim(lit.Value, `"`)
						}
					}

					result[typeName] = append(result[typeName], valStr)
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

// toProtoFieldName transforms the Go field name into snake_case for proto using strcase.
func toProtoFieldName(name string) string {
	return strcase.ToSnake(name)
}

// processEnumIfAny returns the possible values of the field's type if it's recognized as an enum.
func processEnumIfAny(t types.Type) []string {
	if named, ok := t.(*types.Named); ok {
		typeName := named.Obj().Name()
		if enumDef, exists := globalEnumMap[typeName]; exists {
			return enumDef.Values
		}
	}
	return nil
}

// toProtoFieldTypeName checks the field's type; if it's recognized as an enum, treat it as a string.
func toProtoFieldTypeName(f *types.Var, fd *field) string {
	t := f.Type()

	if vals := processEnumIfAny(t); vals != nil {
		fd.EnumValues = vals
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
		if name == "Time" {
			return "google.protobuf.Timestamp"
		}
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
	case "uint":
		return "uint32"
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
func writeOutput(msgs []*message, path string, packageName string) error {
	const msgTemplate = `// Code generated by go2proto. DO NOT EDIT.
syntax = "proto3";

option go_package = "{{.PackageName}}";
import "google/protobuf/timestamp.proto";

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
