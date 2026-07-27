package core

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/dagql/call"
)

const (
	collectionKeysFieldName   = "keys"
	collectionListFieldName   = "list"
	collectionGetFunctionName = "get"
	collectionKeyArgName      = "key"
	collectionSubsetName      = "subset"
	collectionBatchFieldName  = "batch"
)

type collectionProjector struct {
	dag               *dagql.Server
	mod               *Module
	objectDefsByName  map[string]*TypeDef
	objectDefCache    map[string]*TypeDef
	batchTypeDefCache map[string]*TypeDef
	baseCall          *dagql.ResultCall
	baseCallErr       error
	resultIndex       int
}

func newCollectionProjector(
	ctx context.Context,
	dag *dagql.Server,
	mod *Module,
) *collectionProjector {
	projector := &collectionProjector{
		dag:               dag,
		mod:               mod,
		objectDefsByName:  make(map[string]*TypeDef, len(mod.ObjectDefs)),
		objectDefCache:    map[string]*TypeDef{},
		batchTypeDefCache: map[string]*TypeDef{},
		baseCall:          dagql.CurrentCall(ctx),
	}
	for _, defResult := range mod.ObjectDefs {
		def := defResult.Self()
		if object := objectTypeDef(def); object != nil {
			projector.objectDefsByName[object.Name] = def
		}
	}
	if projector.baseCall == nil {
		for _, defs := range []dagql.ObjectResultArray[*TypeDef]{
			mod.ObjectDefs,
			mod.InterfaceDefs,
			mod.EnumDefs,
		} {
			for _, defResult := range defs {
				baseCall, err := defResult.ResultCall()
				if err != nil {
					if projector.baseCallErr == nil {
						projector.baseCallErr = err
					}
					continue
				}
				projector.baseCall = baseCall
				projector.baseCallErr = nil
				break
			}
			if projector.baseCall != nil {
				break
			}
		}
	}
	return projector
}

func collectionProjectionResult[T dagql.Typed](
	p *collectionProjector,
	label string,
	value T,
) (dagql.ObjectResult[T], error) {
	if p.baseCallErr != nil {
		return dagql.ObjectResult[T]{}, fmt.Errorf(
			"create collection projection result %q: get module type definition call: %w",
			label,
			p.baseCallErr,
		)
	}
	p.resultIndex++
	callFrame := dagql.ChildFieldCall(
		p.baseCall,
		fmt.Sprintf("__collectionProjection%d%s", p.resultIndex, label),
		value.Type(),
	)
	if callFrame == nil {
		return dagql.ObjectResult[T]{}, fmt.Errorf(
			"create collection projection result %q: module type definition call is missing",
			label,
		)
	}
	return dagql.NewObjectResultForCall(value, p.dag, callFrame)
}

func (p *collectionProjector) projectTypeDef(typeDef *TypeDef) (*TypeDef, error) {
	if typeDef == nil {
		return nil, nil
	}

	switch typeDef.Kind {
	case TypeDefKindList:
		cp := typeDef.Clone()
		list := cp.AsList.Value.Self().Clone()
		elementTypeDef, err := p.projectTypeDef(list.ElementTypeDef.Self())
		if err != nil {
			return nil, err
		}
		list.ElementTypeDef, err = collectionProjectionResult(p, "ListElement", elementTypeDef)
		if err != nil {
			return nil, err
		}
		cp.AsList.Value, err = collectionProjectionResult(p, "List", list)
		return cp, err
	case TypeDefKindObject:
		if object := objectTypeDef(typeDef); object != nil {
			if rawDef, ok := p.objectDefsByName[object.Name]; ok {
				if rawDef.AsCollection.Valid {
					if rawDef == typeDef {
						return p.projectObjectDef(rawDef)
					}
					return p.projectCollectionRef(typeDef, rawDef)
				}
			}
			if len(object.Fields) > 0 ||
				len(object.Functions) > 0 ||
				object.Constructor.Valid {
				cp := typeDef.Clone()
				projectedObject := object.Clone()
				if err := p.projectObjectMembers(projectedObject); err != nil {
					return nil, err
				}
				var err error
				cp.AsObject.Value, err = collectionProjectionResult(p, "Object", projectedObject)
				return cp, err
			}
		}
		return typeDef.Clone(), nil
	case TypeDefKindInterface:
		cp := typeDef.Clone()
		if cp.AsInterface.Valid && cp.AsInterface.Value.Self() != nil {
			iface := cp.AsInterface.Value.Self().Clone()
			for i, fn := range iface.Functions {
				var err error
				iface.Functions[i], err = p.projectFunction(fn.Self())
				if err != nil {
					return nil, err
				}
			}
			var err error
			cp.AsInterface.Value, err = collectionProjectionResult(p, "Interface", iface)
			return cp, err
		}
		return cp, nil
	default:
		return typeDef.Clone(), nil
	}
}

func (p *collectionProjector) projectObjectDef(typeDef *TypeDef) (*TypeDef, error) {
	object := objectTypeDef(typeDef)
	if cached, ok := p.objectDefCache[object.Name]; ok {
		return cached.Clone(), nil
	}

	var projected *TypeDef
	var err error
	if typeDef.AsCollection.Valid {
		projected, err = p.projectCollectionObjectDef(typeDef)
	} else {
		projected = typeDef.Clone()
		projectedObject := object.Clone()
		err = p.projectObjectMembers(projectedObject)
		if err == nil {
			projected.AsObject.Value, err = collectionProjectionResult(p, "ObjectDef", projectedObject)
		}
	}
	if err != nil {
		return nil, err
	}

	p.objectDefCache[object.Name] = projected.Clone()
	return projected, nil
}

func (p *collectionProjector) projectObjectMembers(obj *ObjectTypeDef) error {
	for i, field := range obj.Fields {
		var err error
		obj.Fields[i], err = p.projectField(field.Self())
		if err != nil {
			return err
		}
	}
	for i, fn := range obj.Functions {
		var err error
		obj.Functions[i], err = p.projectFunction(fn.Self())
		if err != nil {
			return err
		}
	}
	if obj.Constructor.Valid && obj.Constructor.Value.Self() != nil {
		constructor, err := p.projectFunction(obj.Constructor.Value.Self())
		if err != nil {
			return err
		}
		obj.Constructor.Value = constructor
	}
	return nil
}

func (p *collectionProjector) projectField(
	field *FieldTypeDef,
) (dagql.ObjectResult[*FieldTypeDef], error) {
	cp := field.Clone()
	typeDef, err := p.projectTypeDef(cp.TypeDef.Self())
	if err != nil {
		return dagql.ObjectResult[*FieldTypeDef]{}, err
	}
	cp.TypeDef, err = collectionProjectionResult(p, "FieldType", typeDef)
	if err != nil {
		return dagql.ObjectResult[*FieldTypeDef]{}, err
	}
	return collectionProjectionResult(p, "Field", cp)
}

func (p *collectionProjector) projectFunction(
	fn *Function,
) (dagql.ObjectResult[*Function], error) {
	cp := fn.Clone()
	returnType, err := p.projectTypeDef(cp.ReturnType.Self())
	if err != nil {
		return dagql.ObjectResult[*Function]{}, err
	}
	cp.ReturnType, err = collectionProjectionResult(p, "FunctionReturn", returnType)
	if err != nil {
		return dagql.ObjectResult[*Function]{}, err
	}
	for i, argResult := range cp.Args {
		arg := argResult.Self().Clone()
		argTypeDef, err := p.projectTypeDef(arg.TypeDef.Self())
		if err != nil {
			return dagql.ObjectResult[*Function]{}, err
		}
		arg.TypeDef, err = collectionProjectionResult(p, "FunctionArgType", argTypeDef)
		if err != nil {
			return dagql.ObjectResult[*Function]{}, err
		}
		cp.Args[i], err = collectionProjectionResult(p, "FunctionArg", arg)
		if err != nil {
			return dagql.ObjectResult[*Function]{}, err
		}
	}
	return collectionProjectionResult(p, "Function", cp)
}

func (p *collectionProjector) projectCollectionRef(
	typeDef *TypeDef,
	rawDef *TypeDef,
) (*TypeDef, error) {
	cp := typeDef.Clone()
	metadata, err := p.projectCollectionMetadata(rawDef)
	if err != nil {
		return nil, err
	}
	cp.AsCollection = dagql.NonNull(metadata)
	object := objectTypeDef(typeDef)
	if len(object.Fields) > 0 ||
		len(object.Functions) > 0 ||
		object.Constructor.Valid {
		projected, err := p.projectObjectDef(rawDef)
		if err != nil {
			return nil, err
		}
		cp.AsObject.Value = projected.AsObject.Value
	}
	return cp, nil
}

func (p *collectionProjector) projectCollectionMetadata(
	typeDef *TypeDef,
) (*CollectionTypeDef, error) {
	cp := typeDef.AsCollection.Value.Clone()
	var err error
	cp.KeyType, err = p.projectTypeDef(cp.KeyType)
	if err != nil {
		return nil, err
	}
	cp.ValueType, err = p.projectTypeDef(cp.ValueType)
	if err != nil {
		return nil, err
	}
	cp.BatchType = nil
	if batchTypeDef, err := p.projectCollectionBatchTypeDef(typeDef); err != nil {
		return nil, err
	} else if batchTypeDef != nil {
		cp.BatchType, err = p.collectionBatchTypeRef(typeDef)
		if err != nil {
			return nil, err
		}
	}
	return cp, nil
}

func (p *collectionProjector) collectionBatchTypeName(typeDef *TypeDef) string {
	return objectTypeDef(typeDef).Name + "_Batch"
}

func (p *collectionProjector) collectionBatchTypeRef(
	typeDef *TypeDef,
) (*TypeDef, error) {
	batchTypeDef, err := p.projectCollectionBatchTypeDef(typeDef)
	if err != nil {
		return nil, err
	}
	if batchTypeDef == nil {
		return nil, nil
	}
	batchTypeObject := objectTypeDef(batchTypeDef)
	batchObj := NewObjectTypeDef(
		batchTypeObject.Name,
		batchTypeObject.Description,
		batchTypeObject.Deprecated,
	)
	if batchTypeObject.SourceMap.Valid {
		batchObj = batchObj.WithSourceMap(batchTypeObject.SourceMap.Value)
	}
	batchObj.Name = batchTypeObject.Name
	batchObj.OriginalName = batchTypeObject.OriginalName
	batchObjResult, err := collectionProjectionResult(p, "BatchRefObject", batchObj)
	if err != nil {
		return nil, err
	}
	return (&TypeDef{}).WithObject(batchObjResult), nil
}

func (p *collectionProjector) projectCollectionBatchTypeDef(
	typeDef *TypeDef,
) (*TypeDef, error) {
	if !typeDef.AsCollection.Valid {
		return nil, nil
	}
	typeName := p.collectionBatchTypeName(typeDef)
	if cached, ok := p.batchTypeDefCache[typeName]; ok {
		return cached.Clone(), nil
	}

	batchFns := p.collectionBatchFunctions(typeDef)
	if len(batchFns) == 0 {
		return nil, nil
	}

	rawObj := objectTypeDef(typeDef)
	batchObj := NewObjectTypeDef(
		typeName,
		"Type-specific efficient operations over the current subset.",
		nil,
	)
	if rawObj.SourceMap.Valid {
		batchObj = batchObj.WithSourceMap(rawObj.SourceMap.Value)
	}
	batchObj.Name = typeName
	batchObj.OriginalName = typeName
	for _, fn := range batchFns {
		projected, err := p.projectFunction(fn)
		if err != nil {
			return nil, err
		}
		batchObj.Functions = append(batchObj.Functions, projected)
	}
	batchObjResult, err := collectionProjectionResult(p, "BatchObject", batchObj)
	if err != nil {
		return nil, err
	}

	batchTypeDef := (&TypeDef{}).WithObject(batchObjResult)
	p.batchTypeDefCache[typeName] = batchTypeDef.Clone()
	return batchTypeDef, nil
}

func (p *collectionProjector) projectCollectionObjectDef(
	typeDef *TypeDef,
) (*TypeDef, error) {
	rawObj := objectTypeDef(typeDef)
	collection := typeDef.AsCollection.Value

	projectedObj := rawObj.Clone()
	projectedObj.Fields = nil
	projectedObj.Functions = nil
	projectedObj.Constructor = dagql.Null[dagql.ObjectResult[*Function]]()

	keysField, ok := rawObj.FieldByName(collection.KeysFieldName)
	if !ok {
		panic(fmt.Sprintf(
			"collection keys field %q missing from %q",
			collection.KeysFieldName,
			rawObj.Name,
		))
	}
	projectedKeys := keysField.Clone()
	projectedKeys.Name = collectionKeysFieldName
	projectedKeys.OriginalName = collectionKeysFieldName
	projectedKeyType, err := p.projectTypeDef(keysField.TypeDef.Self())
	if err != nil {
		return nil, err
	}
	projectedKeys.TypeDef, err = collectionProjectionResult(p, "KeysType", projectedKeyType)
	if err != nil {
		return nil, err
	}
	projectedKeysResult, err := collectionProjectionResult(p, "KeysField", projectedKeys)
	if err != nil {
		return nil, err
	}
	projectedObj.Fields = append(projectedObj.Fields, projectedKeysResult)

	listTypeDef, err := p.projectedListTypeDef(collection.ValueType)
	if err != nil {
		return nil, err
	}
	listTypeDefResult, err := collectionProjectionResult(p, "ListFieldType", listTypeDef)
	if err != nil {
		return nil, err
	}
	listFieldResult, err := collectionProjectionResult(p, "ListField", &FieldTypeDef{
		Name:         collectionListFieldName,
		OriginalName: collectionListFieldName,
		Description:  "Items in the current subset, in the same order as `keys`.",
		TypeDef:      listTypeDefResult,
	})
	if err != nil {
		return nil, err
	}
	projectedObj.Fields = append(projectedObj.Fields, listFieldResult)

	getFn, ok := rawObj.FunctionByName(collection.GetFunctionName)
	if !ok {
		panic(fmt.Sprintf(
			"collection get function %q missing from %q",
			collection.GetFunctionName,
			rawObj.Name,
		))
	}
	projectedGet := getFn.Clone()
	projectedGet.Name = collectionGetFunctionName
	projectedGet.OriginalName = collectionGetFunctionName
	projectedGetReturn, err := p.projectTypeDef(getFn.ReturnType.Self())
	if err != nil {
		return nil, err
	}
	projectedGet.ReturnType, err = collectionProjectionResult(p, "GetReturn", projectedGetReturn)
	if err != nil {
		return nil, err
	}
	projectedGetArg := getFn.Args[0].Self().Clone()
	projectedGetArg.Name = collectionKeyArgName
	projectedGetArg.OriginalName = collectionKeyArgName
	projectedGetArgType, err := p.projectTypeDef(projectedGetArg.TypeDef.Self())
	if err != nil {
		return nil, err
	}
	projectedGetArg.TypeDef, err = collectionProjectionResult(p, "GetArgType", projectedGetArgType)
	if err != nil {
		return nil, err
	}
	projectedGet.Args[0], err = collectionProjectionResult(p, "GetArg", projectedGetArg)
	if err != nil {
		return nil, err
	}
	projectedGetResult, err := collectionProjectionResult(p, "GetFunction", projectedGet)
	if err != nil {
		return nil, err
	}
	projectedObj.Functions = append(projectedObj.Functions, projectedGetResult)

	subsetKeysType, err := p.projectedListTypeDef(collection.KeyType)
	if err != nil {
		return nil, err
	}
	subsetKeysTypeResult, err := collectionProjectionResult(p, "SubsetKeysType", subsetKeysType)
	if err != nil {
		return nil, err
	}
	subsetArgResult, err := collectionProjectionResult(p, "SubsetArg", &FunctionArg{
		Name:         collectionKeysFieldName,
		OriginalName: collectionKeysFieldName,
		Description:  "Keys to retain from the current subset.",
		TypeDef:      subsetKeysTypeResult,
	})
	if err != nil {
		return nil, err
	}
	collectionRefObject := NewObjectTypeDef(rawObj.Name, "", nil)
	collectionRefObject.Name = rawObj.Name
	collectionRefObject.OriginalName = rawObj.OriginalName
	collectionRefObjectResult, err := collectionProjectionResult(p, "SubsetReturnObject", collectionRefObject)
	if err != nil {
		return nil, err
	}
	collectionMetadata, err := p.projectCollectionMetadata(typeDef)
	if err != nil {
		return nil, err
	}
	subsetReturnTypeDef := (&TypeDef{}).WithObject(collectionRefObjectResult)
	subsetReturnTypeDef.AsCollection = dagql.NonNull(collectionMetadata)
	subsetReturnResult, err := collectionProjectionResult(p, "SubsetReturn", subsetReturnTypeDef)
	if err != nil {
		return nil, err
	}
	subsetResult, err := collectionProjectionResult(p, "SubsetFunction", &Function{
		Name:         collectionSubsetName,
		OriginalName: collectionSubsetName,
		Description:  "Restrict the collection to an exact subset of keys.",
		Args:         dagql.ObjectResultArray[*FunctionArg]{subsetArgResult},
		ReturnType:   subsetReturnResult,
	})
	if err != nil {
		return nil, err
	}
	projectedObj.Functions = append(projectedObj.Functions, subsetResult)

	if batchTypeRef, err := p.collectionBatchTypeRef(typeDef); err != nil {
		return nil, err
	} else if batchTypeRef != nil {
		batchTypeResult, err := collectionProjectionResult(p, "BatchFieldType", batchTypeRef)
		if err != nil {
			return nil, err
		}
		batchFieldResult, err := collectionProjectionResult(p, "BatchField", &FieldTypeDef{
			Name:         collectionBatchFieldName,
			OriginalName: collectionBatchFieldName,
			Description:  "Type-specific efficient operations over the current subset.",
			TypeDef:      batchTypeResult,
		})
		if err != nil {
			return nil, err
		}
		projectedObj.Fields = append(projectedObj.Fields, batchFieldResult)
	}

	projectedObjResult, err := collectionProjectionResult(p, "CollectionObject", projectedObj)
	if err != nil {
		return nil, err
	}
	projectedMetadata, err := p.projectCollectionMetadata(typeDef)
	if err != nil {
		return nil, err
	}
	projectedTypeDef := (&TypeDef{}).WithObject(projectedObjResult)
	projectedTypeDef.AsCollection = dagql.NonNull(projectedMetadata)
	return projectedTypeDef, nil
}

func (p *collectionProjector) projectedListTypeDef(element *TypeDef) (*TypeDef, error) {
	projectedElement, err := p.projectTypeDef(element)
	if err != nil {
		return nil, err
	}
	elementResult, err := collectionProjectionResult(p, "ListElementType", projectedElement)
	if err != nil {
		return nil, err
	}
	listResult, err := collectionProjectionResult(p, "ListType", &ListTypeDef{
		ElementTypeDef: elementResult,
	})
	if err != nil {
		return nil, err
	}
	return (&TypeDef{}).WithListOf(listResult), nil
}

func (p *collectionProjector) collectionBatchFunctions(typeDef *TypeDef) []*Function {
	if !typeDef.AsCollection.Valid {
		return nil
	}
	collection := typeDef.AsCollection.Value
	object := objectTypeDef(typeDef)
	fns := make([]*Function, 0, len(object.Functions))
	for _, fnResult := range object.Functions {
		fn := fnResult.Self()
		if fn.Name == collection.GetFunctionName {
			continue
		}
		fns = append(fns, fn)
	}
	return fns
}

type CollectionBatchObject struct {
	Module         dagql.ObjectResult[*Module]
	TypeDef        *ObjectTypeDef
	BackingTypeDef *ObjectTypeDef
	Collection     *CollectionTypeDef
	Fields         map[string]any
}

func (obj *CollectionBatchObject) Type() *ast.Type {
	return &ast.Type{NamedType: obj.TypeDef.Name, NonNull: true}
}

func (obj *CollectionBatchObject) TypeDescription() string {
	return formatGqlDescription(obj.TypeDef.Description)
}

func (obj *CollectionBatchObject) TypeDefinition(view call.View) *ast.Definition {
	def := &ast.Definition{Kind: ast.Object, Name: obj.Type().Name()}
	if obj.TypeDef.SourceMap.Valid {
		def.Directives = append(def.Directives, obj.TypeDef.SourceMap.Value.Self().TypeDirective())
	}
	return def
}

func (obj *CollectionBatchObject) Install(ctx context.Context, dag *dagql.Server) error {
	classOpts := dagql.ClassOpts[*CollectionBatchObject]{Typed: obj}
	var installDirectives []*ast.Directive
	if obj.TypeDef.SourceMap.Valid {
		classOpts.SourceMap = obj.TypeDef.SourceMap.Value.Self().TypeDirective()
		installDirectives = append(
			installDirectives,
			obj.TypeDef.SourceMap.Value.Self().TypeDirective(),
		)
	}

	class := dagql.NewClass(dag, classOpts)
	fields, err := obj.functions(ctx, dag)
	if err != nil {
		return err
	}
	class.Install(fields...)
	dag.InstallObject(class, installDirectives...)
	return nil
}

func (obj *CollectionBatchObject) functions(
	ctx context.Context,
	dag *dagql.Server,
) ([]dagql.Field[*CollectionBatchObject], error) {
	projector := newCollectionProjector(ctx, dag, obj.Module.Self())
	backingObjectResult, err := collectionProjectionResult(
		projector,
		"BatchBackingObject",
		obj.BackingTypeDef,
	)
	if err != nil {
		return nil, err
	}
	backingTypeDef := (&TypeDef{}).WithObject(backingObjectResult)
	backingTypeDef.AsCollection = dagql.NonNull(obj.Collection)
	batchFns := projector.collectionBatchFunctions(backingTypeDef)
	fields := make([]dagql.Field[*CollectionBatchObject], 0, len(batchFns))
	for _, fn := range batchFns {
		modFun, err := NewModFunction(ctx, obj.Module, obj.BackingTypeDef, fn)
		if err != nil {
			return nil, fmt.Errorf("failed to create batch function %q: %w", fn.Name, err)
		}
		if err := modFun.mergeUserDefaultsTypeDefs(ctx); err != nil {
			return nil, fmt.Errorf("failed to merge user defaults for %q: %w", fn.Name, err)
		}
		spec, err := modFun.metadata.FieldSpec(ctx, NewUserMod(obj.Module))
		if err != nil {
			return nil, fmt.Errorf(
				"failed to get field spec for batch function %q: %w",
				fn.Name,
				err,
			)
		}
		moduleID, err := NewUserMod(obj.Module).ResultCallModule(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve module identity for batch function %q: %w", fn.Name, err)
		}
		spec.Module = moduleID
		spec.GetDynamicInput = modFun.DynamicInputsForCall
		spec.ImplicitInputs = append(spec.ImplicitInputs, modFun.cacheImplicitInputs()...)

		fields = append(fields, dagql.Field[*CollectionBatchObject]{
			Spec: &spec,
			Func: func(
				ctx context.Context,
				batch dagql.ObjectResult[*CollectionBatchObject],
				args map[string]dagql.Input,
				view call.View,
			) (dagql.AnyResult, error) {
				parent, err := dagql.NewResultForCurrentCall(ctx, &ModuleObject{
					Module:     batch.Self().Module,
					TypeDef:    batch.Self().BackingTypeDef,
					Collection: batch.Self().Collection,
					Fields:     batch.Self().Fields,
				})
				if err != nil {
					return nil, err
				}

				opts := &CallOpts{
					ParentTyped:  parent,
					ParentFields: batch.Self().Fields,
					Server:       dag,
				}
				for name, val := range args {
					opts.Inputs = append(opts.Inputs, CallInput{Name: name, Value: val})
				}
				slices.SortFunc(opts.Inputs, func(a, b CallInput) int {
					switch {
					case a.Name < b.Name:
						return -1
					case a.Name > b.Name:
						return 1
					default:
						return 0
					}
				})
				return modFun.Call(ctx, opts)
			},
		})
	}
	return fields, nil
}

func (obj *ModuleObject) collectionMembers(
	ctx context.Context,
	dag *dagql.Server,
) ([]dagql.Field[*ModuleObject], error) {
	projector := newCollectionProjector(ctx, dag, obj.Module.Self())
	backingObjectResult, err := collectionProjectionResult(
		projector,
		"CollectionBackingObject",
		obj.TypeDef,
	)
	if err != nil {
		return nil, err
	}
	backingTypeDef := (&TypeDef{}).WithObject(backingObjectResult)
	backingTypeDef.AsCollection = dagql.NonNull(obj.Collection)
	if batchTypeDef, err := projector.projectCollectionBatchTypeDef(backingTypeDef); err != nil {
		return nil, err
	} else if batchTypeDef != nil {
		batchObj := &CollectionBatchObject{
			Module:         obj.Module,
			TypeDef:        objectTypeDef(batchTypeDef),
			BackingTypeDef: obj.TypeDef,
			Collection:     obj.Collection,
		}
		if err := batchObj.Install(ctx, dag); err != nil {
			return nil, err
		}
	}

	keysField, ok := obj.TypeDef.FieldByName(obj.Collection.KeysFieldName)
	if !ok {
		return nil, fmt.Errorf(
			"collection keys field %q not found on %q",
			obj.Collection.KeysFieldName,
			obj.TypeDef.Name,
		)
	}
	getFn, ok := obj.TypeDef.FunctionByName(obj.Collection.GetFunctionName)
	if !ok {
		return nil, fmt.Errorf(
			"collection get function %q not found on %q",
			obj.Collection.GetFunctionName,
			obj.TypeDef.Name,
		)
	}

	keyModType, ok, err := NewUserMod(obj.Module).ModTypeFor(ctx, obj.Collection.KeyType, true)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("could not resolve key type %s", obj.Collection.KeyType.ToType())
	}

	getModFun, err := NewModFunction(ctx, obj.Module, obj.TypeDef, getFn)
	if err != nil {
		return nil, fmt.Errorf("failed to create get function: %w", err)
	}
	if err := getModFun.mergeUserDefaultsTypeDefs(ctx); err != nil {
		return nil, fmt.Errorf("failed to merge user defaults for get: %w", err)
	}

	projectedGetResult, err := projector.projectFunction(getFn)
	if err != nil {
		return nil, err
	}
	projectedGetFn := projectedGetResult.Self().Clone()
	projectedGetFn.Name = collectionGetFunctionName
	projectedGetFn.OriginalName = collectionGetFunctionName
	projectedGetArg := projectedGetFn.Args[0].Self().Clone()
	projectedGetArg.Name = collectionKeyArgName
	projectedGetArg.OriginalName = collectionKeyArgName
	projectedGetFn.Args[0], err = collectionProjectionResult(
		projector,
		"RuntimeGetArg",
		projectedGetArg,
	)
	if err != nil {
		return nil, err
	}
	getSpec, err := projectedGetFn.FieldSpec(ctx, NewUserMod(obj.Module))
	if err != nil {
		return nil, fmt.Errorf("failed to build get field spec: %w", err)
	}
	moduleID, err := NewUserMod(obj.Module).ResultCallModule(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve collection module identity: %w", err)
	}
	getSpec.Module = moduleID
	getSpec.GetDynamicInput = getModFun.DynamicInputsForCall
	getSpec.ImplicitInputs = append(getSpec.ImplicitInputs, getModFun.cacheImplicitInputs()...)
	getSpec.Directives = append(getSpec.Directives, &ast.Directive{Name: "get"})

	fields := []dagql.Field[*ModuleObject]{
		obj.collectionKeysField(keysField, moduleID),
		obj.collectionListField(getModFun, dag, moduleID),
		{
			Spec: &getSpec,
			Func: func(
				ctx context.Context,
				self dagql.ObjectResult[*ModuleObject],
				args map[string]dagql.Input,
				view call.View,
			) (dagql.AnyResult, error) {
				keyInput, ok := args[collectionKeyArgName]
				if !ok {
					return nil, fmt.Errorf(
						"missing collection key argument %q",
						collectionKeyArgName,
					)
				}
				typedKey, ok := keyInput.(dagql.Typed)
				if !ok {
					return nil, fmt.Errorf("unexpected key input type %T", keyInput)
				}
				rawKey, err := keyModType.ConvertToSDKInput(ctx, typedKey)
				if err != nil {
					return nil, err
				}
				if err := self.Self().validateCollectionKey(rawKey); err != nil {
					return nil, err
				}
				return self.Self().callCollectionGet(ctx, self, getModFun, dag, keyInput)
			},
		},
		obj.collectionSubsetField(
			keyModType,
			keysField.TypeDef.Self().AsList.Value.Self().ElementTypeDef,
			moduleID,
		),
	}

	if batchTypeDef, err := projector.projectCollectionBatchTypeDef(backingTypeDef); err != nil {
		return nil, err
	} else if batchTypeDef != nil {
		fields = append(fields, obj.collectionBatchField(objectTypeDef(batchTypeDef), moduleID))
	}
	return fields, nil
}

func (obj *ModuleObject) collectionKeysField(
	keysField *FieldTypeDef,
	moduleID *dagql.ResultCallModule,
) dagql.Field[*ModuleObject] {
	spec := &dagql.FieldSpec{
		Name:             collectionKeysFieldName,
		Description:      formatGqlDescription(keysField.Description),
		Type:             keysField.TypeDef.Self().ToTyped(),
		Module:           moduleID,
		DeprecatedReason: keysField.Deprecated,
		Trivial:          true,
	}
	spec.Directives = append(
		spec.Directives,
		&ast.Directive{Name: trivialFieldDirectiveName},
		&ast.Directive{Name: "keys"},
	)
	if keysField.SourceMap.Valid {
		spec.Directives = append(spec.Directives, keysField.SourceMap.Value.Self().TypeDirective())
	}

	return dagql.Field[*ModuleObject]{
		Spec: spec,
		Func: func(
			ctx context.Context,
			self dagql.ObjectResult[*ModuleObject],
			_ map[string]dagql.Input,
			view call.View,
		) (dagql.AnyResult, error) {
			if _, err := self.Self().collectionKeys(); err != nil {
				return nil, err
			}
			modType, ok, err := NewUserMod(obj.Module).ModTypeFor(ctx, keysField.TypeDef.Self(), true)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf(
					"could not resolve keys field type %s",
					keysField.TypeDef.Self().ToType(),
				)
			}
			return modType.ConvertFromSDKResult(
				ctx,
				self.Self().Fields[keysField.OriginalName],
			)
		},
	}
}

func (obj *ModuleObject) collectionListField(
	getModFun *ModuleFunction,
	dag *dagql.Server,
	moduleID *dagql.ResultCallModule,
) dagql.Field[*ModuleObject] {
	spec := &dagql.FieldSpec{
		Name:        collectionListFieldName,
		Description: "Items in the current subset, in the same order as `keys`.",
		Type:        dagql.DynamicArrayOutput{Elem: obj.Collection.ValueType.ToTyped()},
		Module:      moduleID,
		Trivial:     true,
	}
	spec.Directives = append(
		spec.Directives,
		&ast.Directive{Name: trivialFieldDirectiveName},
	)

	return dagql.Field[*ModuleObject]{
		Spec: spec,
		Func: func(
			ctx context.Context,
			self dagql.ObjectResult[*ModuleObject],
			_ map[string]dagql.Input,
			view call.View,
		) (dagql.AnyResult, error) {
			keys, err := self.Self().collectionKeys()
			if err != nil {
				return nil, err
			}
			result := dagql.DynamicResultArrayOutput{
				Elem:   obj.Collection.ValueType.ToTyped(),
				Values: make([]dagql.AnyResult, 0, len(keys)),
			}
			for i, key := range keys {
				keyInput, err := self.Self().collectionKeyInput(key)
				if err != nil {
					return nil, err
				}
				itemCtx := ctx
				if currentCall := dagql.CurrentCall(ctx); currentCall != nil {
					itemCall := cloneResultCall(currentCall)
					itemCall.Nth = int64(i + 1)
					if itemCall.Type != nil {
						itemCall.Type = itemCall.Type.Elem
					}
					itemCtx = dagql.ContextWithCall(ctx, itemCall)
				}
				item, err := self.Self().callCollectionGet(
					itemCtx,
					self,
					getModFun,
					dag,
					keyInput,
				)
				if err != nil {
					return nil, err
				}
				result.Values = append(result.Values, item)
			}
			return dagql.NewResultForCurrentCall(ctx, result)
		},
	}
}

func (obj *ModuleObject) collectionSubsetField(
	keyModType ModType,
	keyTypeDef dagql.ObjectResult[*TypeDef],
	moduleID *dagql.ResultCallModule,
) dagql.Field[*ModuleObject] {
	spec := &dagql.FieldSpec{
		Name:        collectionSubsetName,
		Description: "Restrict the collection to an exact subset of keys.",
		Type:        obj,
		Module:      moduleID,
		Args: dagql.NewInputSpecs(dagql.InputSpec{
			Name:        collectionKeysFieldName,
			Description: "Keys to retain from the current subset.",
			Type: dagql.DynamicArrayInput{
				Elem: obj.Collection.KeyType.ToInput(),
			},
		}),
	}
	listKeyModType := &ListType{
		Elem:       keyTypeDef,
		Underlying: keyModType,
	}

	return dagql.Field[*ModuleObject]{
		Spec: spec,
		Func: func(
			ctx context.Context,
			self dagql.ObjectResult[*ModuleObject],
			args map[string]dagql.Input,
			view call.View,
		) (dagql.AnyResult, error) {
			keysInput, ok := args[collectionKeysFieldName]
			if !ok {
				return nil, fmt.Errorf("missing subset keys")
			}
			typedKeys, ok := keysInput.(dagql.Typed)
			if !ok {
				return nil, fmt.Errorf("unexpected subset keys input type %T", keysInput)
			}
			rawSubset, err := listKeyModType.ConvertToSDKInput(ctx, typedKeys)
			if err != nil {
				return nil, err
			}
			subsetKeys, err := collectionSliceValues(rawSubset)
			if err != nil {
				return nil, err
			}
			orderedKeys, err := self.Self().collectionSubsetKeys(subsetKeys)
			if err != nil {
				return nil, err
			}

			fields := maps.Clone(self.Self().Fields)
			keysField, ok := self.Self().TypeDef.FieldByName(
				self.Self().Collection.KeysFieldName,
			)
			if !ok {
				return nil, fmt.Errorf(
					"collection keys field %q not found on %q",
					self.Self().Collection.KeysFieldName,
					self.Self().TypeDef.Name,
				)
			}
			fields[keysField.OriginalName] = orderedKeys
			return dagql.NewResultForCurrentCall(ctx, &ModuleObject{
				Module:     self.Self().Module,
				TypeDef:    self.Self().TypeDef,
				Collection: self.Self().Collection,
				Fields:     fields,
			})
		},
	}
}

func (obj *ModuleObject) collectionBatchField(
	batchTypeDef *ObjectTypeDef,
	moduleID *dagql.ResultCallModule,
) dagql.Field[*ModuleObject] {
	spec := &dagql.FieldSpec{
		Name:        collectionBatchFieldName,
		Description: "Type-specific efficient operations over the current subset.",
		Type:        &CollectionBatchObject{TypeDef: batchTypeDef},
		Module:      moduleID,
		Trivial:     true,
	}
	spec.Directives = append(
		spec.Directives,
		&ast.Directive{Name: trivialFieldDirectiveName},
	)

	return dagql.Field[*ModuleObject]{
		Spec: spec,
		Func: func(
			ctx context.Context,
			self dagql.ObjectResult[*ModuleObject],
			_ map[string]dagql.Input,
			view call.View,
		) (dagql.AnyResult, error) {
			return dagql.NewResultForCurrentCall(ctx, &CollectionBatchObject{
				Module:         self.Self().Module,
				TypeDef:        batchTypeDef,
				BackingTypeDef: self.Self().TypeDef,
				Collection:     self.Self().Collection,
				Fields:         maps.Clone(self.Self().Fields),
			})
		},
	}
}

func (obj *ModuleObject) collectionKeys() ([]any, error) {
	keysField, ok := obj.TypeDef.FieldByName(obj.Collection.KeysFieldName)
	if !ok {
		return nil, fmt.Errorf(
			"collection keys field %q not found on %q",
			obj.Collection.KeysFieldName,
			obj.TypeDef.Name,
		)
	}
	keys, err := collectionSliceValues(obj.Fields[keysField.OriginalName])
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keyID, err := collectionKeyID(key)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[keyID]; exists {
			return nil, fmt.Errorf(
				"collection %q contains duplicate key %s",
				obj.TypeDef.Name,
				keyID,
			)
		}
		seen[keyID] = struct{}{}
	}
	return keys, nil
}

func (obj *ModuleObject) collectionKeyInput(rawKey any) (dagql.Input, error) {
	return obj.Collection.KeyType.ToInput().Decoder().DecodeInput(rawKey)
}

func (obj *ModuleObject) validateCollectionKey(rawKey any) error {
	currentKeys, err := obj.collectionKeys()
	if err != nil {
		return err
	}
	keyID, err := collectionKeyID(rawKey)
	if err != nil {
		return err
	}
	for _, currentKey := range currentKeys {
		currentKeyID, err := collectionKeyID(currentKey)
		if err != nil {
			return err
		}
		if currentKeyID == keyID {
			return nil
		}
	}
	return fmt.Errorf(
		"collection %q does not contain key %s in the current subset",
		obj.TypeDef.Name,
		keyID,
	)
}

func (obj *ModuleObject) collectionSubsetKeys(subsetKeys []any) ([]any, error) {
	currentKeys, err := obj.collectionKeys()
	if err != nil {
		return nil, err
	}

	selected := make(map[string]struct{}, len(subsetKeys))
	for _, key := range subsetKeys {
		keyID, err := collectionKeyID(key)
		if err != nil {
			return nil, err
		}
		if _, exists := selected[keyID]; exists {
			return nil, fmt.Errorf(
				"collection %q subset contains duplicate key %s",
				obj.TypeDef.Name,
				keyID,
			)
		}
		selected[keyID] = struct{}{}
	}

	ordered := make([]any, 0, len(subsetKeys))
	found := make(map[string]struct{}, len(subsetKeys))
	for _, key := range currentKeys {
		keyID, err := collectionKeyID(key)
		if err != nil {
			return nil, err
		}
		if _, keep := selected[keyID]; keep {
			ordered = append(ordered, key)
			found[keyID] = struct{}{}
		}
	}

	for keyID := range selected {
		if _, ok := found[keyID]; !ok {
			return nil, fmt.Errorf(
				"collection %q does not contain key %s in the current subset",
				obj.TypeDef.Name,
				keyID,
			)
		}
	}
	return ordered, nil
}

func (obj *ModuleObject) callCollectionGet(
	ctx context.Context,
	parent dagql.AnyResult,
	getModFun *ModuleFunction,
	dag *dagql.Server,
	keyInput dagql.Input,
) (dagql.AnyResult, error) {
	return getModFun.Call(ctx, &CallOpts{
		Inputs: []CallInput{{
			Name:  obj.Collection.GetArgName,
			Value: keyInput,
		}},
		ParentTyped:  parent,
		ParentFields: obj.Fields,
		Server:       dag,
	})
}

func collectionSliceValues(value any) ([]any, error) {
	if value == nil {
		return []any{}, nil
	}
	if values, ok := value.([]any); ok {
		return slices.Clone(values), nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var values []any
	if err := json.Unmarshal(payload, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func collectionKeyID(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func collectionCoordinate(value any) (string, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case fmt.Stringer:
		return value.String(), nil
	case bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return fmt.Sprint(value), nil
	default:
		payload, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		var decoded string
		if err := json.Unmarshal(payload, &decoded); err == nil {
			return decoded, nil
		}
		return string(payload), nil
	}
}
