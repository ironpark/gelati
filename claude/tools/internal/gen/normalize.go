package main

import (
	"fmt"
	"strings"
)

// key returns a canonical structural fingerprint of t, ignoring docs. Two
// types with the same key generate the same Go type.
func key(t *Type) string {
	switch t.Kind {
	case KString:
		return "string"
	case KNumber:
		return "number"
	case KBool:
		return "boolean"
	case KUnknown:
		return "unknown"
	case KNull:
		return "null"
	case KLiteral:
		return fmt.Sprintf("lit(%#v)", t.Lit)
	case KRef:
		return "ref(" + t.Ref + ")"
	case KArray:
		return "[" + key(t.Elem) + "]"
	case KTuple:
		parts := make([]string, 0, len(t.Elems)+1)
		for _, e := range t.Elems {
			parts = append(parts, key(e))
		}
		if t.Rest != nil {
			parts = append(parts, "..."+key(t.Rest))
		}
		return "tuple(" + strings.Join(parts, ",") + ")"
	case KUnion:
		parts := make([]string, 0, len(t.Members))
		for _, m := range t.Members {
			parts = append(parts, key(m))
		}
		return "union(" + strings.Join(parts, "|") + ")"
	case KObject:
		var b strings.Builder
		b.WriteString("{")
		for _, p := range t.Props {
			b.WriteString(p.Name)
			if p.Optional {
				b.WriteString("?")
			}
			b.WriteString(":")
			b.WriteString(key(p.Type))
			b.WriteString(";")
		}
		if t.Index != nil {
			b.WriteString("[k]:" + key(t.Index) + ";")
		}
		b.WriteString("}")
		return b.String()
	}
	panic("unknown kind")
}

// normalize rewrites t into the shapes the emitter understands:
//   - unions are flattened and de-duplicated;
//   - homogeneous tuples ([A, A], [A, ...A[]]) become arrays of A, and unions
//     of such arrays (the @minItems/@maxItems expansion) collapse into one.
func normalize(t *Type) *Type {
	switch t.Kind {
	case KObject:
		out := &Type{Kind: KObject}
		for _, p := range t.Props {
			np := *p
			np.Type = normalize(p.Type)
			out.Props = append(out.Props, &np)
		}
		if t.Index != nil {
			out.Index = normalize(t.Index)
		}
		return out
	case KArray:
		return &Type{Kind: KArray, Elem: normalize(t.Elem)}
	case KTuple:
		var elems []*Type
		for _, e := range t.Elems {
			elems = append(elems, normalize(e))
		}
		var rest *Type
		if t.Rest != nil {
			rest = normalize(t.Rest)
		}
		all := elems
		if rest != nil {
			all = append(append([]*Type(nil), elems...), rest)
		}
		if len(all) > 0 {
			k := key(all[0])
			same := true
			for _, e := range all[1:] {
				if key(e) != k {
					same = false
					break
				}
			}
			if same {
				return &Type{Kind: KArray, Elem: all[0]}
			}
		}
		return &Type{Kind: KTuple, Elems: elems, Rest: rest}
	case KUnion:
		var flat []*Type
		seen := map[string]bool{}
		var add func(m *Type)
		add = func(m *Type) {
			m = normalize(m)
			if m.Kind == KUnion {
				for _, mm := range m.Members {
					add(mm)
				}
				return
			}
			k := key(m)
			if seen[k] {
				return
			}
			seen[k] = true
			flat = append(flat, m)
		}
		for _, m := range t.Members {
			add(m)
		}
		if len(flat) == 1 {
			return flat[0]
		}
		return &Type{Kind: KUnion, Members: flat}
	}
	return t
}

// splitNull removes null members from a union, reporting whether any were
// present. Non-union types are returned unchanged.
func splitNull(t *Type) (*Type, bool) {
	if t.Kind == KNull {
		return &Type{Kind: KUnknown}, true
	}
	if t.Kind != KUnion {
		return t, false
	}
	var rest []*Type
	null := false
	for _, m := range t.Members {
		if m.Kind == KNull {
			null = true
			continue
		}
		rest = append(rest, m)
	}
	if !null {
		return t, false
	}
	if len(rest) == 1 {
		return rest[0], true
	}
	return &Type{Kind: KUnion, Members: rest}, true
}

func isStringish(t *Type) bool {
	if t.Kind == KString {
		return true
	}
	if t.Kind == KLiteral {
		_, ok := t.Lit.(string)
		return ok
	}
	return false
}

func isBoolish(t *Type) bool {
	if t.Kind == KBool {
		return true
	}
	if t.Kind == KLiteral {
		_, ok := t.Lit.(bool)
		return ok
	}
	return false
}

func isNumberish(t *Type) bool {
	if t.Kind == KNumber {
		return true
	}
	if t.Kind == KLiteral {
		_, ok := t.Lit.(float64)
		return ok
	}
	return false
}

// jsonKind returns the JSON value kind a (non-union) type serialises to.
func jsonKind(t *Type) string {
	switch {
	case isStringish(t):
		return "String"
	case isBoolish(t):
		return "Bool"
	case isNumberish(t):
		return "Number"
	case t.Kind == KArray || t.Kind == KTuple:
		return "Array"
	case t.Kind == KObject:
		return "Object"
	}
	return "Any"
}

// discriminator finds a property that is required in every member of an
// all-object union and whose string-literal values are disjoint across
// members. It returns the property name and, per member, its values.
func discriminator(members []*Type) (string, [][]string) {
	if len(members) < 2 {
		return "", nil
	}
	for _, m := range members {
		if m.Kind != KObject {
			return "", nil
		}
	}
	for _, cand := range members[0].Props {
		seen := map[string]bool{}
		var vals [][]string
		ok := true
		for _, m := range members {
			var p *Prop
			for _, q := range m.Props {
				if q.Name == cand.Name {
					p = q
				}
			}
			if p == nil || p.Optional {
				ok = false
				break
			}
			lits := stringLiterals(p.Type)
			if lits == nil {
				ok = false
				break
			}
			for _, l := range lits {
				if seen[l] {
					ok = false
				}
				seen[l] = true
			}
			vals = append(vals, lits)
		}
		if ok {
			return cand.Name, vals
		}
	}
	return "", nil
}

// stringLiterals returns the values of a string literal or a union of string
// literals, or nil otherwise.
func stringLiterals(t *Type) []string {
	if t.Kind == KLiteral {
		if s, ok := t.Lit.(string); ok {
			return []string{s}
		}
		return nil
	}
	if t.Kind != KUnion {
		return nil
	}
	var out []string
	for _, m := range t.Members {
		if m.Kind != KLiteral {
			return nil
		}
		s, ok := m.Lit.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// mergeObjects folds an untagged union of object types into a single object
// whose properties are the union of all members' properties. A property is
// required only if every member requires it; conflicting property types become
// a (normalised) union of the alternatives.
func mergeObjects(members []*Type) *Type {
	type acc struct {
		prop     *Prop
		types    []*Type
		required int
	}
	var order []string
	byName := map[string]*acc{}
	out := &Type{Kind: KObject}
	for _, m := range members {
		for _, p := range m.Props {
			a := byName[p.Name]
			if a == nil {
				a = &acc{prop: p}
				byName[p.Name] = a
				order = append(order, p.Name)
			}
			if a.prop.Doc == "" {
				a.prop = &Prop{Name: p.Name, Doc: p.Doc}
			}
			a.types = append(a.types, p.Type)
			if !p.Optional {
				a.required++
			}
		}
		if m.Index != nil {
			out.Index = m.Index
		}
	}
	for _, name := range order {
		a := byName[name]
		var t *Type
		if len(a.types) == 1 {
			t = a.types[0]
		} else {
			t = normalize(&Type{Kind: KUnion, Members: a.types})
		}
		out.Props = append(out.Props, &Prop{
			Name:     name,
			Doc:      a.prop.Doc,
			Optional: a.required != len(members),
			Type:     t,
		})
	}
	return out
}
