package jsgo

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
)

var structCache sync.Map // Stores reflect.Type -> fieldInfo

type fieldInfo struct {
	normalFields         map[string]*field
	embeddedJsValueField *field // Could be the base also
	baseClass            string
}

type field struct {
	index      []int // path
	isEmbedded bool
	_type      reflect.Type
	tag        fieldTag
}

type fieldTag struct {
	name      string
	ignore    bool
	extend    string
	omitempty bool
}

// getFieldTag parses jsgo struct tags with extended syntax:
// `jsgo:"[name][,omitempty][,extends=ParentClass]"`
// Examples:
// `jsgo:"-"`                          - ignore field
// `jsgo:"fieldName"`                  - custom JS property name
// `jsgo:",omitmempty"`
// `jsgo:",extends=BaseClass"`         - inheritance specification
// `jsgo:"_parent,extends=BaseClass"`  - named inheritance
// NOTE: Json uses "-" but "_" is what we use in go. We support both
// If no name is found for an embedded field, don't implicitly name it
func getFieldTag(t *reflect.StructField) fieldTag {
	var f fieldTag
	if !t.Anonymous {
		f.name = t.Name
	}

	// Get raw tag value
	tagVal, ok := t.Tag.Lookup("jsgo")
	if !ok {
		return f
	}

	// Split into components
	parts := strings.Split(tagVal, ",")
	if len(parts) == 0 {
		return f
	}

	// First part is always name (optional)
	if parts[0] != "" {
		f.name = strings.TrimSpace(parts[0])
	}
	// Process remaining options
	for _, part := range parts[1:] {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "extends="):
			f.extend = strings.TrimPrefix(part, "extends=")
		case part == "_" || part == "-":
			f.ignore = true
		case part == "omitempty": // consider using this for both marshal and unmarshal
			// but it's painful as it has to be added to almost all fields
			f.omitempty = true
		}
	}

	// Special case: name "_" means ignore
	if f.name == "_" || f.name == "-" {
		f.ignore = true
	}

	return f
}

// getCachedFields retrieves or calculates field information for a type
// BUG: Double computation(expansion) of visibleFields(t) during race
func getCachedFields(t reflect.Type) *fieldInfo {
	// Check cache
	if cached, ok := structCache.Load(t); ok {
		return cached.(*fieldInfo)
	}

	// Update cache
	fields, _ := structCache.LoadOrStore(t, visibleFields(t))
	return fields.(*fieldInfo)
}

// getExtendsTag returns the JS class to extend if specified in the struct tags
func getExtendsTag(t reflect.Type) string {
	if t.Kind() != reflect.Struct {
		return ""
	}

	return getCachedFields(t).baseClass
}

// Copied from the reflect package and modified to our need

// VisibleFields returns all the visible fields in t, which must be a
// struct type. A field is defined as visible if it's accessible
// directly with a FieldByName call. The returned fields include fields
// inside anonymous struct members and unexported fields. They follow
// the same order found in the struct, with anonymous fields followed
// immediately by their promoted fields.
//
// For each element e of the returned slice, the corresponding field
// can be retrieved from a value v of type t by calling v.FieldByIndex(e.Index).
func visibleFields(t reflect.Type) *fieldInfo {
	// Let's leave the panic msgs...
	if t == nil {
		panic("reflect: VisibleFields(nil)")
	}
	if t.Kind() != reflect.Struct {
		panic("reflect.VisibleFields of non-struct type")
	}
	w := &visibleFieldsWalker{
		fieldInfo: fieldInfo{
			normalFields:         make(map[string]*field, t.NumField()/2),
			embeddedJsValueField: nil,
			baseClass:            "",
		},
		visiting: make(map[reflect.Type]bool),
		index:    make([]int, 0, 2),
	}
	w.walk(t)
	return &w.fieldInfo
}

type visibleFieldsWalker struct {
	fieldInfo
	visiting map[reflect.Type]bool
	index    []int
}

// We need the absolute path
// walk walks all the fields in the struct type t, visiting
// fields in index preorder and appending them to w.fields
// (this maintains the required ordering).
// Fields that have been overridden have their
// Name field cleared.
func (w *visibleFieldsWalker) walk(t reflect.Type) {
	if w.visiting[t] {
		return
	}
	w.visiting[t] = true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			t := f.Type
			if t.Kind() == reflect.Pointer {
				t = t.Elem()
			}
			if !f.IsExported() && t.Kind() != reflect.Struct {
				// Ignore embedded fields of unexported non-struct types.
				continue
			}
			// Do not ignore embedded fields of unexported struct types
			// since they may have exported fields.
		} else if !f.IsExported() {
			// Ignore unexported non-embedded fields.
			continue
		}

		tag := getFieldTag(&f)
		if tag.ignore {
			continue
		}
		w.index = append(w.index, i)
		add := true
		// We have an embedded field that's not named. Json only
		// considers the fields in this and not it particularly.
		// e.g
		//		`{
		//			"G": 3,
		//			"H": 5,
		//			"F": {"G": 6, "H": 10}
		//		}`
		//		type E struct {
		//			F
		//		}
		//
		//		type F struct {
		//			G int
		//			H int
		//		}
		// unmarshaling gives: {{3 5}}
		if f.Anonymous && tag.name == "" {
			add = false
		}
		if old, ok := w.normalFields[tag.name]; ok {
			if len(w.index) == len(old.index) {
				// Fields with the same name at the same depth
				// cancel one another out. Set the field name
				// to empty to signify that has happened, and
				// there's no need to add this field.
				delete(w.normalFields, tag.name)
				add = false
			} else if len(w.index) < len(old.index) {
				// The old field loses because it's deeper than the new one.
				delete(w.normalFields, tag.name)
			} else {
				// The old field wins because it's shallower than the new one.
				add = false
			}
		}
		if add {
			// Copy the index so that it's not overwritten
			// by the other appends.
			index := append([]int(nil), w.index...)
			w.normalFields[tag.name] = &field{
				index:      index,
				isEmbedded: f.Anonymous,
				_type:      f.Type,
				tag:        tag,
			}
		}
		// If the embedded is named, we should treat it as though it isn't
		// embedded.
		if f.Anonymous && tag.name == "" {
			walk := true
			// We shouldn't have stale data here as no other fields
			// can annihilate unnamed fields... they were never in
			if len(w.index) == 1 {
				if w.baseClass == "" && tag.extend != "" {
					w.baseClass = tag.extend
				}

				if f.Type == jsValueType || f.Type == jsValueTypePtr {
					if w.embeddedJsValueField == nil {
						// We won't have more than one
						w.embeddedJsValueField = &field{
							index:      append([]int(nil), w.index...),
							isEmbedded: f.Anonymous,
							_type:      f.Type,
							tag:        tag,
						}
					}
					walk = false
				}
			}
			if walk {
				if f.Type.Kind() == reflect.Pointer {
					f.Type = f.Type.Elem()
				}
				if f.Type.Kind() == reflect.Struct {
					w.walk(f.Type)
				}
			}
		}
		w.index = w.index[:len(w.index)-1]
	}
	delete(w.visiting, t)
}

func getOrFillField(v reflect.Value, index []int) (reflect.Value, error) {
	if len(index) == 1 {
		return v.Field(index[0]), nil
	}
	for _, x := range index {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				// v.Type().Elem().Kind() == reflect.Struct &&
				if !v.CanSet() {
					return reflect.Value{}, fmt.Errorf("cannot set embedded pointer to unexported struct: %v", v.Type().Elem())
				}
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(x)
	}
	return v, nil
}
