package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dave/jennifer/jen"

	"git.sr.ht/~emersion/go-varlink/varlinkdef"
)

func main() {
	var inFilename, outFilename, pkgName string
	flag.StringVar(&inFilename, "i", "", "input filename")
	flag.StringVar(&outFilename, "o", "", "output filename")
	flag.StringVar(&pkgName, "n", "", "package name")
	flag.Parse()

	if inFilename == "" {
		log.Fatal("-i is required")
	}
	if outFilename == "" {
		log.Fatal("-o is required")
	}

	if pkgName == "" {
		abs, err := filepath.Abs(outFilename)
		if err != nil {
			log.Fatalf("failed to get absolute output filename: %v", err)
		}
		pkgName = filepath.Base(filepath.Dir(abs))
	}

	iface, err := loadInterface(inFilename)
	if err != nil {
		log.Fatalf("failed to load Varlink interface definition: %v", err)
	}

	f := jen.NewFile(pkgName)

	var typeNames []string
	for name := range iface.Types {
		typeNames = append(typeNames, name)
	}
	sort.Strings(typeNames)

	for _, name := range typeNames {
		typ := iface.Types[name]
		switch typ.Kind {
		case varlinkdef.KindStruct:
			f.Type().Id(name).Add(genType(&typ))
		case varlinkdef.KindEnum:
			var defs []jen.Code
			for _, k := range typ.Enum {
				defs = append(defs, jen.Id(name+goName(k)).Id(name).Op("=").Lit(k))
			}

			f.Type().Id(name).String()
			f.Const().Defs(defs...)
		default:
			panic("unreachable")
		}
	}

	f.Line()

	var errorNames []string
	for name := range iface.Errors {
		errorNames = append(errorNames, name)
	}
	sort.Strings(errorNames)

	for _, name := range errorNames {
		err := iface.Errors[name]
		f.Type().Id(name + "Error").Add(genStruct(err))
		f.Func().Params(
			jen.Id("err").Op("*").Id(name + "Error"),
		).Id("Error").Params().String().Block(
			jen.Return().Lit(iface.Name + "." + name),
		)
	}

	f.Line()

	var methodNames []string
	for name := range iface.Methods {
		methodNames = append(methodNames, name)
	}
	sort.Strings(methodNames)

	for _, name := range methodNames {
		method := iface.Methods[name]

		f.Type().Id(name + "In").Add(genStruct(method.In))
		f.Type().Id(name + "Out").Add(genStruct(method.Out))
		f.Line()
	}

	f.Type().Id("Client").Struct(
		jen.Op("*").Qual("git.sr.ht/~emersion/go-varlink", "Client"),
	)

	f.Line()

	var errCases []jen.Code
	for _, name := range errorNames {
		errCases = append(errCases, jen.Case(
			jen.Lit(iface.Name+"."+name),
		).Block(
			jen.Id("v").Op("=").New(jen.Id(name+"Error")),
		))
	}
	errCases = append(errCases, jen.Default().Block(
		jen.Return().Id("err"),
	))

	// TODO: consider introducing a central error registry so that errors
	// coming from foreign interfaces can be used
	// TODO: consider wrapping the original varlink.Error inside the
	// unmarshalled one
	f.Func().Id("unmarshalError").Params(
		jen.Id("err").Id("error"),
	).Id("error").Block(
		jen.List(jen.Id("verr"), jen.Id("ok")).Op(":=").Id("err").Assert(jen.Op("*").Qual("git.sr.ht/~emersion/go-varlink", "ClientError")),
		jen.If(jen.Op("!").Id("ok")).Block(
			jen.Return().Id("err"),
		),
		jen.Var().Id("v").Id("error"),
		jen.Switch(jen.Id("verr").Dot("Name")).Block(errCases...),
		jen.If(
			jen.Id("err").Op(":=").Qual("encoding/json", "Unmarshal").Call(
				jen.Id("verr").Dot("Parameters"),
				jen.Id("v"),
			),
			jen.Id("err").Op("!=").Nil(),
		).Block(
			jen.Return().Id("err"),
		),
		jen.Return().Id("v"),
	)

	for _, name := range methodNames {
		f.Func().Params(
			jen.Id("c").Op("*").Id("Client"),
		).Id(name).Params(
			jen.Id("in").Op("*").Id(name+"In"),
		).Params(
			jen.Op("*").Id(name+"Out"),
			jen.Id("error"),
		).Block(
			jen.If(jen.Id("in").Op("==").Nil()).Block(
				jen.Id("in").Op("=").New(jen.Id(name+"In")),
			),
			jen.Id("out").Op(":=").New(jen.Id(name+"Out")),
			jen.Id("err").Op(":=").Id("c").Dot("Client").Dot("Do").Call(
				jen.Lit(iface.Name+"."+name),
				jen.Id("in"),
				jen.Id("out"),
			),
			jen.Return().List(
				jen.Id("out"),
				jen.Id("unmarshalError").Call(jen.Id("err")),
			),
		)
	}

	f.Line()

	var backendMethods []jen.Code
	for _, name := range methodNames {
		backendMethods = append(backendMethods, jen.Id(name).Params(
			jen.Op("*").Id(name+"In"),
		).Params(
			jen.Op("*").Id(name+"Out"),
			jen.Id("error"),
		))
	}

	f.Type().Id("Backend").Interface(backendMethods...)

	f.Line()

	f.Type().Id("Handler").Struct(
		jen.Id("Backend").Id("Backend"),
	)

	errCases = nil
	for _, name := range errorNames {
		errCases = append(errCases, jen.Case(jen.Op("*").Id(name+"Error")).Block(
			jen.Id("name").Op("=").Lit(iface.Name+"."+name),
		))
	}
	errCases = append(errCases, jen.Default().Block(
		jen.Return().Id("err"),
	))

	f.Func().Id("marshalError").Params(
		jen.Id("err").Id("error"),
	).Id("error").Block(
		jen.Var().Id("name").String(),
		jen.Switch(jen.Id("err").Assert(jen.Type())).Block(errCases...),
		jen.Return().Op("&").Qual("git.sr.ht/~emersion/go-varlink", "ServerError").Values(jen.Dict{
			jen.Id("Name"):       jen.Id("name"),
			jen.Id("Parameters"): jen.Id("err"),
		}),
	)

	var methodCases []jen.Code
	for _, name := range methodNames {
		methodCases = append(methodCases, jen.Case(jen.Lit(iface.Name+"."+name)).Block(
			jen.Id("in").Op(":=").New(jen.Id(name+"In")),
			jen.If(
				jen.Id("err").Op(":=").Qual("encoding/json", "Unmarshal").Call(
					jen.Id("req.Parameters"),
					jen.Id("in"),
				),
				jen.Id("err").Op("!=").Nil(),
			).Block(
				jen.Return().Id("err"),
			),
			jen.List(jen.Id("out"), jen.Id("err")).Op("=").Id("h").Dot("Backend").Dot(name).Call(jen.Id("in")),
		))
	}
	methodCases = append(methodCases, jen.Default().Block(
		// TODO: consider using a generated error struct
		jen.Id("err").Op("=").Op("&").Qual("git.sr.ht/~emersion/go-varlink", "ServerError").Values(jen.Dict{
			jen.Id("Name"): jen.Lit("org.varlink.service.MethodNotFound"),
			jen.Id("Parameters"): jen.Map(jen.String()).String().Values(jen.Dict{
				jen.Lit("method"): jen.Id("req").Dot("Method"),
			}),
		}),
	))

	f.Func().Params(
		jen.Id("h").Id("Handler"),
	).Id("HandleVarlink").Params(
		jen.Id("call").Op("*").Qual("git.sr.ht/~emersion/go-varlink", "ServerCall"),
		jen.Id("req").Op("*").Qual("git.sr.ht/~emersion/go-varlink", "ServerRequest"),
	).Id("error").Block(
		jen.Var().Defs(
			jen.Id("out").Interface(),
			jen.Id("err").Id("error"),
		),
		jen.Switch(jen.Id("req").Dot("Method")).Block(methodCases...),
		jen.If(jen.Id("err").Op("!=").Nil()).Block(
			jen.Return().Id("marshalError").Call(jen.Id("err")),
		),
		jen.Return().Id("call").Dot("CloseWithReply").Call(jen.Id("out")),
	)

	if err := f.Save(outFilename); err != nil {
		log.Fatal(err)
	}
}

func loadInterface(filename string) (*varlinkdef.Interface, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return varlinkdef.Read(f)
}

func genType(typ *varlinkdef.Type) jen.Code {
	if typ.Nullable {
		t := *typ
		t.Nullable = false
		return jen.Op("*").Add(genType(&t))
	}

	switch typ.Kind {
	case varlinkdef.KindStruct:
		return genStruct(typ.Struct)
	case varlinkdef.KindEnum:
		return jen.String() // TODO
	case varlinkdef.KindName:
		return jen.Id(goName(typ.Name))
	case varlinkdef.KindBool:
		return jen.Bool()
	case varlinkdef.KindInt:
		return jen.Int()
	case varlinkdef.KindFloat:
		return jen.Float64()
	case varlinkdef.KindString:
		return jen.String()
	case varlinkdef.KindObject:
		return jen.Qual("encoding/json", "RawMessage")
	case varlinkdef.KindArray:
		return jen.Index().Add(genType(typ.Inner))
	case varlinkdef.KindMap:
		return jen.Map(jen.String()).Add(genType(typ.Inner))
	default:
		panic("unreachable")
	}
}

func genStruct(def varlinkdef.Struct) jen.Code {
	var keys []string
	for k := range def {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var fields []jen.Code
	for _, k := range keys {
		t := def[k]

		tag := map[string]string{"json": k}
		if t.Nullable {
			tag["json"] += ",omitempty"
		}

		fields = append(fields, jen.Id(goName(k)).Add(genType(&t)).Tag(tag))
	}

	return jen.Struct(fields...)
}

func goName(name string) string {
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.Title(name)
	name = strings.ReplaceAll(name, " ", "")
	return name
}
