package store

import (
	"encoding/json"
	"reflect"
	"strings"
)

// Extra holds the keys of a JSON object that the Go structs don't name, so a
// record written by the Python bot (or a newer version) survives a load and
// save without losing anything.
type Extra map[string]json.RawMessage

// unmarshalKeeping decodes data into known (a pointer to a struct with no
// custom UnmarshalJSON) and returns the keys it has no field for.
func unmarshalKeeping(data []byte, known any) (Extra, error) {
	if err := json.Unmarshal(data, known); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	for key := range jsonKeys(reflect.TypeOf(known).Elem()) {
		delete(all, key)
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

// marshalKeeping encodes known (a struct with no custom MarshalJSON) plus the
// extra keys.
func marshalKeeping(known any, extra Extra) ([]byte, error) {
	data, err := json.Marshal(known)
	if err != nil || len(extra) == 0 {
		return data, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	for k, v := range extra {
		if _, ok := all[k]; !ok {
			all[k] = v
		}
	}
	return json.Marshal(all)
}

func jsonKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		keys[name] = true
	}
	return keys
}
