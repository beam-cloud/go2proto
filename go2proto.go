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
	filter      = flag.String("filter", "", "Filter by struct names. Case insensitive.")
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

	msgs := getMessages(pkgs, strings.ToLower(*filter))

	if err = writeOutput(msgs, *targetFile, *packageName); err != nil {
		log.Fatalf("error writing output: %s", err)
	}

	log.Printf("output file written to ===> %s\n", *targetFile)
}

// attempt to load all packages
func loadPackages(pwd string, pkgs []string) ([]*packages.Package, error) {
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Dir:  pwd,
		Mode: packages.LoadSyntax,
		Fset: fset,
	}
	packages, err := packages.Load(cfg, pkgs...)
	if err != nil {
		return nil, err
	}
	var errs = ""

	//check each loaded package for errors during loading
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

func getMessages(pkgs []*packages.Package, filter string) []*message {
	var out []*message
	seen := map[string]struct{}{}

	for _, p := range pkgs {
		for _, t := range p.TypesInfo.Defs {
			if t == nil {
				continue
			}

			// Check if the object is a struct and has a // @go2proto comment
			if s, ok := t.Type().Underlying().(*types.Struct); ok {
				if hasGo2ProtoComment(p.Fset, t) {
					seen[t.Name()] = struct{}{}
					if filter == "" || strings.Contains(strings.ToLower(t.Name()), filter) {
						out = appendMessage(out, t, s)
					}
				}
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Function to check if a struct has the @go2proto comment
func hasGo2ProtoComment(fset *token.FileSet, t types.Object) bool {
	pos := t.Pos() // Get the position of the object
	if !pos.IsValid() {
		return false
	}

	// Use the provided FileSet to get the position
	position := fset.Position(pos)

	// Parse the file containing the object
	file, err := parser.ParseFile(fset, position.Filename, nil, parser.ParseComments)
	if err != nil {
		log.Printf("error parsing file: %s", err)
		return false
	}

	// Traverse AST and find the struct declaration
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}

		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != t.Name() {
				continue
			}

			// Check for a @go2proto comment
			if genDecl.Doc != nil {
				for _, comment := range genDecl.Doc.List {
					if strings.Contains(comment.Text, "@go2proto") {
						return true
					}
				}
			}
		}
	}

	return false
}

func appendMessage(out []*message, t types.Object, s *types.Struct) []*message {
	msg := &message{
		Name:   t.Name(),
		Fields: []*field{},
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
	out = append(out, msg)
	return out
}

func toProtoFieldTypeName(f *types.Var) string {
	switch f.Type().Underlying().(type) {
	case *types.Basic:
		name := f.Type().String()
		return normalizeType(name)
	case *types.Slice:
		name := splitNameHelper(f)
		return normalizeType(strings.TrimLeft(name, "[]"))

	case *types.Pointer, *types.Struct:
		name := splitNameHelper(f)
		return normalizeType(name)
	}
	return f.Type().String()
}

func splitNameHelper(f *types.Var) string {
	// TODO: this is ugly. Find another way of getting field type name.
	parts := strings.Split(f.Type().String(), ".")

	name := parts[len(parts)-1]

	if name[0] == '*' {
		name = name[1:]
	}
	return name
}

func normalizeType(name string) string {
	switch name {
	case "int":
		return "int64"
	case "float32":
		return "float"
	case "float64":
		return "double"
	default:
		return name
	}
}

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

func writeOutput(msgs []*message, path string, packageName string) error {
	msgTemplate := `syntax = "proto3";
package {{.PackageName}};

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
	tmpl, err := template.New("test").Parse(msgTemplate)
	if err != nil {
		panic(err)
	}

	os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("unable to create file %s : %s", path, err)
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]interface{}{
		"PackageName": packageName,
		"Messages":    msgs,
	})
}
