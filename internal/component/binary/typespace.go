package binary

import "fmt"

// This file implements the component's full TYPE index space, as distinct
// from Types (which only holds type-section deftypes -- see the Types field
// doc). Per the component-model binary format, indices in the "type" sort
// are assigned, in declaration order, to every type-index-producing
// definition across every section: type-section deftypes (section 7),
// type-sort aliases (section 6, sort == 0x03), and imports whose externdesc
// names a type (section 10, ExternType == 0x03). Canon TypeIdx and
// export/instance type references index this full space, not Types alone.
//
// A component export whose sort is type (section 11, ExternType 0x03) also
// introduces a new type index -- an alias of whatever it exports -- per the
// spec ("export introduces an alias"). It is tracked as a TypeSpaceExport
// contributor, resolving through to the exported sortidx's type index; a WIT
// component that re-exports its named types (rec-t, var-t, ...) interleaves
// these exports with type-section deftypes, shifting every later type index,
// so omitting them misresolves the first func type declared after one.
//
// TypeSpace is built incrementally by decodeComponent as it walks sections in
// file order (see the case 6/7/10 branches), so it is correct even when
// sections of different ids interleave or repeat.

// TypeSpaceEntryKind distinguishes what produced a component type-index-space
// entry.
type TypeSpaceEntryKind byte

const (
	// TypeSpaceDef marks an entry produced by a type-section (id 7) deftype.
	TypeSpaceDef TypeSpaceEntryKind = iota
	// TypeSpaceAlias marks an entry produced by a type-sort alias (id 6,
	// Sort == 0x03).
	TypeSpaceAlias
	// TypeSpaceImport marks an entry produced by an import whose externdesc
	// names a type (id 10, ExternType == 0x03).
	TypeSpaceImport
	// TypeSpaceExport marks an entry produced by an export whose sort is type
	// (id 11, ExternType == 0x03) -- "export introduces an alias" of the
	// exported type.
	TypeSpaceExport
)

// TypeSpaceEntry is one entry in the component's full type index space.
// Exactly one of Def, Alias, Import is meaningful, selected by Kind.
type TypeSpaceEntry struct {
	Kind TypeSpaceEntryKind

	// Def is the index into Types, valid when Kind == TypeSpaceDef.
	Def uint32

	// Alias is the index into Aliases, valid when Kind == TypeSpaceAlias.
	Alias uint32

	// Import is the index into Imports, valid when Kind == TypeSpaceImport.
	Import uint32

	// Export is the index into Exports, valid when Kind == TypeSpaceExport.
	Export uint32
}

// maxTypeAliasDepth bounds alias-chain following in ResolveType so a
// malformed or cyclic alias chain fails loud instead of looping forever.
const maxTypeAliasDepth = 32

// extraTypeBase marks the start of a private, decode-independent index range
// backed by Component.extraTypes rather than TypeSpace/Types -- see that
// field's doc for why (globalizing a type discovered inside an imported
// instance's declared instancetype, for `use iface.{T}`, without disturbing
// any real file-declared type index). Comfortably above any realistic
// component's real type count: a LEB128 vec count large enough to reach it
// would need gigabytes of encoding just for the count's own declarations.
const extraTypeBase = 1 << 30

// internExtraType appends d to c.extraTypes and returns its escape-range
// index (see extraTypeBase).
func (c *Component) internExtraType(d TypeDesc) uint32 {
	c.extraTypes = append(c.extraTypes, d)
	return extraTypeBase + uint32(len(c.extraTypes)-1)
}

// ResolveType resolves a component type index -- which may name a
// type-section deftype directly, or (transitively) a type-sort alias -- to
// its underlying TypeDesc, walking the component's full TypeSpace rather
// than indexing Types directly (which only covers type-section entries and
// misresolves the moment any alias or type import precedes a deftype).
//
// It follows alias chains where this decoder has enough structure to do so:
// an "export" alias (TargetKind 0x00) naming a type-sort entry inline-exported
// by one of this component's own instances (Instances, section 5, Kind
// 0x01), and an "outer" alias (TargetKind 0x02) -- both the self-referential
// form (OuterCount 0, another index in this same TypeSpace) and a genuine
// de Bruijn reference into an ENCLOSING component's type index space, which
// walks Component.Outer OuterCount times. Anything else this decoder
// cannot follow structurally -- most notably a type exported from an
// *imported* instance (the common real-guest shape, e.g. `alias export
// $streams "output-stream" (type $ot)`; this package does not decode nested
// type declarations inside an imported instance type) -- fails loud with an error
// naming the index and the reason, rather than returning a zero value or
// panicking. That is the correct outcome for such an index: as documented
// on OwnDesc/BorrowDesc, own/borrow handle types only ever use their
// ResourceType index as an opaque tag (never dereferenced through a
// resolver), so a real guest built this way still instantiates and runs --
// see internal/component/instance's package doc.
//
// For a Component that was not produced by Decode (TypeSpace left empty --
// the common shape for hand-built binary.Component values in tests), idx is
// resolved directly against Types instead, matching this method's behavior
// before TypeSpace existed.
func (c *Component) ResolveType(idx uint32) (TypeDesc, error) {
	return c.resolveTypeDepth(idx, 0)
}

// ResourceDefIndex maps a resource type index to the canonical index where the
// resource is DEFINED (or the root import it names), following the same
// TypeSpace links ResolveType does: export-introduces-alias, self "outer" type
// alias, and an `eq`-bounded type import. A resource is commonly named by
// several indices in one component -- the deftype that defines it AND the export
// alias created when it is exported -- while canon resource.new/drop/rep use the
// deftype and own<R>/borrow<R> in a lifted signature use the export alias.
// Reducing every alias of one resource to this single index gives a stable
// per-component identity. Returns idx unchanged for anything it cannot resolve
// further (a plain import, an opaque alias, or a hand-built Component with no
// TypeSpace) -- which is the right stable tag for those.
func (c *Component) ResourceDefIndex(idx uint32) uint32 {
	return c.resourceDefIndexDepth(idx, 0)
}

func (c *Component) resourceDefIndexDepth(idx uint32, depth int) uint32 {
	if depth > maxTypeAliasDepth || int(idx) >= len(c.TypeSpace) {
		return idx
	}
	switch entry := c.TypeSpace[idx]; entry.Kind {
	case TypeSpaceExport:
		if int(entry.Export) < len(c.Exports) {
			return c.resourceDefIndexDepth(c.Exports[entry.Export].ExternIndex, depth+1)
		}
	case TypeSpaceImport:
		if int(entry.Import) < len(c.Imports) {
			if im := c.Imports[entry.Import]; im.TypeEqBound {
				return c.resourceDefIndexDepth(im.TypeEqIndex, depth+1)
			}
		}
	case TypeSpaceAlias:
		if int(entry.Alias) < len(c.Aliases) {
			// A self "outer" type alias (OuterCount 0) names another index in
			// this same TypeSpace -- follow it, as resolveTypeDepth does.
			if al := c.Aliases[entry.Alias]; al.Sort == 0x03 && al.TargetKind == 0x02 && al.OuterCount == 0 {
				return c.resourceDefIndexDepth(al.OuterIndex, depth+1)
			}
		}
	}
	return idx
}

func (c *Component) resolveTypeDepth(idx uint32, depth int) (TypeDesc, error) {
	if depth > maxTypeAliasDepth {
		return nil, fmt.Errorf("type index %d: alias chain exceeds depth %d (cycle?)", idx, maxTypeAliasDepth)
	}

	if idx >= extraTypeBase {
		i := idx - extraTypeBase
		if int(i) >= len(c.extraTypes) {
			return nil, fmt.Errorf("type index %d: out of range of the %d-entry extra type table", idx, len(c.extraTypes))
		}
		return c.extraTypes[i], nil
	}

	if len(c.TypeSpace) == 0 {
		// No TypeSpace: either a component with no type-index-producing
		// definitions at all, or (far more commonly in this codebase's
		// tests) a hand-built Component that never went through Decode.
		// Preserve pre-TypeSpace behavior: index Types directly.
		if int(idx) >= len(c.Types) {
			return nil, fmt.Errorf("type index %d out of range of %d types", idx, len(c.Types))
		}
		return c.Types[idx].Descriptor, nil
	}

	if int(idx) >= len(c.TypeSpace) {
		return nil, fmt.Errorf("type index %d out of range of the %d-entry component type index space", idx, len(c.TypeSpace))
	}

	entry := c.TypeSpace[idx]
	switch entry.Kind {
	case TypeSpaceDef:
		if int(entry.Def) >= len(c.Types) {
			return nil, fmt.Errorf("type index %d: internal error: deftype index %d out of range of %d types", idx, entry.Def, len(c.Types))
		}
		return c.Types[entry.Def].Descriptor, nil

	case TypeSpaceImport:
		name := "?"
		if int(entry.Import) < len(c.Imports) {
			im := c.Imports[entry.Import]
			name = im.Name
			// A type import with an `eq N` bound is declared equal to the
			// component type N (wit-component/cargo-component re-import a
			// world's exported types this way), so resolve through to it rather
			// than treating it as opaque.
			if im.TypeEqBound {
				return c.resolveTypeDepth(im.TypeEqIndex, depth+1)
			}
		}
		return nil, fmt.Errorf("type index %d names an imported type (import %q); its structural definition is not decoded by this milestone", idx, name)

	case TypeSpaceAlias:
		if int(entry.Alias) >= len(c.Aliases) {
			return nil, fmt.Errorf("type index %d: internal error: alias index %d out of range of %d aliases", idx, entry.Alias, len(c.Aliases))
		}
		return c.resolveAlias(c.Aliases[entry.Alias], idx, depth)

	case TypeSpaceExport:
		// An "export introduces an alias" entry: it aliases the type named by
		// the export's sortidx, which is itself an index into this same
		// TypeSpace. Resolve through to it.
		if int(entry.Export) >= len(c.Exports) {
			return nil, fmt.Errorf("type index %d: internal error: export index %d out of range of %d exports", idx, entry.Export, len(c.Exports))
		}
		return c.resolveTypeDepth(c.Exports[entry.Export].ExternIndex, depth+1)

	default:
		return nil, fmt.Errorf("type index %d: unknown type-space entry kind %d", idx, entry.Kind)
	}
}

// resolveAlias follows one type-sort AliasDef's target to an underlying
// TypeDesc where this decoder has enough structure to do so. idx and depth
// are the original index and current alias-chain depth, threaded through
// purely for error messages and the cycle guard.
func (c *Component) resolveAlias(al AliasDef, idx uint32, depth int) (TypeDesc, error) {
	switch al.TargetKind {
	case 0x00: // export
		// This component's own inline-export instances (Instances, section
		// 5, Kind == 0x01): its Exports list directly names a sortidx, so a
		// type-sort export within it is just another index into this same
		// TypeSpace. al.InstanceIdx here is treated as a raw index into
		// Instances (section-5-only), not the full component instance index
		// space (see componentinstancespace.go) -- kept as-is (rather than
		// routed through ComponentInstanceSpace like the import branch
		// below) so a hand-built Component with no ComponentInstanceSpace
		// still resolves this the way it always has.
		if int(al.InstanceIdx) < len(c.Instances) {
			inst := c.Instances[al.InstanceIdx]
			if inst.Kind == 0x01 {
				for _, ie := range inst.Exports {
					if ie.Name == al.Name && ie.Sort == 0x03 {
						return c.resolveTypeDepth(ie.SortIdx, depth+1)
					}
				}
			}
		}
		// Otherwise, al.InstanceIdx may instead be a component instance-sort
		// index (ComponentInstanceSpace) naming an IMPORTED instance -- the
		// common real-guest shape for `use iface.{T}`. Unlike a locally
		// inline-exported instance, an import's exports aren't visible via
		// Instances at all; its declared instancetype's own Exports (built
		// structurally by readInstancetypeDesc -- see InstanceDesc.Exports'
		// doc) answers this directly instead.
		if int(al.InstanceIdx) < len(c.ComponentInstanceSpace) {
			if e := c.ComponentInstanceSpace[al.InstanceIdx]; e.Kind == ComponentInstanceFromImport && int(e.Import) < len(c.Imports) {
				if im := c.Imports[e.Import]; im.ExternType == 0x05 {
					if td, err := c.resolveTypeDepth(im.ExternIndex, depth+1); err == nil {
						if instDesc, ok := td.(InstanceDesc); ok {
							if exp, ok := instDesc.Exports[al.Name]; ok {
								// exp is a TypeRef into instDesc.Types (this
								// instancetype's own local type-sort index
								// space, not this Component's) -- globalize it
								// before returning, so any TypeRef nested
								// inside the result is resolvable through c's
								// ordinary Resolver like any other (see
								// InstanceDesc's doc and globalizeLocalRef).
								global, gerr := c.globalizeLocalRef(instDesc.Types, exp, map[uint32]uint32{})
								if gerr != nil {
									return nil, fmt.Errorf("type index %d: alias exports %q from instance %d: %w", idx, al.Name, al.InstanceIdx, gerr)
								}
								if global.Primitive != "" {
									return PrimitiveDesc{Prim: global.Primitive}, nil
								}
								return c.resolveTypeDepth(*global.TypeIndex, depth+1)
							}
						}
					}
				}
			}
		}
		return nil, fmt.Errorf("type index %d: alias exports %q from instance %d, which this decoder cannot resolve structurally (an imported instance whose export type this decoder could not resolve -- e.g. an abstract resource, or a further alias -- or a locally-instantiated instance whose nested type declarations are not decoded)", idx, al.Name, al.InstanceIdx)

	case 0x02: // outer
		// The de Bruijn count is the number of enclosing components to skip:
		// 0 is this same component (Explainer.md permits it), 1 the component
		// this one is nested in, and so on. Walk Component.Outer that many
		// times and resolve OuterIndex in the component we land on -- which
		// is a plain index in ITS type index space, not this one's.
		//
		// A nested component built by wit-component routinely does this to
		// re-export a type its parent defines (fused.22.wast's `$c1` aliases
		// all six of the root's flags types), so failing here made those
		// components uninstantiable.
		outer := c
		for i := uint32(0); i < al.OuterCount; i++ {
			if outer.Outer == nil {
				return nil, fmt.Errorf("type index %d: outer alias (count=%d) targets %d enclosing component(s), but only %d are known (this component was decoded standalone, or is hand-built)", idx, al.OuterCount, al.OuterCount, i)
			}
			outer = outer.Outer
		}
		return outer.resolveTypeDepth(al.OuterIndex, depth+1)

	default:
		// TargetKind 0x01 (core export) cannot legally carry a type-sort
		// alias (core exports are func/table/memory/global/tag, never
		// type), but decodeAliasSection does not itself reject the
		// combination, so fail loud here rather than mis-index.
		return nil, fmt.Errorf("type index %d: alias target kind %#x cannot resolve to a type", idx, al.TargetKind)
	}
}

// globalizeLocalRef rewrites one TypeRef found inside an imported instance's
// local type graph (localTypes -- an InstanceDesc's own Types, per its doc)
// so it resolves through c's ordinary Resolver: unchanged if it's a
// primitive, or remapped to a fresh Component.extraTypes index (recursively
// globalizing, and interning, the local type it names) if it names a local
// index. memo caches localIdx -> already-assigned extraTypes index, both to
// avoid re-interning a local type reachable more than once and to break a
// genuine cycle (e.g. a locally-declared resource referencing its own
// handle) by returning the placeholder reserved for an in-progress index
// instead of recursing forever.
func (c *Component) globalizeLocalRef(localTypes []TypeDesc, ref TypeRef, memo map[uint32]uint32) (TypeRef, error) {
	if ref.Primitive != "" || ref.TypeIndex == nil {
		return ref, nil
	}
	localIdx := *ref.TypeIndex
	if g, ok := memo[localIdx]; ok {
		return TypeRef{TypeIndex: &g}, nil
	}
	if int(localIdx) >= len(localTypes) || localTypes[localIdx] == nil {
		return TypeRef{}, fmt.Errorf("local type index %d is out of range, or was not resolved structurally by this decoder (an abstract resource export, or a type-sort alias)", localIdx)
	}
	// Reserve (and memoize) the escape slot before recursing, so a
	// self-referential local type terminates instead of looping forever.
	global := c.internExtraType(nil)
	memo[localIdx] = global
	rewritten, err := c.globalizeLocalTypeDesc(localTypes, localTypes[localIdx], memo)
	if err != nil {
		return TypeRef{}, err
	}
	c.extraTypes[global-extraTypeBase] = rewritten
	return TypeRef{TypeIndex: &global}, nil
}

// globalizeLocalTypeDesc rewrites every TypeRef nested directly inside d (a
// TypeDesc from an imported instance's local type declarations) via
// globalizeLocalRef, returning an equivalent TypeDesc whose own TypeRefs are
// all either primitives or Component.extraTypes indices -- safe to embed in a
// TypeDesc handed to an ordinary caller of this Component's Resolver.
func (c *Component) globalizeLocalTypeDesc(localTypes []TypeDesc, d TypeDesc, memo map[uint32]uint32) (TypeDesc, error) {
	switch t := d.(type) {
	case PrimitiveDesc, FlagsDesc, EnumDesc:
		return d, nil

	case ListDesc:
		elem, err := c.globalizeLocalRef(localTypes, t.Element, memo)
		if err != nil {
			return nil, err
		}
		return ListDesc{Element: elem}, nil

	case MapDesc:
		key, err := c.globalizeLocalRef(localTypes, t.Key, memo)
		if err != nil {
			return nil, err
		}
		val, err := c.globalizeLocalRef(localTypes, t.Value, memo)
		if err != nil {
			return nil, err
		}
		return MapDesc{Key: key, Value: val}, nil

	case RecordDesc:
		fields := make([]RecordField, len(t.Fields))
		for i, f := range t.Fields {
			ft, err := c.globalizeLocalRef(localTypes, f.Type, memo)
			if err != nil {
				return nil, err
			}
			fields[i] = RecordField{Name: f.Name, Type: ft}
		}
		return RecordDesc{Fields: fields}, nil

	case TupleDesc:
		elems := make([]TypeRef, len(t.Elements))
		for i, e := range t.Elements {
			er, err := c.globalizeLocalRef(localTypes, e, memo)
			if err != nil {
				return nil, err
			}
			elems[i] = er
		}
		return TupleDesc{Elements: elems}, nil

	case VariantDesc:
		cases := make([]VariantCase, len(t.Cases))
		for i, cs := range t.Cases {
			nc := VariantCase{Name: cs.Name}
			if cs.Type != nil {
				ct, err := c.globalizeLocalRef(localTypes, *cs.Type, memo)
				if err != nil {
					return nil, err
				}
				nc.Type = &ct
			}
			cases[i] = nc
		}
		return VariantDesc{Cases: cases}, nil

	case OptionDesc:
		elem, err := c.globalizeLocalRef(localTypes, t.Element, memo)
		if err != nil {
			return nil, err
		}
		return OptionDesc{Element: elem}, nil

	case ResultDesc:
		nr := ResultDesc{}
		if t.Ok != nil {
			ok, err := c.globalizeLocalRef(localTypes, *t.Ok, memo)
			if err != nil {
				return nil, err
			}
			nr.Ok = &ok
		}
		if t.Err != nil {
			er, err := c.globalizeLocalRef(localTypes, *t.Err, memo)
			if err != nil {
				return nil, err
			}
			nr.Err = &er
		}
		return nr, nil

	case StreamDesc:
		ns := StreamDesc{}
		if t.Element != nil {
			el, err := c.globalizeLocalRef(localTypes, *t.Element, memo)
			if err != nil {
				return nil, err
			}
			ns.Element = &el
		}
		return ns, nil

	case FutureDesc:
		nf := FutureDesc{}
		if t.Element != nil {
			el, err := c.globalizeLocalRef(localTypes, *t.Element, memo)
			if err != nil {
				return nil, err
			}
			nf.Element = &el
		}
		return nf, nil

	case OwnDesc, BorrowDesc:
		// ResourceType is an opaque tag, never dereferenced through a
		// Resolver (see OwnDesc/BorrowDesc's doc) -- left as its original
		// local index even though it is technically still one, since nothing
		// ever looks it up.
		return d, nil

	default:
		return nil, fmt.Errorf("cannot resolve a locally-declared %s type from an imported instance's type declarations", d.Kind())
	}
}
