package obj

import (
	"encoding/binary"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"reflect"

	"golang.org/x/tools/go/buildutil"
	"honnef.co/go/tools/obj/cgo"

	"github.com/dgraph-io/badger"
	uuid "github.com/satori/go.uuid"
)

// OPT(dh): in types with elems like slices, consider storing the
// concrete underlying type together with the type ID, so that we can
// defer the actual lookup

// OPT(dh): also consider not using UUIDs for types. if the IDs were
// sequential, we could use a range query to load all referred types
// in one go. UUIDs do help with multiple tools writing to the same
// database, though.

// OPT(dh): optimize calculation of IDs (use byte slices and in-place
// modifications instead of all the Sprintf calls)

// OPT(dh): use batch sets when inserting data

// TODO(dh): add index mapping package names to import pathscd
// TODO(dh): store AST, types.Info and checksums

type Graph struct {
	curpkg string

	Fset *token.FileSet

	kv *badger.KV

	objToID map[types.Object][]byte
	typToID map[types.Type][]byte

	idToObj map[string]types.Object
	idToTyp map[string]types.Type
	idToPkg map[string]*types.Package

	// OPT(dh): merge idToPkg and pkgs
	pkgs map[string]*types.Package

	scopes map[*types.Package]map[string][]byte
	set    []*badger.Entry

	build build.Context

	checker *types.Config
}

func OpenGraph(dir string) (*Graph, error) {
	opt := badger.DefaultOptions
	opt.Dir = dir
	opt.ValueDir = dir
	kv, err := badger.NewKV(&opt)
	if err != nil {
		return nil, err
	}

	g := &Graph{
		Fset:    token.NewFileSet(),
		kv:      kv,
		objToID: map[types.Object][]byte{},
		typToID: map[types.Type][]byte{},
		idToObj: map[string]types.Object{},
		idToTyp: map[string]types.Type{},
		idToPkg: map[string]*types.Package{},
		pkgs:    map[string]*types.Package{},
		scopes:  map[*types.Package]map[string][]byte{},
		build:   build.Default,
		checker: &types.Config{},
	}
	g.checker.Importer = g

	return g, nil
}

func (g *Graph) Import(path string) (*types.Package, error) {
	panic("not implemented, use ImportFrom")
}

func (g *Graph) ImportFrom(path, srcDir string, mode types.ImportMode) (*types.Package, error) {
	bpkg, err := g.build.Import(path, srcDir, 0)
	if err != nil {
		return nil, err
	}

	if bpkg.ImportPath == "unsafe" {
		return types.Unsafe, nil
	}

	// TODO(dh): use checksum to verify that package is up to date
	if pkg, ok := g.pkgs[bpkg.ImportPath]; ok {
		return pkg, nil
	}
	if g.HasPackage(bpkg.ImportPath) {
		log.Println("importing from graph:", bpkg.ImportPath)
		pkg := g.Package(bpkg.ImportPath)
		return pkg, nil
	}

	log.Println("compiling:", bpkg.ImportPath)

	// TODO(dh): support returning partially built packages. For
	// example, an invalid AST still is usable for some operations.
	var files []*ast.File
	for _, f := range bpkg.GoFiles {
		af, err := buildutil.ParseFile(g.Fset, &g.build, nil, bpkg.Dir, f, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, af)
	}

	if len(bpkg.CgoFiles) > 0 {
		cgoFiles, err := cgo.ProcessCgoFiles(bpkg, g.Fset, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, cgoFiles...)
	}

	// TODO(dh): collect info
	info := &types.Info{}
	pkg, err := g.checker.Check(path, g.Fset, files, info)
	if err != nil {
		return nil, err
	}

	// TODO(dh): build SSA

	g.InsertPackage(bpkg, pkg)
	return pkg, nil
}

func (g *Graph) HasPackage(path string) bool {
	if path == "unsafe" {
		return true
	}
	if _, ok := g.pkgs[path]; ok {
		return true
	}
	ok, _ := g.kv.Exists([]byte(fmt.Sprintf("pkgs/%s\x00name", path)))
	return ok
}

func (g *Graph) InsertPackage(bpkg *build.Package, pkg *types.Package) {
	if pkg == types.Unsafe {
		return
	}
	if _, ok := g.pkgs[bpkg.ImportPath]; ok {
		return
	}
	log.Println("inserting", pkg)
	g.pkgs[bpkg.ImportPath] = pkg

	g.set = []*badger.Entry{}
	for _, imp := range pkg.Imports() {
		key := []byte(fmt.Sprintf("pkgs/%s\x00imports/%s", pkg.Path(), imp.Path()))
		g.set = badger.EntriesSet(g.set, key, nil)
	}

	key := []byte(fmt.Sprintf("pkgs/%s\x00name", pkg.Path()))
	g.set = badger.EntriesSet(g.set, key, []byte(pkg.Name()))

	id := []byte(fmt.Sprintf("pkgs/%s\x00scopes/%s", pkg.Path(), g.encodeScope(pkg, pkg.Scope())))
	key = []byte(fmt.Sprintf("pkgs/%s\x00scope", pkg.Path()))
	g.set = badger.EntriesSet(g.set, key, id)

	g.kv.BatchSet(g.set)
	g.set = nil
}

func (g *Graph) encodeScope(pkg *types.Package, scope *types.Scope) [16]byte {
	id := [16]byte(uuid.NewV1())

	var args [][]byte

	names := scope.Names()
	n := make([]byte, binary.MaxVarintLen64)
	l := binary.PutUvarint(n, uint64(len(names)))
	n = n[:l]
	args = append(args, n)

	for _, name := range names {
		obj := scope.Lookup(name)
		g.encodeObject(obj)
		args = append(args, g.objToID[obj])
	}

	n = make([]byte, binary.MaxVarintLen64)
	l = binary.PutUvarint(n, uint64(scope.NumChildren()))
	n = n[:l]
	args = append(args, n)

	for i := 0; i < scope.NumChildren(); i++ {
		sid := g.encodeScope(pkg, scope.Child(i))
		args = append(args, []byte(fmt.Sprintf("pkgs/%s\x00scopes/%s", pkg.Path(), sid)))
	}

	v := encodeBytes(args...)
	key := []byte(fmt.Sprintf("pkgs/%s\x00scopes/%s", pkg.Path(), id))
	g.set = badger.EntriesSet(g.set, key, v)

	return id
}

const (
	kindFunc = iota
	kindVar
	kindTypename
	kindConst
	kindPkgname

	kindSignature
	kindNamed
	kindSlice
	kindPointer
	kindInterface
	kindArray
	kindStruct
	kindTuple
	kindMap
	kindChan
)

func (g *Graph) encodeObject(obj types.Object) {
	if _, ok := g.objToID[obj]; ok {
		return
	}
	if obj.Pkg() == nil {
		g.objToID[obj] = []byte(fmt.Sprintf("builtin/%s", obj.Name()))
		return
	}
	id := uuid.NewV1()
	path := obj.Pkg().Path()
	key := []byte(fmt.Sprintf("pkgs/%s\x00objects/%s", path, [16]byte(id)))
	g.objToID[obj] = key

	g.encodeType(obj.Type())
	typID := g.typToID[obj.Type()]
	var typ byte
	switch obj.(type) {
	case *types.Func:
		typ = kindFunc
	case *types.Var:
		typ = kindVar
	case *types.TypeName:
		typ = kindTypename
	case *types.Const:
		typ = kindConst
	case *types.PkgName:
		typ = kindPkgname
	default:
		panic(fmt.Sprintf("%T", obj))
	}

	var v []byte
	switch obj := obj.(type) {
	case *types.PkgName:
		v = encodeBytes(
			[]byte(obj.Name()),
			[]byte{typ},
			typID,
			[]byte(obj.Imported().Path()),
		)
	case *types.Const:
		kind, data := encodeConstant(reflect.ValueOf(obj.Val()))
		v = encodeBytes(
			[]byte(obj.Name()),
			[]byte{typ},
			typID,
			[]byte{kind},
			data,
		)
	default:
		v = encodeBytes(
			[]byte(obj.Name()),
			[]byte{typ},
			typID,
		)
	}

	g.set = badger.EntriesSet(g.set, key, v)
}

func encodeBytes(vs ...[]byte) []byte {
	var out []byte
	num := make([]byte, binary.MaxVarintLen64)
	for _, v := range vs {
		n := binary.PutUvarint(num, uint64(len(v)))
		out = append(out, num[:n]...)
		out = append(out, v...)
	}
	return out
}

func (g *Graph) encodeType(T types.Type) {
	if id := g.typToID[T]; id != nil {
		return
	}
	if T, ok := T.(*types.Basic); ok {
		// OPT(dh): use an enum instead of strings for the built in
		// types
		g.typToID[T] = []byte(fmt.Sprintf("builtin/%s", T.Name()))
		return
	}
	id := uuid.NewV1()
	key := []byte(fmt.Sprintf("types/%s", [16]byte(id)))
	g.typToID[T] = key

	switch T := T.(type) {
	case *types.Signature:
		g.encodeType(T.Params())
		g.encodeType(T.Results())
		if T.Recv() != nil {
			g.encodeObject(T.Recv())
		}

		variadic := byte(0)
		if T.Variadic() {
			variadic = 1
		}
		params := g.typToID[T.Params()]
		results := g.typToID[T.Results()]
		recv := g.objToID[T.Recv()]

		v := encodeBytes(
			[]byte{kindSignature},
			params,
			results,
			recv,
			[]byte{variadic},
		)

		g.set = badger.EntriesSet(g.set, key, v)
	case *types.Named:
		var args [][]byte
		args = append(args, []byte{kindNamed})

		underlying := T.Underlying()
		g.encodeType(underlying)
		args = append(args, g.typToID[underlying])

		typename := T.Obj()
		g.encodeObject(typename)
		args = append(args, g.objToID[typename])

		for i := 0; i < T.NumMethods(); i++ {
			fn := T.Method(i)
			g.encodeObject(fn)
			args = append(args, g.objToID[fn])
		}
		v := encodeBytes(args...)
		g.set = badger.EntriesSet(g.set, key, v)
	case *types.Slice:
		elem := T.Elem()
		g.encodeType(elem)
		v := encodeBytes(
			[]byte{kindSlice},
			g.typToID[elem],
		)
		g.set = badger.EntriesSet(g.set, key, v)
	case *types.Pointer:
		elem := T.Elem()
		g.encodeType(elem)
		v := encodeBytes(
			[]byte{kindPointer},
			g.typToID[elem],
		)
		g.set = badger.EntriesSet(g.set, key, v)
	case *types.Interface:
		var args [][]byte
		args = append(args, []byte{kindInterface})

		n := make([]byte, binary.MaxVarintLen64)
		l := binary.PutUvarint(n, uint64(T.NumExplicitMethods()))
		args = append(args, n[:l])

		for i := 0; i < T.NumExplicitMethods(); i++ {
			fn := T.ExplicitMethod(i)
			g.encodeObject(fn)
			args = append(args, g.objToID[fn])
		}

		n = make([]byte, binary.MaxVarintLen64)
		l = binary.PutUvarint(n, uint64(T.NumEmbeddeds()))
		args = append(args, n[:l])

		for i := 0; i < T.NumEmbeddeds(); i++ {
			embedded := T.Embedded(i)
			g.encodeType(embedded)
			args = append(args, g.typToID[embedded])
		}
		v := encodeBytes(args...)
		g.set = badger.EntriesSet(g.set, key, v)
	case *types.Array:
		elem := T.Elem()
		g.encodeType(elem)

		n := make([]byte, binary.MaxVarintLen64)
		l := binary.PutUvarint(n, uint64(T.Len()))
		n = n[:l]
		v := encodeBytes(
			[]byte{kindArray},
			g.typToID[elem],
			n,
		)
		g.set = badger.EntriesSet(g.set, key, v)
	case *types.Struct:
		var args [][]byte
		args = append(args, []byte{kindStruct})
		for i := 0; i < T.NumFields(); i++ {
			field := T.Field(i)
			tag := T.Tag(i)
			g.encodeObject(field)

			args = append(args, g.objToID[field])
			args = append(args, []byte(tag))
		}
		v := encodeBytes(args...)
		g.set = badger.EntriesSet(g.set, key, v)
	case *types.Tuple:
		var args [][]byte
		args = append(args, []byte{kindTuple})
		for i := 0; i < T.Len(); i++ {
			v := T.At(i)
			g.encodeObject(v)
			args = append(args, g.objToID[v])
		}
		v := encodeBytes(args...)
		g.set = badger.EntriesSet(g.set, key, v)
	case *types.Map:
		g.encodeType(T.Key())
		g.encodeType(T.Elem())
		v := encodeBytes(
			[]byte{kindMap},
			g.typToID[T.Key()],
			g.typToID[T.Elem()],
		)
		g.set = badger.EntriesSet(g.set, key, v)
	case *types.Chan:
		g.encodeType(T.Elem())

		v := encodeBytes(
			[]byte{kindChan},
			g.typToID[T.Elem()],
			[]byte{byte(T.Dir())},
		)
		g.set = badger.EntriesSet(g.set, key, v)
	default:
		panic(fmt.Sprintf("%T", T))
	}
}

func (g *Graph) Close() error {
	return g.kv.Close()
}
