package structutil

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/printer"
	"io"
	"io/ioutil"
	"regexp"

	"go/token"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/fatih/structtag"
	"golang.org/x/tools/go/packages"
)

// Heavily influenced by https://gitee.com/dwdcth/accessor

type PrinterWriter interface {
	io.Writer
	Printf(format string, args ...interface{})
}

type shadowPrinter struct {
	io.Writer

	structName string
	printf     func(structName string, format string, args ...interface{})
}

func (p *shadowPrinter) Printf(format string, args ...interface{}) {
	p.printf(p.structName, format, args...)
}

type StructInfo struct {
	Package *Package
	File    *File
	Name    string
	Fields  []StructFieldInfo
}

type GenerateForFields struct {
	toolName    string
	fileSuffix  string
	gofmtOutput bool

	genFunc func(info *StructInfo, p PrinterWriter)

	typeNames *string
	output    *string

	buf      map[string]*bytes.Buffer // Accumulated output.
	pkg      *Package                 // Package we are scanning.
	walkMark map[string]bool
}

type GenerateForFieldsConfig struct {
	ToolName    string
	FileSuffix  string
	GoFmtOutput bool
}

func NewForFieldsGenerator(c *GenerateForFieldsConfig, generator func(info *StructInfo, p PrinterWriter)) *GenerateForFields {
	return &GenerateForFields{
		toolName:    c.ToolName,
		fileSuffix:  c.FileSuffix,
		gofmtOutput: c.GoFmtOutput,

		genFunc: generator,

		buf:      make(map[string]*bytes.Buffer),
		walkMark: make(map[string]bool),
	}
}

func (g *GenerateForFields) OpinionatedPreRun() {
	log.SetFlags(0)
	log.SetPrefix(fmt.Sprintf("%s: ", g.toolName))
	flag.Usage = func() { g.Usage(os.Stderr) }

}

func (g *GenerateForFields) Usage(w io.Writer) {
	fmt.Fprintf(w, "Usage of %s:\n", g.toolName)
	fmt.Fprintf(w, "\t%s [flags] -type T [directory]\n", g.toolName)
	fmt.Fprintf(w, "\t%s [flags] -type T files... # Must be a single package\n", g.toolName)
	fmt.Fprintf(w, "Flags:\n")
	flag.PrintDefaults()
}

func (g *GenerateForFields) Init() {
	g.typeNames = flag.String("type", "", "comma-separated list of type names; must be set")
	g.output = flag.String("output", "", fmt.Sprintf("output file name; default srcdir/<type>_%s.go", g.fileSuffix))
}

func (g *GenerateForFields) Run() {
	if len(*g.typeNames) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	types := strings.Split(*g.typeNames, ",")

	// We accept either one directory or a list of files. Which do we have?
	args := flag.Args()
	if len(args) == 0 {
		// Default: process whole package in current directory.
		args = []string{"."}
	}

	// Parse the package once.
	var dir string
	if len(args) == 1 && isDirectory(args[0]) {
		dir = args[0]
	} else {
		dir = filepath.Dir(args[0])
	}
	g.parsePackage(args)

	// Print the header and package clause.
	// Run generate for each type.
	for i, typeName := range types {
		g.generate(typeName)
		// AccessWrite to file.
		outputName := *g.output
		if outputName == "" {
			baseName := fmt.Sprintf("%s_%s.go", toSnakeCase(types[i]), g.fileSuffix)
			outputName = filepath.Join(dir, strings.ToLower(baseName))
		}

		var (
			src = g.buf[typeName].Bytes()
			err error
		)
		if g.gofmtOutput {
			src, err = format.Source(src)
			if err != nil {
				log.Fatalf("formatting output: %s", err)
			}
		}

		err = ioutil.WriteFile(outputName, src, 0644)
		if err != nil {
			log.Fatalf("writing output: %s", err)
		}
	}
}

func (g *GenerateForFields) printf(structName, format string, args ...interface{}) {
	buf, ok := g.buf[structName]
	if !ok {
		buf = bytes.NewBufferString("")
		g.buf[structName] = buf
	}
	fmt.Fprintf(buf, format, args...)
}

func (g *GenerateForFields) writer(structName string) io.Writer {
	buf, ok := g.buf[structName]
	if !ok {
		buf = bytes.NewBufferString("")
		g.buf[structName] = buf
	}
	return buf
}

var matchFirstCap = regexp.MustCompile("(.)([A-Z][a-z]+)")
var matchAllCap = regexp.MustCompile("([a-z0-9])([A-Z])")

func toSnakeCase(str string) string {
	snake := matchFirstCap.ReplaceAllString(str, "${1}_${2}")
	snake = matchAllCap.ReplaceAllString(snake, "${1}_${2}")
	return strings.ToLower(snake)
}

// isDirectory reports whether the named file is a directory.
func isDirectory(name string) bool {
	info, err := os.Stat(name)
	if err != nil {
		log.Fatal(err)
	}
	return info.IsDir()
}

// File holds a single parsed file and associated data.
type File struct {
	pkg     *Package  // Package to which this file belongs.
	file    *ast.File // Parsed AST.
	fileSet *token.FileSet
	// These fields are reset for each type being generated.
	typeName string // Name of the constant type.

}

type Package struct {
	name  string
	defs  map[*ast.Ident]types.Object
	files []*File
}

func (p *Package) GetName() string {
	return p.name
}

// parsePackage analyzes the single package constructed from the patterns and tags.
// parsePackage exits if there is an error.
func (g *GenerateForFields) parsePackage(patterns []string) {
	cfg := &packages.Config{
		Mode:  packages.LoadSyntax,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		log.Fatal(err)
	}
	if len(pkgs) != 1 {
		log.Fatalf("error: %d packages found", len(pkgs))
	}
	g.addPackage(pkgs[0])
}

// addPackage adds a type checked Package and its syntax files to the generator.
func (g *GenerateForFields) addPackage(pkg *packages.Package) {
	g.pkg = &Package{
		name:  pkg.Name,
		defs:  pkg.TypesInfo.Defs,
		files: make([]*File, len(pkg.Syntax)),
	}

	for i, file := range pkg.Syntax {
		g.pkg.files[i] = &File{
			file:    file,
			pkg:     g.pkg,
			fileSet: pkg.Fset,
		}
	}
}

// generate produces the String method for the named type.
func (g *GenerateForFields) generate(typeName string) {
	for _, file := range g.pkg.files { //按包来的，读取包下的所有文件
		// Set the state for this run of the walker.
		file.typeName = typeName
		if file.file != nil {

			structInfo, err := parseStruct(file.file, file.fileSet)
			if err != nil {
				fmt.Println("failed to parse struct:" + err.Error())
				return
			}

			for stName, info := range structInfo {
				g.genFunc(&StructInfo{
					Fields:  info,
					File:    file,
					Name:    stName,
					Package: g.pkg,
				}, &shadowPrinter{
					Writer:     g.writer(stName),
					structName: stName,
					printf:     g.printf,
				})
			}

		}
	}
}

type StructFieldInfo struct {
	Name string
	Type string
	Tags *structtag.Tags
}
type StructFieldInfoArr = []StructFieldInfo

func parseStruct(file *ast.File, fileSet *token.FileSet) (structMap map[string]StructFieldInfoArr, err error) {
	structMap = make(map[string]StructFieldInfoArr)

	collectStructs := func(x ast.Node) bool {
		ts, ok := x.(*ast.TypeSpec)
		if !ok || ts.Type == nil {
			return true
		}

		structName := ts.Name.Name

		s, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		fileInfos := make([]StructFieldInfo, 0)
		for _, field := range s.Fields.List {
			name := field.Names[0].Name
			info := StructFieldInfo{Name: name}
			var typeNameBuf bytes.Buffer
			err := printer.Fprint(&typeNameBuf, fileSet, field.Type)
			if err != nil {
				fmt.Println("error:", err)
				return true
			}
			info.Type = typeNameBuf.String()
			if field.Tag != nil { // 有tag
				tag := field.Tag.Value
				tag = strings.Trim(tag, "`")
				tags, err := structtag.Parse(tag)
				if err == nil {
					info.Tags = tags
				}
			}
			fileInfos = append(fileInfos, info)
		}
		structMap[structName] = fileInfos
		return false
	}

	ast.Inspect(file, collectStructs)

	return structMap, nil
}

func genSetter(structName, fieldName, typeName string) string {
	tpl := `func ({{.Receiver}} *{{.Struct}}) Set{{.Field}}(param {{.Type}}) {
	{{.Receiver}}.{{.Field}} = param
}`
	t := template.New("setter")
	t = template.Must(t.Parse(tpl))
	res := bytes.NewBufferString("")
	t.Execute(res, map[string]string{
		"Receiver": strings.ToLower(structName[0:1]),
		"Struct":   structName,
		"Field":    fieldName,
		"Type":     typeName,
	})
	return res.String()
}

func genGetter(structName, fieldName, typeName string) string {
	tpl := `func ({{.Receiver}} *{{.Struct}}) Get{{.Field}}() {{.Type}} {
	return {{.Receiver}}.{{.Field}}
}`
	t := template.New("getter")
	t = template.Must(t.Parse(tpl))
	res := bytes.NewBufferString("")
	t.Execute(res, map[string]string{
		"Receiver": strings.ToLower(structName[0:1]),
		"Struct":   structName,
		"Field":    fieldName,
		"Type":     typeName,
	})
	return res.String()
}
