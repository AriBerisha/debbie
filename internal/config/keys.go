package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
)

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// checkKeys reports a key that appears twice in one JSON object, or that
// doesn't name a field of the Go struct the object decodes into.
// encoding/json keeps the last of duplicate keys and, by default, ignores
// unknown ones; in this file either would be dangerous, because a misspelled
// "tls_pin_sha256" would silently leave an endpoint unpinned. Keys must match
// the field names exactly, without encoding/json's case folding.
func checkKeys(data []byte, t reflect.Type) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return errors.New("the file must contain a JSON object")
	}
	if err := walkObject(dec, t, ""); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF // the object never closed
		}
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = errors.New("unexpected data after the top-level object")
		}
		return err
	}
	return nil
}

// walkValue checks the next value in dec, which decodes into t. A nil t
// means a value without a schema, whose objects are only checked for
// duplicate keys.
func walkValue(dec *json.Decoder, t reflect.Type, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		return walkObject(dec, t, path)
	case json.Delim('['):
		var elem reflect.Type
		if t = deref(t); t != nil && t.Kind() == reflect.Slice && t != rawMessageType {
			elem = t.Elem()
		}
		for i := 0; dec.More(); i++ {
			if err := walkValue(dec, elem, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
		_, err := dec.Token() // ]
		return err
	}
	return nil
}

// walkObject checks the members of an object whose opening brace has been
// read.
func walkObject(dec *json.Decoder, t reflect.Type, path string) error {
	var fields map[string]reflect.Type
	var elem reflect.Type
	if t = deref(t); t != nil && t != rawMessageType {
		switch t.Kind() {
		case reflect.Struct:
			fields = jsonFields(t)
		case reflect.Map:
			elem = t.Elem()
		}
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key := tok.(string)
		keyPath := joinPath(path, key)
		if seen[key] {
			return fmt.Errorf("%s: the key appears more than once", keyPath)
		}
		seen[key] = true
		next := elem
		if fields != nil {
			ft, ok := fields[key]
			if !ok {
				return fmt.Errorf("%s: unknown setting", keyPath)
			}
			next = ft
		}
		if err := walkValue(dec, next, keyPath); err != nil {
			return err
		}
	}
	_, err := dec.Token() // }
	return err
}

func deref(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// jsonFields maps the JSON names of a struct's fields to their types.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		fields[name] = f.Type
	}
	return fields
}

// joinPath extends a dotted path such as providers.home-gpu.model. Keys that
// aren't plain words are quoted.
func joinPath(path, key string) string {
	if strings.ContainsAny(key, ".[]\" ") || key == "" {
		key = strconv.Quote(key)
	}
	if path == "" {
		return key
	}
	return path + "." + key
}
